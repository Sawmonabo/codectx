package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// readerCeiling is the wall no question-answering process may exceed while
// another process is building a generation.
//
// The reasoning, not a round number: storage.busy_timeout is 5s (the shipped
// default, and what this sandbox runs under). A process that queued behind the
// writer's transaction either exhausts that timeout and fails typed -- which
// the code check below catches -- or gets through after waiting, and then its
// wall is at least the wait it did. A ceiling of 3s is under the smallest
// completed busy wait a contended open could produce and an order of magnitude
// above every read this fixture serves, so it separates "answered" from
// "waited out a writer and then answered" without encoding a host speed.
const readerCeiling = 3 * time.Second

// padPerFile is how many symbols each fixture source file gains before each of
// the two measured index runs. It is the size that makes the window real: with
// the five generated files alone the writer holds its ingestion transaction for
// a few milliseconds and no second process can be inside it, so a green run
// would prove nothing. The mutation named in this test's doc comment is what
// calibrates it -- with the read-only open removed the readers below must fail
// busy, and if they do not, the padding is too small rather than the product
// correct.
const padPerFile = 6000

// readerPause separates one reader cycle from the next. See the call sites.
const readerPause = 50 * time.Millisecond

// TestE2EReadsAnswerDuringALiveIndex is the product-boundary proof that a
// second process answers questions throughout another process's index.
//
// Failure mode it protects: a question-answering command performs a database
// WRITE between process start and its first read -- the schema check inside a
// write transaction, a retention lease on the pinned generation -- and is
// therefore refused CTX_WORKSPACE_BUSY for the whole of a run, so an agent's
// queries fail for as long as the index it is querying takes to refresh.
// Mutation that must fail it: internal/app/compose.go's
// `ReadOnly: o.mode == modeQuery` -> `ReadOnly: false`.
//
// The measurements it reports: each reader's wall, and the indexing child's own
// wall in two arms, one with the readers running throughout and one alone.
func TestE2EReadsAnswerDuringALiveIndex(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// A generation must be published before any reader runs: a read against a
	// never-indexed workspace answers CTX_NO_ACTIVE_GENERATION, which is a
	// phase-ordering mistake in this test and not an answer.
	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the first index failed (exit %d): %+v", code, env.Error)
	}
	seedEnv, seedCode := s.run(t, "search", "TinyStore")
	hits := data[model.Page[model.SearchHit]](t, seedEnv, seedCode)
	if len(hits.Items) == 0 {
		t.Fatal("the published generation holds no hit for TinyStore, so there is no node to walk from")
	}
	seed := hits.Items[0].NodeID

	// The readers, one exec each. `doctor` is the shallow report on purpose:
	// --deep spells the search index's integrity check as an insert, so it
	// needs the writer and is documented as a command that writes.
	readers := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"status --resources", []string{"status", "--resources"}},
		{"search", []string{"search", "TinyStore"}},
		{"symbol", []string{"symbol", tinySeedSymbol}},
		{"refs", []string{"refs", fmt.Sprint(seed)}},
		{"doctor", []string{"doctor"}},
	}

	// Arm one: the index alone. Both arms are handed the same shape of work --
	// the same files, the same symbol count, the same appended byte width --
	// so the difference between the two walls is about the readers and not
	// about what each run had to parse.
	padFixture(t, s.Repo, 1)
	aloneStart := time.Now()
	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the unattended index failed (exit %d): %+v", code, env.Error)
	}
	aloneWall := time.Since(aloneStart)

	// Arm two: the same index with every reader running throughout.
	padFixture(t, s.Repo, 2)
	child := exec.Command(binary, "index", "--repo", s.Repo, "--json")
	child.Env = s.Environ
	child.Stdout, child.Stderr = os.Stderr, os.Stderr
	withStart := time.Now()
	if err := child.Start(); err != nil {
		t.Fatalf("start the indexing child: %v", err)
	}
	var withWall time.Duration
	indexed := make(chan error, 1)
	go func() {
		err := child.Wait()
		withWall = time.Since(withStart)
		indexed <- err
	}()

	// Two sets of walls, because only one of them is evidence: the LAST cycle
	// may have run after the child exited, and a wall measured with no writer
	// in the workspace says nothing about reading during a run. Only the
	// strictly-inside walls carry the ceiling.
	worst := make(map[string]time.Duration, len(readers))
	after := make(map[string]time.Duration, len(readers))
	cycles := 0
	for running := true; running; {
		cycle := make(map[string]time.Duration, len(readers))
		for _, r := range readers {
			cycle[r.name] = s.readDuringIndex(t, r.name, r.args...)
		}
		// A pause between cycles, because the readers are readers and not a
		// load generator: six execs per cycle with no gap saturates the cores
		// this test is capped to and would put CPU contention into both the
		// reader walls and the two index walls, which are measurements about
		// the database.
		time.Sleep(readerPause)
		select {
		case err := <-indexed:
			// withWall is published by the goroutine before it sends, so the
			// receive is what makes it readable here.
			if err != nil {
				t.Fatalf("the indexing child failed while the readers ran: %v", err)
			}
			running = false
			merge(after, cycle)
		default:
			// The child outlived a whole cycle: every reader in it answered
			// strictly inside the run.
			cycles++
			merge(worst, cycle)
		}
	}
	for _, r := range readers {
		t.Logf("reader %-18s worst wall %s inside the run, %s in the cycle that outlived it",
			r.name, worst[r.name], after[r.name])
		if worst[r.name] > readerCeiling {
			t.Errorf("%s took %s during a live index, over the %s ceiling: it waited on the writer",
				r.name, worst[r.name], readerCeiling)
		}
	}
	t.Logf("reader cycles completed strictly inside the run: %d", cycles)
	t.Logf("index wall alone %s, with the readers running throughout %s", aloneWall, withWall)
	if cycles == 0 {
		t.Errorf("the index finished (wall %s) before one full reader cycle completed: "+
			"the readers are being held up by the run", withWall)
	}

	mcpReadsDuringOwnRefresh(t, ctx, s)
}

