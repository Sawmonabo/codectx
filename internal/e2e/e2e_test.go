package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ---------------------------------------------------------------------------
// L0 harness
// ---------------------------------------------------------------------------

// binary is the codectx under test, built once for the whole test binary. Every
// row execs it; nothing in this package links the product packages in.
var binary string

// TestMain builds that binary. It is the only TestMain this package has, and it
// is unrelated to internal/bench's -- that one hijacks its own test binary as
// the parser worker, which this package must NOT do: the parser worker here has
// to be the real codectx binary re-execing itself, which is the production path
// Section 8 describes.
//
// The build is skipped under -short because every row skips under -short: a
// linked build of the whole product is not an ordinary unit-test cost.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(func() int {
		if !testing.Short() {
			dir, err := os.MkdirTemp("", "codectx-e2e-bin")
			if err != nil {
				fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
				return 1
			}
			defer os.RemoveAll(dir)
			binary = filepath.Join(dir, "codectx")
			build := exec.Command("go", "build", "-trimpath", "-o", binary,
				"github.com/Sawmonabo/codectx/cmd/codectx")
			build.Stdout, build.Stderr = os.Stderr, os.Stderr
			if err := build.Run(); err != nil {
				fmt.Fprintln(os.Stderr, "e2e: build codectx:", err)
				return 1
			}
		}
		return m.Run()
	}())
}

// envelope is the Section 18.2 shape, redeclared here rather than imported.
// internal/cli owns the product's Envelope and this package may not import it
// (see doc.go); a consumer-side copy is also what the assertion is about, since
// an external client is exactly what reads these bytes.
type envelope struct {
	SchemaVersion string          `json:"schema_version"`
	Command       string          `json:"command"`
	OK            bool            `json:"ok"`
	Data          json.RawMessage `json:"data"`
	Warnings      []string        `json:"warnings"`
	Error         *model.Error    `json:"error"`
}

// toolResult is the mcpserver.result envelope as it arrives on the wire, for
// the same reason: the server's type is unexported and in a package this one
// does not import.
type toolResult struct {
	SchemaVersion string          `json:"schema_version"`
	Warnings      []string        `json:"warnings"`
	Data          json.RawMessage `json:"data"`
}

// sandbox is one isolated installation: a generated repository, an XDG home
// nothing else shares, and the environment every codectx process of this test
// runs with. The CLI leg and the MCP leg are handed the SAME Environ, which is
// what makes "the two adapters answered about the same workspace" a fact rather
// than an assumption.
type sandbox struct {
	Repo    string
	Home    string
	Environ []string
}

// newSandbox generates the tiny repository and writes the user configuration
// the whole test runs under.
//
// The configuration is written to the USER file, not the project file, because
// provider enablement is user-level trust. Three things are pinned here on
// purpose: tools.offline makes every managed fetch a typed refusal without
// opening a socket, so no row can depend on the network; the three optional
// providers are switched off so an answer is about the corpus and not about
// what happens to be installed on this host; and mcp.watch is off so a serve
// session does not publish a new generation underneath a comparison.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end scenario: builds the binary and execs it; run without -short")
	}
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	cfgDir := filepath.Join(home, "config", "codectx")
	for _, dir := range []string{repo, cfgDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "[storage]\ndata_dir = \"" + filepath.Join(home, "data") + "\"\n" +
		"[tools]\noffline = true\n" +
		"[mcp]\nwatch = false\n" +
		"[providers.scip]\nenabled = false\n" +
		"[providers.lsp]\nenabled = false\n" +
		"[providers.dependence]\nenabled = false\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	generateTinyRepo(t, repo)
	return &sandbox{
		Repo: repo,
		Home: home,
		// PATH is kept because the binary re-execs itself through absolute
		// paths but the Go runtime and the C toolchain still expect a sane
		// environment; everything else is replaced so no developer setting
		// reaches the process under test.
		Environ: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
			"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
			"XDG_DATA_HOME=" + filepath.Join(home, "share"),
		},
	}
}

// run execs one codectx command against this sandbox with --json and returns
// the single envelope it wrote to stdout plus the process exit code.
//
// stdout and stderr are captured SEPARATELY and only stdout is decoded: Section
// 18.2 puts logs and human error text on stderr, so a combined buffer would
// make the envelope undecodable the moment the command logged anything. A
// non-zero exit is not fatal here -- the failure envelope is still on stdout,
// and rows assert on it -- so the exit code is returned rather than raised.
func (s *sandbox) run(t *testing.T, args ...string) (envelope, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, append(args, "--repo", s.Repo, "--json")...)
	cmd.Env = s.Environ
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("codectx %v: %v\nstderr:\n%s", args, err, stderr.String())
		}
		code = exit.ExitCode()
	}
	var env envelope
	if decodeErr := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); decodeErr != nil {
		t.Fatalf("codectx %v: stdout is not one envelope: %v\nstdout:\n%s\nstderr:\n%s",
			args, decodeErr, stdout.String(), stderr.String())
	}
	return env, code
}

