package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// waitPadPerFile is what the waiting leg pads with instead. That leg's holder
// must still be running well after the fixed ten-second bound the progress
// policy replaced, or its mutation cannot bite: at padPerFile the holder's run
// measures about six seconds on this host, so three times the symbols puts it
// near twenty -- twice the bound, with room for a host half this speed.
const waitPadPerFile = 3 * padPerFile

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
	padFixture(t, s.Repo, 1, padPerFile)
	aloneStart := time.Now()
	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the unattended index failed (exit %d): %+v", code, env.Error)
	}
	aloneWall := time.Since(aloneStart)

	// Arm two: the same index with every reader running throughout.
	padFixture(t, s.Repo, 2, padPerFile)
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
// It is a sandbox of its own, not a further leg of the row above: the row above
// leaves a server connected and a published generation of its own, and this one
// must start from a workspace whose only index is the one it runs itself.
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
	padFixture(t, s.Repo, 2, padPerFile)
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
	session, _, closeSession := s.mcpServer(t, ctx)
	t.Cleanup(closeSession)

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
	// That session is ended here, deliberately: the other half of coexistence
	// is asked of a session of its own, with one server on the workspace.
	closeSession()
	serverReleasesTheWorkspaceAfterARefresh(t, ctx, s, tools)
}