// TestE2EServerStartsDuringAnotherProcessIndex is the leg that could not run
// while the server took the workspace lock at startup: an external `codectx
// index` is the writer, the server is STARTED DURING it, and it serves the
// whole time.
//
// Failure mode it protects: `mcp serve` takes the cross-process workspace lock
// as part of opening, so a server an agent starts while the person is indexing
// in a terminal is refused CTX_WORKSPACE_BUSY at startup and answers nothing at
// all -- not even the exploration tools, which need neither the lock nor the
// writer. Mutations that must fail it are quoted in this lane's report:
// internal/app/compose.go's `o.locksAtOpen()` -> `o.indexing()` on the lock (the
// connect is refused busy), and `LazyWriter: o.mode == modeServe` -> false (the
// open's schema check queues behind the run and is refused `database is busy`).
//
// What it asserts, and why each one is the product's promise: the session
// CONNECTS during the run; both exploration tools answer throughout it;
// codectx_refresh_index answers the typed busy refusal naming the holder --
// that one call, not the session; and the server is still answering after the
// run has ended. The server is started exactly as an agent starts it, with the
// watch loop at its shipped default: a watch that ended the session on a busy
// workspace would fail here rather than in the field.
// It is a sandbox of its own, not a further leg of the row above: the server
// that row leaves connected holds the workspace lock for the rest of its
// session, so an external index started beside it would be refused -- which is
// the product working, and the opposite of what this row must set up.
func TestE2EServerStartsDuringAnotherProcessIndex(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// A published generation first: the exploration tools below must answer
	// about something, and CTX_NO_ACTIVE_GENERATION would be this row asking
	// before it indexed rather than the server failing to serve.
	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the first index failed (exit %d): %+v", code, env.Error)
	}
	padFixture(t, s.Repo, 2)
	child := exec.Command(binary, "index", "--repo", s.Repo, "--json")
	child.Env = s.Environ
	child.Stdout, child.Stderr = os.Stderr, os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start the indexing child: %v", err)
	}
	indexed := make(chan error, 1)
	go func() { indexed <- child.Wait() }()
	defer func() {
		if err := <-indexed; err != nil {
			t.Errorf("the external index failed while the server served beside it: %v", err)
		}
	}()

	// The connect is the first assertion: it starts the server process and
	// completes the protocol handshake, which the old composition could not
	// reach at all while another process held the workspace.
	session := s.mcpSession(t, ctx)

	tools := []struct {
		name string
		args any
	}{
		{"codectx_index_status", model.StatusRequest{}},
		{"codectx_search", model.SearchRequest{Query: "TinyStore"}},
	}
	cycles, refusals := 0, 0
	for running := true; running; {
		for _, tool := range tools {
			start := time.Now()
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
			wall := time.Since(start)
			if err != nil {
				t.Fatalf("%s failed while another process was indexing: %v", tool.name, err)
			}
			if res.IsError {
				t.Fatalf("%s was refused while another process was indexing: %s",
					tool.name, contentText(res))
			}
			if wall > readerCeiling {
				t.Errorf("%s took %s while another process was indexing, over the %s ceiling",
					tool.name, wall, readerCeiling)
			}
		}
		// The one operation that DOES need the workspace: it must be refused,
		// and refused as the typed, retryable busy answer rather than as a
		// session that ends or an untyped internal failure.
		refusals += refusedBusy(t, ctx, session)
		time.Sleep(readerPause)
		select {
		case err := <-indexed:
			indexed <- err
			running = false
		default:
			cycles++
		}
	}
	t.Logf("tool cycles completed strictly inside another process's index: %d, refresh refusals: %d", cycles, refusals)
	if cycles == 0 {
		t.Fatal("the external index finished before one full tool cycle completed: " +
			"nothing was proved about serving beside another process's run")
	}
	if refusals == 0 {
		t.Fatal("codectx_refresh_index was never refused while another process held the workspace: " +
			"either the run was over or the refusal is not typed")
	}
	// The session outlived the run it served beside. A refresh that had ended
	// the session, or a watch pass that had, would fail here.
	for _, tool := range tools {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
		if err != nil || res.IsError {
			t.Fatalf("%s no longer answers after the external index ended: the busy refusal took the session with it (err %v)", tool.name, err)
		}
	}
}