// data decodes a successful envelope's payload. A failed envelope is a test
// failure here rather than a silent zero value, so a row that expected an
// answer never compares two empty pages and passes.
func data[T any](t *testing.T, env envelope, code int) T {
	t.Helper()
	var out T
	if !env.OK || code != 0 {
		t.Fatalf("%s failed (exit %d): %+v", env.Command, code, env.Error)
	}
	if err := json.Unmarshal(env.Data, &out); err != nil {
		t.Fatalf("%s: decode data: %v", env.Command, err)
	}
	return out
}

// mcpSession starts `codectx mcp serve --repo` as a child process and connects
// a real MCP client to it over its stdin/stdout pipes.
//
// This is the boundary the package exists for: the SDK frames newline-delimited
// JSON over those two pipes, so any byte the server writes to stdout that is
// not protocol -- a log line, a stray envelope -- corrupts the session and the
// connect below fails. The server's stderr is captured and reported on failure
// rather than discarded, since a startup refusal (a busy workspace, a rejected
// configuration) is written there by design.
//
// Closing the session closes the child's stdin, which is what ends the serve
// process and releases the Section 13.2 workspace lock it holds for the whole
// session. Nothing else in a row may take that lock while this is open.
func (s *sandbox) mcpSession(t *testing.T, ctx context.Context) *mcp.ClientSession {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(binary, "mcp", "serve", "--repo", s.Repo)
	cmd.Env = s.Environ
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "codectx-e2e", Version: "0.0.0-test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect to `codectx mcp serve` over stdio: %v\nserver stderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close mcp session: %v\nserver stderr:\n%s", err, stderr.String())
		}
	})
	return session
}

// callTool calls one tool and decodes the Section 19 answer envelope out of the
// structured content. A tool error is a test failure: the SDK discards the
// handler's result on that path, so the structured content would be absent and
// the row would otherwise compare a zero value.
func callTool[T any](t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any) T {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, contentText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: re-encode structured content: %v", name, err)
	}
	var envelope toolResult
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("%s: decode envelope: %v", name, err)
	}
	var out T
	if err := json.Unmarshal(envelope.Data, &out); err != nil {
		t.Fatalf("%s: decode data: %v", name, err)
	}
	return out
}