// serverReleasesTheWorkspaceAfterARefresh is the other half of the same
// promise: the person's own `codectx index` runs while an agent's server is
// connected and answering.
//
// Failure mode it protects: the server takes the workspace lock at its first
// refresh and KEEPS it for the rest of the session, so an idle agent server
// makes `codectx index` in a terminal answer busy for as long as that server
// lives -- the same coexistence defect from the other side. Mutation that must
// fail it is quoted in this lane's report: hold the lock for the session in
// internal/app/compose.go (drop the release, or never decrement) and the
// external index below is refused.
//
// The session runs with --watch=false so that the refresh is the only thing
// that can have taken the workspace: what a WATCHING session owes is the same
// promise over its beats, and the row below this one asks it of one.
func serverReleasesTheWorkspaceAfterARefresh(t *testing.T, ctx context.Context, s *sandbox, tools []struct {
	name string
	args any
}) {
	t.Helper()
	session, _, closeSession := s.mcpServer(t, ctx, "--watch=false")
	t.Cleanup(closeSession)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "codectx_refresh_index", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("codectx_refresh_index failed at the transport on an unheld workspace: %v", err)
	}
	if res.IsError {
		t.Fatalf("codectx_refresh_index was refused on a workspace nothing else holds: %s", contentText(res))
	}

	// The assertion: the person's own index, as a further process, while that
	// same session is still connected.
	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("`codectx index` was refused (exit %d) while a connected server sat idle: %+v -- "+
			"the server is holding the workspace lock past the refresh that took it", code, env.Error)
	}
	// And the session is still answering afterwards, so what it gave up was
	// the lock and not the workspace.
	for _, tool := range tools {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
		if err != nil || res.IsError {
			t.Fatalf("%s no longer answers after another process indexed beside the session (err %v)", tool.name, err)
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
// That is not a convenience: this row is about a read and a write inside ONE
// process, which is what a server answering while it refreshes is. Another
// process's index beside a server is the row above, and the reads a session
// serves through its own refresh are these.
func mcpReadsDuringOwnRefresh(t *testing.T, ctx context.Context, s *sandbox) {
	t.Helper()
	padFixture(t, s.Repo, 3, padPerFile)
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

// padFixture rewrites the fixture and appends the given number of trivially
// parseable symbols to each generated source file, so an index run has work to
// do for long enough that a second process can be inside it.
//
// It REGENERATES first, and that is what makes the two index walls comparable:
// appending onto an already-padded file would leave the second measured run
// parsing twice the first one's bytes, and the difference between the two walls
// would be about the workload rather than about the readers. Each round takes
// its own symbol numbers at a fixed width, so every round leaves every file the
// same size while changing every byte range a fingerprint covers -- which holds
// as long as one workspace pads at one size, so symbols is a property of the
// row and not of the round.
func padFixture(t *testing.T, repo string, round, symbols int) {
	t.Helper()
	generateTinyRepo(t, repo)
	first := (round-1)*symbols + 1
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
		for n := first; n < first+symbols; n++ {
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

// TestE2EAWatchingServerLeavesTheWorkspaceToThePerson is the product-boundary
// proof of the other half of coexistence: the half a WATCHING session owes.
//
// Failure mode it protects: `mcp.watch` is the shipped default, and a watching
// session used to take the Section 13.2 workspace lock at its first pass and
// keep it until the session ended. The person's own `codectx index` in a
// terminal was then refused CTX_WORKSPACE_BUSY for as long as their agent's
// server happened to be running -- an idle process holding a workspace it is
// not building in. Mutation that must fail this row is quoted in this lane's
// report: give the beat's release back to the session in
// internal/index/coordinator.go and the index below is refused.
//
// The beat BEFORE that index is load-bearing and not setup: a session that has
// never beaten holds nothing under either behaviour, so a row that indexed
// straight away would pass against the very defect it exists to catch.
//
// What it then asserts is that letting go costs nothing: the watch's next beat
// REUSES the generation the person's index published rather than rebuilding
// the workspace itself.
func TestE2EAWatchingServerLeavesTheWorkspaceToThePerson(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the first index failed (exit %d): %+v", code, env.Error)
	}
	// --watch=true and not the sandbox default: this sandbox turns mcp.watch
	// off so the other rows compare generations nothing moves underneath, and
	// this row is about the watching session in particular.
	session, serverLog, closeSession := s.mcpServer(t, ctx, "--watch=true")
	t.Cleanup(closeSession)

	touchFixture(t, s.Repo)
	armed := awaitRefreshPast(t, ctx, serverLog, refreshedRecord, 0)
	t.Logf("the watching session completed a beat: generation %d, %d reused, %d built",
		armed.generation, armed.reused, armed.built)

	// The assertion: the person's own index, as a further process, while that
	// watching session is connected and has already beaten.
	env, code := s.run(t, "index")
	if !env.OK || code != 0 {
		t.Fatalf("`codectx index` was refused (exit %d) beside a connected watching server: %+v -- "+
			"the watch is holding the workspace between its beats", code, env.Error)
	}
	person := data[model.IndexResult](t, env, code)
	if int64(person.Binding.GenerationID) <= armed.generation {
		t.Fatalf("the person's index published generation %d, which is not past the watch's beat at %d",
			person.Binding.GenerationID, armed.generation)
	}

	// The next beat: the fixture is rewritten with the content it already has,
	// so the notification fires and every unit hashes to what the person's
	// index just sealed. A beat that reuses them is a beat that started from
	// the generation that index published; one that rebuilt them started from
	// its own.
	touchFixture(t, s.Repo)
	next := awaitRefreshPast(t, ctx, serverLog, refreshedRecord, int64(person.Binding.GenerationID))
	t.Logf("the beat after the person's index: generation %d, %d reused, %d built, %d invalidated",
		next.generation, next.reused, next.built, next.invalidated)
	if next.reused == 0 || next.built != 0 {
		t.Fatalf("the beat after the person's index reused %d units and built %d: "+
			"it rebuilt the workspace rather than reusing the generation that index published",
			next.reused, next.built)
	}

	// And the session is still answering, so what it gave up was the workspace
	// and not the session.
	for _, tool := range []struct {
		name string
		args any
	}{
		{"codectx_index_status", model.StatusRequest{}},
		{"codectx_search", model.SearchRequest{Query: "TinyStore"}},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
		if err != nil || res.IsError {
			t.Fatalf("%s no longer answers after the person indexed beside the watching session (err %v)", tool.name, err)
		}
	}
}

// touchFixture rewrites one generated source file with the content it already
// holds. The filesystem notification fires on the write, so it is what makes a
// beat happen now rather than at the next reconcile interval, and the content
// is unchanged so what that beat does with the published generation is the only
// thing the caller is measuring.
func touchFixture(t *testing.T, repo string) {
	t.Helper()
	rel := filepath.Join("src", "go", "store", "store.go")
	body, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := os.WriteFile(filepath.Join(repo, rel), body, 0o600); err != nil {
		t.Fatalf("rewrite %s: %v", rel, err)
	}
}

// refreshed is one "refreshed" record a watching session wrote to its stderr:
// the generation that beat published and what it did with the units.
type refreshed struct {
	generation  int64
	reused      int64
	built       int64
	invalidated int64
}

// refreshedRecord matches the record `mcp serve` writes for each beat of its
// watch (internal/cli/mcp.go), as the default text handler renders it.
var refreshedRecord = regexp.MustCompile(
	`msg=refreshed generation=(\d+) health=\S+ reused=(\d+) built=(\d+) invalidated=(\d+)`)

// awaitRefreshPast waits for the first beat of the running watch that published
// a generation past minGeneration, and reports what that beat did.
//
// Past a generation rather than "the next record": a beat that was already
// running when the caller's index started publishes whatever it staged before
// it, and only a generation greater than the one that index published can have
// been staged after it.
//
// The record pattern is the caller's because the two watching surfaces report a
// beat in two different renderings -- the server logs one, `codectx watch`
// writes its own machine line -- and one loop over both is what keeps the two
// legs comparing the same thing.
func awaitRefreshPast(t *testing.T, ctx context.Context, log *serverLog, record *regexp.Regexp, minGeneration int64) refreshed {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		for _, m := range record.FindAllStringSubmatch(log.String(), -1) {
			r := refreshed{generation: mustInt(t, m[1]), reused: mustInt(t, m[2]),
				built: mustInt(t, m[3]), invalidated: mustInt(t, m[4])}
			if r.generation > minGeneration {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the watching session reported no beat past generation %d within the wait\nserver stderr:\n%s",
				minGeneration, log.String())
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the watch's next beat: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func mustInt(t *testing.T, text string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatalf("a refreshed record carried %q where a number belongs: %v", text, err)
	}
	return n
}

// TestE2EAnIdleServerHoldsNoLedgerWriter is the one-writer-per-ledger proof.
//
// Failure mode it protects: a process opens the run ledger's WRITER for its
// whole session while it holds the workspace lock only for the beat that
// builds. A connected `mcp serve` sitting idle then has a collector on
// ledger.db, and the person's own `codectx index` beside it is a second writer
// on the same file -- the exact thing the ledger's separate database and its
// single collector exist to prevent. The artifact is probed rather than the
// run rows, because an idle server with a collector open writes no run row
// either: only the file ledger.Open creates distinguishes the two.
//
// Mutation that must fail it: in internal/app/compose.go, attach the ledger's
// collector at composition (`if o.indexing()`) instead of under the hold.
func TestE2EAnIdleServerHoldsNoLedgerWriter(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	root := filepath.Join(s.Home, "data")
	if files := ledgerFiles(t, root); len(files) != 0 {
		t.Fatalf("a workspace where nothing has built already carries %v", files)
	}

	session, serverLog, closeSession := s.mcpServer(t, ctx)
	defer closeSession()
	// Connected AND serving: a server that failed to come up would carry no
	// ledger either, and would prove nothing. The tool listing and not a query
	// tool, because nothing has been published yet and every query tool
	// correctly refuses CTX_NO_ACTIVE_GENERATION on this workspace.
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("the session lists no tools (err %v), so it is not serving\nserver stderr:\n%s",
			err, serverLog.String())
	}
	if files := ledgerFiles(t, root); len(files) != 0 {
		t.Fatalf("a connected, idle `mcp serve` opened the run ledger's writer: %v\n"+
			"it holds the workspace lock only for the beat that builds, so this is a second "+
			"writer on a file whose design rests on there being one\nserver stderr:\n%s",
			files, serverLog.String())
	}

	// The person's own index, as a second process, beside that idle server.
	env, code := s.run(t, "index")
	if !env.OK || code != 0 {
		t.Fatalf("`codectx index` was refused (exit %d) beside a connected idle server: %+v", code, env.Error)
	}
	result := data[model.IndexResult](t, env, code)
	if result.Run == nil {
		t.Fatal("the index beside an idle server recorded no run: it is the only writer and must record in full")
	}
	if result.Run.GenerationID == nil || len(result.Stages) == 0 {
		t.Fatalf("the index's run is incomplete: generation %v, %d stages",
			result.Run.GenerationID, len(result.Stages))
	}
	if files := ledgerFiles(t, root); len(files) == 0 {
		t.Fatal("the index recorded no ledger file at all, so the absence above proves nothing")
	}
	closeSession()

	// The other half: a session that BUILDS does open the writer. The ledger
	// is removed the way TestE2EResourcesReportsAStoreWithNoLedger removes it,
	// so what reappears can only be this session's own refresh.
	for _, path := range ledgerFiles(t, root) {
		if err := os.Remove(path); err != nil {
			t.Fatalf("removing the run ledger: %v", err)
		}
	}
	_, watchLog, closeWatching := s.mcpServer(t, ctx, "--watch=true")
	defer closeWatching()
	touchFixture(t, s.Repo)
	beat := awaitRefreshPast(t, ctx, watchLog, refreshedRecord, int64(result.Binding.GenerationID))
	t.Logf("the watching session's beat published generation %d", beat.generation)
	if files := ledgerFiles(t, root); len(files) == 0 {
		t.Fatalf("a session that refreshed opened no run ledger, so its beat recorded nothing\nserver stderr:\n%s",
			watchLog.String())
	}
}

// ledgerFiles lists the run-ledger artifacts under a data root: the database
// and its write-ahead sidecars, which are what opening the ledger for writing
// creates.
func ledgerFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() && strings.HasPrefix(d.Name(), "ledger.db") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// cliRefreshedRecord matches the machine line `codectx watch --json` writes to
// its stderr for each beat (internal/cli/index.go machineRefreshLine). It is
// anchored to a whole line because the session's structured log shares that
// stream, and it carries `carried` where the server's slog record does not,
// which is exactly why the two legs cannot share one pattern.
var cliRefreshedRecord = regexp.MustCompile(
	`(?m)^refreshed generation=(\d+) health=\S+ reused=(\d+) built=(\d+) carried=\d+ invalidated=(\d+)$`)

// watchCommand starts `codectx watch --json` as a child process and hands back
// its stderr -- where every beat is reported while the session runs -- and the
// stop that ends it.
//
// The stop INTERRUPTS rather than kills: cmd/codectx turns SIGINT into the
// context cancellation a watch session ends on, so the envelope it returns is
// the one a real interrupted session writes, and a session that had died on
// its own is caught by the exit code rather than passing for a live one. It is
// idempotent and registered as a cleanup, so a row may end the watch where it
// says so and still be sure no child outlives the test.
func (s *sandbox) watchCommand(t *testing.T) (*serverLog, func() (envelope, int)) {
	t.Helper()
	stderr := &serverLog{}
	var stdout bytes.Buffer
	cmd := exec.Command(binary, "watch", "--repo", s.Repo, "--json")
	cmd.Env = s.Environ
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start `codectx watch`: %v", err)
	}
	var (
		once sync.Once
		env  envelope
		code int
	)
	stop := func() (envelope, int) {
		once.Do(func() {
			if err := cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("interrupt `codectx watch`: %v\nstderr:\n%s", err, stderr.String())
			}
			if err := cmd.Wait(); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("`codectx watch`: %v\nstderr:\n%s", err, stderr.String())
				}
				code = exit.ExitCode()
			}
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil {
				t.Fatalf("`codectx watch`: stdout is not one envelope: %v\nstdout:\n%s\nstderr:\n%s",
					err, stdout.String(), stderr.String())
			}
		})
		return env, code
	}
	t.Cleanup(func() { stop() })
	return stderr, stop
}