// refusedBusy calls codectx_refresh_index once and reports 1 when it was
// refused as CTX_WORKSPACE_BUSY. A refresh that SUCCEEDS is not a failure: the
// external run may have ended between the read above and this call, and this
// leg's caller asserts that at least one call landed strictly inside it. Any
// other refusal is a failure -- an untyped one, or a second spelling of busy,
// is precisely what this leg exists to catch.
func refusedBusy(t *testing.T, ctx context.Context, session *mcp.ClientSession) int {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "codectx_refresh_index", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("codectx_refresh_index failed at the transport while another process was indexing: %v", err)
	}
	if !res.IsError {
		return 0
	}
	text := contentText(res)
	if !strings.HasPrefix(text, model.CodeWorkspaceBusy+":") {
		t.Fatalf("codectx_refresh_index was refused while another process held the workspace, but not as %s: %s",
			model.CodeWorkspaceBusy, text)
	}
	return 1
}

// mcpReadsDuringOwnRefresh is the MCP half, and it is the server's OWN refresh
// that writes underneath the read tools rather than a separate `codectx index`.
// That is not a convenience: `codectx mcp serve` holds the Section 13.2
// workspace lock for its whole session (internal/cli/mcp.go's OpenWorkspaceForServer,
// internal/app/compose.go's indexing() modes), so a server started while another
// process is indexing is refused at startup for the lock -- which its own help
// text promises -- and never reaches a tool call. The concurrency the read
// tools must survive is therefore the refresh running beside them in the same
// process, which is what this drives.
func mcpReadsDuringOwnRefresh(t *testing.T, ctx context.Context, s *sandbox) {
	t.Helper()
	padFixture(t, s.Repo, 3)
	session := s.mcpSession(t, ctx)

	refreshed := make(chan error, 1)
	go func() {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "codectx_refresh_index", Arguments: map[string]any{}})
		if err == nil && res.IsError {
			err = fmt.Errorf("codectx_refresh_index returned a tool error: %s", contentText(res))
		}
		refreshed <- err
	}()

	tools := []struct {
		name string
		args any
	}{
		{"codectx_index_status", model.StatusRequest{}},
		{"codectx_search", model.SearchRequest{Query: "TinyStore"}},
	}
	worst := make(map[string]time.Duration, len(tools))
	calls := 0
	for running := true; running; {
		for _, tool := range tools {
			start := time.Now()
			res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
			wall := time.Since(start)
			if err != nil {
				t.Fatalf("%s failed while the server's own refresh was writing: %v", tool.name, err)
			}
			if res.IsError {
				t.Fatalf("%s was refused while the server's own refresh was writing: %s",
					tool.name, contentText(res))
			}
			if wall > worst[tool.name] {
				worst[tool.name] = wall
			}
		}
		time.Sleep(readerPause)
		select {
		case err := <-refreshed:
			if err != nil {
				t.Fatalf("the server's refresh failed while its read tools were called: %v", err)
			}
			running = false
		default:
			calls++
		}
	}
	if calls == 0 {
		t.Fatal("the refresh finished before one full tool cycle completed: " +
			"nothing was proved about reading during the server's own write")
	}
	for _, tool := range tools {
		t.Logf("tool   %-18s worst wall %s", tool.name, worst[tool.name])
		if worst[tool.name] > readerCeiling {
			t.Errorf("%s took %s during the server's own refresh, over the %s ceiling",
				tool.name, worst[tool.name], readerCeiling)
		}
	}
	t.Logf("tool cycles completed strictly inside the refresh: %d", calls)
}