// contentText renders a tool error's content for a failure message.
func contentText(res *mcp.CallToolResult) string {
	var b bytes.Buffer
	for _, c := range res.Content {
		if text, isText := c.(*mcp.TextContent); isText {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

// generateTinyRepo writes the deterministic repository every row runs against:
// fixed content, fixed layout, three languages, no network and no VCS history.
//
// It is sized against the wave-F finding that sealing a 17-file session needs
// around 29 round trips: five source files, of which only two are reachable
// from the TinyRun seed, so `context plan` selects 2-3 files and the whole
// scenario is a handful of round trips rather than thirty. The two Go files are
// deliberately a caller and a callee so there is a real call edge to select on;
// the Python and TypeScript files carry the same symbol vocabulary so a row can
// tell "all languages indexed" from "one language indexed".
//
// This is NOT internal/bench's generator. That one is a benchmark workload
// parameterised by scale; this one is a fixed five-file product fixture, and
// bench may not be imported from here in any case (a _test.go in another
// package is not importable).
func generateTinyRepo(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"go.mod":       "module example.com/tiny\n\ngo 1.27\n",
		"package.json": "{\n  \"name\": \"tiny\",\n  \"version\": \"0.0.0\",\n  \"private\": true\n}\n",
		"README.md":    "# tiny\n\nGenerated end-to-end fixture. Fixed content, fixed layout, no network.\n",
		"src/go/store/store.go": "package store\n\n" +
			"// TinyStore holds one record.\n" +
			"type TinyStore struct {\n\tTinyName string\n}\n\n" +
			"// TinyLoad returns the record name.\n" +
			"func (s *TinyStore) TinyLoad() string {\n\treturn s.TinyName\n}\n",
		"src/go/service/service.go": "package service\n\n" +
			"import \"example.com/tiny/src/go/store\"\n\n" +
			"// TinyRun builds a store and reads it back. It is the scenario's seed.\n" +
			"func TinyRun() string {\n" +
			"\ts := &store.TinyStore{TinyName: \"tiny\"}\n" +
			"\treturn s.TinyLoad()\n}\n",
		"src/py/tiny.py": "\"\"\"Generated Python module.\"\"\"\n\n\n" +
			"class TinyStorePy:\n    def TinyLoadPy(self):\n        return \"tiny\"\n\n\n" +
			"def TinyRunPy():\n    return TinyStorePy().TinyLoadPy()\n",
		"src/ts/tiny.ts": "// Generated TypeScript module.\n\n" +
			"export class TinyStoreTs {\n  TinyLoadTs(): string {\n    return \"tiny\";\n  }\n}\n\n" +
			"export function TinyRunTs(): string {\n  return new TinyStoreTs().TinyLoadTs();\n}\n",
	}
	for rel, body := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// hitKey is the comparable projection of a search hit: the domain facts an
// answer is, with no operational identity in it. NodeID and FileID are left out
// deliberately -- they are per-generation identifiers, and comparing them would
// assert that the two legs read the same row of the same table rather than that
// they gave the same answer.
type hitKey struct {
	Path          string
	Kind          model.NodeKind
	Name          string
	QualifiedName string
	Tier          model.SearchTier
	Score         int64
}

func hitKeys(hits []model.SearchHit) []hitKey {
	keys := make([]hitKey, 0, len(hits))
	for _, h := range hits {
		keys = append(keys, hitKey{
			Path: h.Path, Kind: h.Kind, Name: h.Name,
			QualifiedName: h.QualifiedName, Tier: h.Tier, Score: h.ScoreMicros,
		})
	}
	return keys
}

// TestE2ESearchParity is L0's vertical slice: build the index through the CLI,
// then ask the SAME question of the SAME workspace through the CLI and through
// `codectx mcp serve` over real stdio, and require the same domain answer.
//
// Failure mode it protects: the two adapters diverge -- one filters, ranks,
// pages or truncates differently from the other -- and, because every other
// test in the tree drives exactly one of them, nothing notices. It is also the
// only row anywhere that proves `codectx mcp serve` speaks the protocol over
// real pipes at all: a stray byte on the server's stdout fails the connect.
func TestE2ESearchParity(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	// The index is built first and the process exits, releasing the workspace
	// lock. `mcp serve` below takes that same lock for its whole session, so
	// the order is not stylistic: an overlapping index would be refused busy.
	indexEnv, indexCode := s.run(t, "index")
	if !indexEnv.OK || indexCode != 0 {
		t.Fatalf("index failed (exit %d): %+v", indexCode, indexEnv.Error)
	}

	const query = "TinyStore"
	searchEnv, searchCode := s.run(t, "search", query)
	cliPage := data[model.Page[model.SearchHit]](t, searchEnv, searchCode)
	if len(cliPage.Items) == 0 {
		t.Fatalf("the CLI found no hit for %q in the generated repository", query)
	}

	session := s.mcpSession(t, ctx)
	mcpPage := callTool[model.Page[model.SearchHit]](t, ctx, session, "codectx_search",
		model.SearchRequest{Query: query})

	cli, mcpHits := hitKeys(cliPage.Items), hitKeys(mcpPage.Items)
	if len(cli) != len(mcpHits) {
		t.Fatalf("adapters disagree on hit count for %q: cli %d, mcp %d\ncli: %+v\nmcp: %+v",
			query, len(cli), len(mcpHits), cli, mcpHits)
	}
	for i := range cli {
		if cli[i] != mcpHits[i] {
			t.Errorf("hit %d differs between adapters:\ncli: %+v\nmcp: %+v", i, cli[i], mcpHits[i])
		}
	}
	// The two legs must also have answered from the same generation. Without
	// this, a serve session that quietly published its own generation would
	// still compare equal on a repository that had not changed.
	if cliPage.Meta.Binding.GenerationID != mcpPage.Meta.Binding.GenerationID {
		t.Errorf("adapters answered from different generations: cli %d, mcp %d",
			cliPage.Meta.Binding.GenerationID, mcpPage.Meta.Binding.GenerationID)
	}
}

// ---------------------------------------------------------------------------
// L1 rows
//
// Lane T21-L1 fills the one Section 25.1 product-boundary scenario in here, as
// four rows of one table over ONE pass of the harness above -- not four
// walkthroughs. The seams it builds on are newSandbox, sandbox.run, data,
// sandbox.mcpSession, callTool and generateTinyRepo; it adds no second TestMain
// and no second fixture generator.
//
//	(a) TestE2EProductBoundary  -- index -> search -> impact -> context plan ->
//	    context read -> context acknowledge (receipt) -> context acknowledge
//	    --file-review -> context advance verify -> context advance consolidate
//	    -> context capsule, through the CLI and the same domain data through the
//	    stdio MCP transport; the MCP leg writes nothing to the repository.
//	    Failure mode: the two adapters diverge and only one is ever exercised.
//	(b) incremental mutation -- one file changed, `refresh`, the new generation
//	    reflects it while a session pinned to the old generation still reads the
//	    RETAINED bytes. Failure mode: a refresh serves an open session the wrong
//	    source.
//	(c) old-session retained read after a later activation returns byte-identical
//	    content and a receipt that still verifies. Failure mode: retention
//	    silently drops what a capsule cites.
//	(d) populated-capsule human rendering -- a capsule with real observations
//	    renders every section populated in the HUMAN form. Failure mode:
//	    `context capsule` prints a shell an operator reads as "nothing found".
//
// ---------------------------------------------------------------------------