// TestE2EAWatchingCommandLeavesTheWorkspaceToThePerson is the CLI half of
// TestE2EAWatchingServerLeavesTheWorkspaceToThePerson: the same obligation,
// asserted at the product boundary against `codectx watch` rather than against
// a watching `mcp serve`.
//
// Failure mode it protects: a CLI watch composed itself as the workspace's
// owner for its whole session -- the lock taken at its open and given up only
// when the process exited -- so an idle `codectx watch` in one terminal owned a
// workspace it was not building in, and the person's own `codectx index` in
// another was made to wait out the grace and then refused by name. One watcher
// mechanism, not two: a watch holds the workspace for the beat that builds and
// nothing else, exactly as the server's watch already does.
//
// The beat BEFORE that index is load-bearing and not setup: a session that has
// never beaten holds nothing under either behaviour, so a row that indexed
// straight away would pass against the very defect it exists to catch.
//
// Mutation that must fail this row is quoted in this lane's report: make
// locksAtOpen true for the watch composition again and the index below is
// refused.
func TestE2EAWatchingCommandLeavesTheWorkspaceToThePerson(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	if env, code := s.run(t, "index"); !env.OK || code != 0 {
		t.Fatalf("the first index failed (exit %d): %+v", code, env.Error)
	}
	watchLog, stopWatch := s.watchCommand(t)

	touchFixture(t, s.Repo)
	armed := awaitRefreshPast(t, ctx, watchLog, cliRefreshedRecord, 0)
	t.Logf("`codectx watch` completed a beat: generation %d, %d reused, %d built",
		armed.generation, armed.reused, armed.built)

	// The assertion: the person's own index, as a further process, while that
	// watch session is running and has already beaten.
	env, code := s.run(t, "index")
	if !env.OK || code != 0 {
		t.Fatalf("`codectx index` was refused (exit %d) beside a running `codectx watch`: %+v -- "+
			"the watch is holding the workspace between its beats", code, env.Error)
	}
	person := data[model.IndexResult](t, env, code)
	if int64(person.Binding.GenerationID) <= armed.generation {
		t.Fatalf("the person's index published generation %d, which is not past the watch's beat at %d",
			person.Binding.GenerationID, armed.generation)
	}

	// Letting go must cost nothing: the fixture is rewritten with the content
	// it already has, so the notification fires and every unit hashes to what
	// the person's index just sealed. A beat that reuses them is a beat that
	// started from the generation that index published; one that rebuilt them
	// started from its own.
	touchFixture(t, s.Repo)
	next := awaitRefreshPast(t, ctx, watchLog, cliRefreshedRecord, int64(person.Binding.GenerationID))
	t.Logf("the beat after the person's index: generation %d, %d reused, %d built, %d invalidated",
		next.generation, next.reused, next.built, next.invalidated)
	if next.reused == 0 || next.built != 0 {
		t.Fatalf("the beat after the person's index reused %d units and built %d: "+
			"it rebuilt the workspace rather than reusing the generation that index published",
			next.reused, next.built)
	}

	// And what it gave up was the workspace and not the session: the watch is
	// still running here, and interrupting it now ends it the ordinary way,
	// counting the beats it ran.
	final, exit := stopWatch()
	if !final.OK || exit != 0 {
		t.Fatalf("the watch session did not end cleanly (exit %d): %+v", exit, final.Error)
	}
	var counts map[string]int64
	if err := json.Unmarshal(final.Data, &counts); err != nil {
		t.Fatalf("the watch envelope carries no refresh count: %v", err)
	}
	if counts["refreshes"] < 2 {
		t.Fatalf("the watch reported %d refreshes: it did not survive the person's index beside it",
			counts["refreshes"])
	}
}