// readDuringIndex execs one reader and returns its wall, failing on any
// envelope that is not a success.
//
// The two failure branches are separate on purpose. CTX_WORKSPACE_BUSY is the
// defect this test exists to catch and is named as such; any OTHER code is a
// different defect -- CTX_NO_ACTIVE_GENERATION, for one, would mean this test
// asked before it indexed -- and must not be quietly read as "a reader did not
// answer, close enough".
func (s *sandbox) readDuringIndex(t *testing.T, name string, args ...string) time.Duration {
	t.Helper()
	start := time.Now()
	env, code := s.run(t, args...)
	wall := time.Since(start)
	if env.OK && code == 0 {
		return wall
	}
	failed := model.Error{Code: "no error in the envelope"}
	if env.Error != nil {
		failed = *env.Error
	}
	if failed.Code == model.CodeWorkspaceBusy {
		t.Fatalf("`codectx %v` was refused while another process was indexing: %s: %s -- "+
			"a question-answering command wrote to the database before its first read",
			args, failed.Code, failed.Message)
	}
	t.Fatalf("`codectx %v` failed during an index for a reason other than %s (exit %d): %s: %s",
		args, model.CodeWorkspaceBusy, code, failed.Code, failed.Message)
	return wall
}

// merge folds one cycle's walls into the worst seen so far.
func merge(worst, cycle map[string]time.Duration) {
	for name, wall := range cycle {
		if wall > worst[name] {
			worst[name] = wall
		}
	}
}

// padFixture rewrites the fixture and appends the same number of trivially
// parseable symbols to each generated source file, so an index run has work to
// do for long enough that a second process can be inside it.
//
// It REGENERATES first, and that is what makes the two index walls comparable:
// appending onto an already-padded file would leave the second measured run
// parsing twice the first one's bytes, and the difference between the two walls
// would be about the workload rather than about the readers. Each round takes
// its own symbol numbers at a fixed width, so every round leaves every file the
// same size while changing every byte range a fingerprint covers.
func padFixture(t *testing.T, repo string, round int) {
	t.Helper()
	generateTinyRepo(t, repo)
	first := (round-1)*padPerFile + 1
	pad := map[string]func(int) string{
		filepath.Join("src", "go", "store", "store.go"): func(n int) string {
			return fmt.Sprintf("\nfunc TinyPadStore%04d() string {\n\treturn \"tiny\"\n}\n", n)
		},
		filepath.Join("src", "go", "service", "service.go"): func(n int) string {
			return fmt.Sprintf("\nfunc TinyPadService%04d() string {\n\treturn \"tiny\"\n}\n", n)
		},
		filepath.Join("src", "py", "tiny.py"): func(n int) string {
			return fmt.Sprintf("\n\ndef TinyPadPy%04d():\n    return \"tiny\"\n", n)
		},
		filepath.Join("src", "ts", "tiny.ts"): func(n int) string {
			return fmt.Sprintf("\nexport function TinyPadTs%04d(): string {\n  return \"tiny\";\n}\n", n)
		},
	}
	for rel, body := range pad {
		f, err := os.OpenFile(filepath.Join(repo, rel), os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open %s to widen it: %v", rel, err)
		}
		for n := first; n < first+padPerFile; n++ {
			if _, err := f.WriteString(body(n)); err != nil {
				f.Close()
				t.Fatalf("widen %s: %v", rel, err)
			}
		}
		if err := f.Close(); err != nil {
			t.Fatalf("widen %s: %v", rel, err)
		}
	}
}
