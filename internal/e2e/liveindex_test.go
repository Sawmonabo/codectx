package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
const padPerFile = 3000

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

	worst := make(map[string]time.Duration, len(readers))
	cycles := 0
	for running := true; running; {
		for _, r := range readers {
			if wall := s.readDuringIndex(t, r.name, r.args...); wall > worst[r.name] {
				worst[r.name] = wall
			}
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
		default:
			// The child outlived a whole cycle: every reader in it answered
			// strictly inside the run.
			cycles++
		}
	}
	for _, r := range readers {
		t.Logf("reader %-18s worst wall %s", r.name, worst[r.name])
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

// padFixture appends the same number of trivially parseable symbols to each
// generated source file, so an index run has work to do for long enough that a
// second process can be inside it. Each round takes its own symbol numbers, at
// a fixed width, so every round appends the same number of bytes and the two
// measured index walls are comparable.
func padFixture(t *testing.T, repo string, round int) {
	t.Helper()
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