// waitingLine matches the machine waiting line a building command writes to its
// stderr for each change while another process holds the workspace
// (internal/cli/index.go waitingForHolder). It is anchored to a whole line
// because the command's structured log shares that stream, and `stage` is
// captured as the rest of the line rather than one field: a holder that has
// named no stage yet legitimately renders an empty one, and the row below
// asserts about the stages that appear rather than about the first line.
var waitingLine = regexp.MustCompile(`(?m)^waiting holder_pid=(\d+) holder_operation=(\S+) stage=(.*)$`)

// indexChild is one `codectx index --json` process: the envelope it wrote, its
// stderr -- where the waiting and progressive lines land -- and the wall its
// process actually took, measured around exec rather than taken from the run's
// own accounting, because the wait for the workspace happens before the run
// exists and is exactly what this leg measures.
type indexChild struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr serverLog
	start  time.Time
	env    envelope
	code   int
	wall   time.Duration
	exitAt time.Time
	done   chan struct{}
}

// startIndexChild starts `codectx index` against the sandbox and returns at
// once. Both processes of the leg below are started this way, so the two walls
// it reports are measured the same way.
func startIndexChild(t *testing.T, s *sandbox) *indexChild {
	t.Helper()
	c := &indexChild{cmd: exec.Command(binary, "index", "--repo", s.Repo, "--json"), done: make(chan struct{})}
	c.cmd.Env = s.Environ
	c.cmd.Stdout, c.cmd.Stderr = &c.stdout, &c.stderr
	c.start = time.Now()
	if err := c.cmd.Start(); err != nil {
		t.Fatalf("start `codectx index`: %v", err)
	}
	go func() {
		defer close(c.done)
		err := c.cmd.Wait()
		c.wall, c.exitAt = time.Since(c.start), time.Now()
		var exit *exec.ExitError
		if err != nil && errors.As(err, &exit) {
			c.code = exit.ExitCode()
		} else if err != nil {
			c.code = -1
		}
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			c.cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

// await blocks until the process has exited and decodes its one envelope. A
// non-zero exit is not fatal here: the failure envelope is still on stdout and
// the caller asserts on it, exactly as sandbox.run does.
func (c *indexChild) await(t *testing.T) (envelope, int) {
	t.Helper()
	<-c.done
	if err := json.Unmarshal(bytes.TrimSpace(c.stdout.Bytes()), &c.env); err != nil {
		t.Fatalf("`codectx index`: stdout is not one envelope: %v\nstdout:\n%s\nstderr:\n%s",
			err, c.stdout.String(), c.stderr.String())
	}
	return c.env, c.code
}

// awaitHolderPID waits until a process has recorded itself in the workspace
// lock file and returns its pid. The lock file is read rather than the run
// ledger because the record is written by the acquisition itself
// (internal/snapshot/lock.go record), so a pid read here means the holder HAS
// the workspace -- which is what makes the second process below a waiter and
// not a race.
func awaitHolderPID(t *testing.T, s *sandbox, holder *indexChild) int {
	t.Helper()
	path := filepath.Join(s.Home, "data", "workspace.lock")
	deadline := time.Now().Add(time.Minute)
	for {
		if body, err := os.ReadFile(path); err == nil {
			if pidText, _, split := strings.Cut(strings.TrimSpace(string(body)), " "); split {
				if pid, convErr := strconv.Atoi(pidText); convErr == nil && pid > 0 {
					return pid
				}
			}
		}
		select {
		case <-holder.done:
			t.Fatalf("the holding index exited (code %d) before it recorded itself in %s\nstderr:\n%s",
				holder.code, path, holder.stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no process recorded itself in %s within the wait\nholder stderr:\n%s",
				path, holder.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2EASecondBuildWaitsOutTheHolderAndReusesItsWork is the process-level
// proof of the wait for the workspace: one built binary waits for another built
// binary and then reuses its work.
//
// Failure mode it protects: a build that finds the workspace held is bounded by
// a fixed wait and is then refused CTX_WORKSPACE_BUSY however hard the holder
// is working -- so the person who typed `codectx index` in a second terminal,
// or the agent whose refresh followed a person's index, is told to go away in
// the middle of a run that is progressing normally, and the work the holder was
// about to publish is never reused. Unit rows measure the policy
// (internal/snapshot/lock_test.go) and pin which operation asks for it
// (internal/index/index_test.go); nothing had ever watched two built binaries
// do it.
//
// Mutation that must fail it is quoted in this lane's report: restore the fixed
// bound the policy replaced (a ten-second deadline beside the grace in
// internal/snapshot/lock.go LockWorkspace) and the second process is refused.
// The fixture is padded so the holder's run outlasts that bound several times
// over; the wait the waiter actually did is logged, so the padding is justified
// by a measurement rather than by a symbol count.
//
// The stalled holder is deliberately not asked here: it is a unit row, because
// stalling a real built index is a sleep and not a proof.
func TestE2EASecondBuildWaitsOutTheHolderAndReusesItsWork(t *testing.T) {
	s := newSandbox(t)

	// No seeded generation: the holder is the cold index of the padded
	// fixture, which is both the longest run this fixture can produce and one
	// process fewer than a seeded leg would cost.
	padFixture(t, s.Repo, 1, waitPadPerFile)

	holder := startIndexChild(t, s)
	holderPID := awaitHolderPID(t, s, holder)
	if holderPID != holder.cmd.Process.Pid {
		t.Fatalf("the workspace lock records process %d where the index this leg started is %d: "+
			"the waiting line below cannot be checked against the holder's pid", holderPID, holder.cmd.Process.Pid)
	}

	// The waiter, started while the holder demonstrably has the workspace.
	waiter := startIndexChild(t, s)

	holderEnv, holderCode := holder.await(t)
	if !holderEnv.OK || holderCode != 0 {
		t.Fatalf("the holding index failed (exit %d): %+v\nstderr:\n%s", holderCode, holderEnv.Error, holder.stderr.String())
	}
	held := data[model.IndexResult](t, holderEnv, holderCode)

	// (1) The waiter is not refused.
	waiterEnv, waiterCode := waiter.await(t)
	if !waiterEnv.OK || waiterCode != 0 {
		t.Fatalf("the second `codectx index` was refused (exit %d) behind a progressing index: %+v -- "+
			"a build that finds the workspace held must wait for as long as the holder is getting somewhere\nstderr:\n%s",
			waiterCode, waiterEnv.Error, waiter.stderr.String())
	}

	// (2) It said who it was waiting for, by pid, operation and stage.
	lines := waitingLine.FindAllStringSubmatch(waiter.stderr.String(), -1)
	if len(lines) == 0 {
		t.Fatalf("the waiting index printed no waiting line at all, so nobody watching it learns why it is "+
			"sitting there\nstderr:\n%s", waiter.stderr.String())
	}
	staged := ""
	for _, line := range lines {
		if pid := mustInt(t, line[1]); int(pid) != holderPID {
			t.Fatalf("a waiting line names process %d where the holder is %d: %q", pid, holderPID, line[0])
		}
		if line[2] != "index" {
			t.Fatalf("a waiting line names operation %q where the holder is an index: %q", line[2], line[0])
		}
		if line[3] != "" {
			staged = line[3]
		}
	}
	if staged == "" {
		t.Fatalf("no waiting line named a stage of the holder's run, so the line reports a process and not "+
			"progress\nstderr:\n%s", waiter.stderr.String())
	}
	t.Logf("the waiter reported %d waiting changes; the last stage it named was %q", len(lines), staged)

	// (3) It proceeded after the holder exited, and reused what the holder
	// published rather than building the workspace again. The counts are the
	// discriminator, not the generation number: nothing changed between the
	// two runs, so reusing every unit is the whole claim.
	if !waiter.exitAt.After(holder.exitAt) {
		t.Fatalf("the waiter exited at %s and the holder at %s: it did not wait for the workspace",
			waiter.exitAt, holder.exitAt)
	}
	reused := data[model.IndexResult](t, waiterEnv, waiterCode)
	if reused.UnitsReused == 0 || reused.UnitsBuilt != 0 {
		t.Fatalf("the index that waited reused %d units and built %d: it waited for the holder and then "+
			"rebuilt the workspace instead of reusing the generation the holder published",
			reused.UnitsReused, reused.UnitsBuilt)
	}
	if reused.Binding.GenerationID < held.Binding.GenerationID {
		t.Fatalf("the index that waited is bound to generation %d, behind the holder's %d",
			reused.Binding.GenerationID, held.Binding.GenerationID)
	}

	// (4) The walls. A waiter that rebuilt would have spent the holder's whole
	// run again after it, landing near twice the holder's wall; half of that
	// wall is the margin that separates "waited and reused" from "waited and
	// rebuilt" without encoding a host speed.
	t.Logf("holder wall %s (built %d units), waiter wall %s of which %s waiting and %s after the holder exited "+
		"(reused %d units, built %d)",
		holder.wall, held.UnitsBuilt, waiter.wall, holder.exitAt.Sub(waiter.start),
		waiter.exitAt.Sub(holder.exitAt), reused.UnitsReused, reused.UnitsBuilt)
	if ceiling := holder.wall + holder.wall/2; waiter.wall > ceiling {
		t.Errorf("the index that waited took %s against the holder's %s, over the %s ceiling: "+
			"it waited out the holder and then did the holder's work again", waiter.wall, holder.wall, ceiling)
	}
}
