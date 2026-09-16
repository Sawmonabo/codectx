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
	"strings"
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

// runText execs one codectx command WITHOUT --json and returns what it wrote to
// stdout: the human rendering, which is what row (d) is about.
//
// sandbox.run cannot serve this row. It pins --json by design, because every
// other row asserts on the Section 18.2 envelope; the human renderers are a
// separate output path with separate code, and the wave-F gap this row closes
// is precisely that nothing had ever executed them against a populated capsule.
func (s *sandbox) runText(t *testing.T, args ...string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(binary, append(args, "--repo", s.Repo)...)
	cmd.Env = s.Environ
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("codectx %v: %v\nstdout:\n%s\nstderr:\n%s", args, err, stdout.String(), stderr.String())
	}
	return stdout.String()
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
// The one Section 25.1 product-boundary scenario is filled in here as four rows
// of one table over ONE pass of the harness above -- not four walkthroughs. The
// seams they build on are newSandbox, sandbox.run, data, sandbox.mcpSession,
// callTool and generateTinyRepo; they add no second TestMain and no second
// fixture generator.
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

// --- The one scenario, walked by both product boundaries --------------------

// adapter is one of the two boundaries. The scenario below is written once
// against this interface and walked by both, which is what row (a) is for: a
// divergence cannot hide behind "the other adapter is covered elsewhere" when
// neither has a walkthrough of its own.
//
// Every method takes the landed model request type, so neither implementation
// can quietly answer a narrower question than the other -- the CLI adapter
// renders the request into argv and the MCP adapter sends it as tool arguments,
// and a field either of them drops surfaces as a compared difference.
type adapter interface {
	// kind names the boundary in a failure message.
	kind() string
	impact(t *testing.T, req model.ImpactRequest) model.ImpactResult
	plan(t *testing.T, req model.PlanRequest) (model.PlanResult, model.SessionStatus)
	coverage(t *testing.T, req model.SessionRequest) ([]model.FileCoverage, model.SessionStatus)
	read(t *testing.T, req model.ReadChunkRequest) model.ReadChunkResponse
	acknowledge(t *testing.T, req model.AcknowledgeRequest) model.SessionStatus
	waive(t *testing.T, req model.WaiverRequest) model.WaiverRecord
	record(t *testing.T, req model.ObservationRequest) model.Observation
	advance(t *testing.T, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus)
	capsule(t *testing.T, req model.CapsuleRequest) model.CapsulePage
}

// The comparable projections the two walks are compared on. Operational
// identity is left out for the reason hitKey leaves out NodeID: session ids,
// observation ids and creation times are per-actor facts, and requiring them to
// match would assert that the two boundaries shared a session rather than that
// they gave the same answer about the same repository.
type (
	impactKey struct {
		NodeID    model.NodeID
		FileID    model.FileID
		Name      string
		Kind      model.NodeKind
		Direction model.Direction
		Depth     int
		Score     int64
		Relations string
	}
	chunkKey struct {
		FileID      model.FileID
		ContentHash string
		Start, End  uint64
		Encoding    model.ChunkEncoding
		PartialLine bool
		Content     string
	}
	waiverKey struct {
		FileID      model.FileID
		ContentHash string
		Reason      string
	}
	observationKey struct {
		Kind       model.ObservationKind
		References int
		Note       string
	}
)

// walk is everything one boundary produced, projected onto domain facts.
type walk struct {
	Session        model.SessionID
	Generation     model.GenerationID
	ManifestID     model.ManifestID
	ManifestHash   string
	RequestHash    string
	EntryCount     int
	SliceCount     int
	ScopeComplete  bool
	Impact         []impactKey
	Chunks         []chunkKey
	Coverage       []model.FileCoverage
	Accepted       []model.RelationID
	Rejected       []model.RelationID
	Contradictions []observationKey
	Unresolved     []observationKey
	Waivers        []waiverKey
	CapsuleHash    string
	StrictGate     bool
	FinalState     model.WorkflowState
}

// seed is what the shared discovery step found and both walks start from: the
// node a change is assessed against and one real relation id out of the graph,
// so the observations the capsule records cite a fact this index actually
// published rather than a well-formed id nothing produced.
type seed struct {
	Node     model.NodeID
	Relation model.RelationID
}

// capsuleViews is the six Section 17.3 projections, in the order the scenario
// pages them.
var capsuleViews = []model.CapsuleView{
	model.CapsuleViewAcceptedFacts, model.CapsuleViewRejectedFacts,
	model.CapsuleViewContradictions, model.CapsuleViewUnresolved,
	model.CapsuleViewCoverage, model.CapsuleViewWaivers,
}

// runScenario walks the Section 25.1 product-boundary scenario once, through
// one adapter, as one actor:
//
//	impact -> context plan -> context status -> context read -> context
//	acknowledge (receipt) -> context acknowledge --file-review -> context record
//	(accept, reject, contradiction, unresolved, scope review) -> context waive ->
//	context advance verify -> advance consolidate -> advance complete ->
//	context capsule.
//
// `index` and `search` are the two steps deliberately not repeated here:
// TestE2ESearchParity already drives both of them through both boundaries, and
// restating them would be the redundant row the test policy calls a defect.
// Search is still the step that produces the seed -- it is run once, by the
// caller, and its answer is what both walks are handed.
//
// `advance complete` is in the sequence because a capsule exists only once a
// session completes: PutCapsule is reached from sealCapsule alone, which is the
// consolidate_open -> complete guard. Stopping at consolidate would leave
// `context capsule` with nothing to page.
func runScenario(t *testing.T, a adapter, actor string, s seed) walk {
	t.Helper()
	var w walk

	impact := a.impact(t, model.ImpactRequest{Start: []model.NodeID{s.Node}, Direction: model.DirectionBoth})
	if len(impact.Entries) == 0 {
		t.Fatalf("%s: impact on the seed node reported nothing affected", a.kind())
	}
	w.Impact = impactKeys(impact.Entries)

	plan, opened := a.plan(t, model.PlanRequest{
		ActorID: actor,
		Context: model.ContextRequest{Task: "trace the tiny call edge", Phase: model.PhaseSweep,
			Seeds: []string{tinySeedSymbol}},
	})
	w.Session, w.ManifestID = plan.SessionID, plan.Manifest.ID
	w.ManifestHash, w.RequestHash = plan.Manifest.CanonicalHash, plan.Manifest.RequestHash
	w.EntryCount, w.SliceCount = plan.Manifest.EntryCount, plan.Manifest.SliceCount
	w.ScopeComplete = plan.Manifest.ScopeComplete
	w.Generation = opened.Binding.GenerationID

	session := model.SessionRequest{SessionID: plan.SessionID, ActorID: actor}
	files, status := a.coverage(t, session)
	required := requiredFiles(files)
	if len(required) == 0 {
		t.Fatalf("%s: the plan selected no required file, so there is nothing to read", a.kind())
	}

	// Read, confirm, review. The receipt is confirmed by its own call rather
	// than folded into the next read's ConfirmReceipts, because row (c) turns
	// on a receipt outliving a generation change and a receipt confirmed only
	// as a side effect of a later read would not show that.
	for _, f := range required {
		chunk := a.read(t, model.ReadChunkRequest{SessionID: plan.SessionID, ActorID: actor, FileID: f.FileID})
		if chunk.NextOffset != nil {
			t.Fatalf("%s: file %s did not fit one chunk; the fixture is sized so that it does", a.kind(), f.FileID)
		}
		w.Chunks = append(w.Chunks, chunkKey{
			FileID: chunk.FileID, ContentHash: chunk.ContentHash,
			Start: chunk.ByteRange.Start, End: chunk.ByteRange.End,
			Encoding: chunk.Encoding, PartialLine: chunk.PartialLine, Content: chunk.Content,
		})
		// Printing a chunk is not delivery: the coverage reported beside it is
		// the coverage BEFORE it, and only the confirmation below earns any.
		if chunk.Coverage == model.CoverageFullServed {
			t.Errorf("%s: file %s reported full coverage from the read itself, before its receipt was confirmed",
				a.kind(), f.FileID)
		}
		a.acknowledge(t, model.AcknowledgeRequest{SessionID: plan.SessionID, ActorID: actor,
			Kind: model.AcknowledgeReceipt, Receipts: []string{chunk.Receipt}})
		// The file review is the second, separate assertion: it is refused
		// unless the file is already fully served, which the confirmation above
		// has just made true.
		status = a.acknowledge(t, model.AcknowledgeRequest{SessionID: plan.SessionID, ActorID: actor,
			Kind: model.AcknowledgeFile, FileID: f.FileID})
	}
	if status.FullyServedFiles != int64(len(required)) {
		t.Fatalf("%s: %d of %d required files are served after every receipt was confirmed",
			a.kind(), status.FullyServedFiles, len(required))
	}

	// The actor's own conclusions. codectx writes none of them itself, and each
	// one populates a capsule list that row (d) then renders.
	scope := status.ScopeVersion
	a.record(t, observationOf(plan.SessionID, actor, scope, model.ObservationAcceptFact,
		"the seed reaches the store method through this edge",
		model.ClaimReference{RelationID: s.Relation}))
	a.record(t, observationOf(plan.SessionID, actor, scope, model.ObservationRejectFact,
		"the same edge does not make the store a dependency of the calling package",
		model.ClaimReference{RelationID: s.Relation}))
	a.record(t, observationOf(plan.SessionID, actor, scope, model.ObservationContradiction,
		"the edge and the node disagree about which package owns the call",
		model.ClaimReference{RelationID: s.Relation}, model.ClaimReference{NodeID: s.Node}))
	a.record(t, observationOf(plan.SessionID, actor, scope, model.ObservationUnresolved,
		"the method body itself was not traced in this session",
		model.ClaimReference{NodeID: s.Node}))
	review := a.record(t, scopeReviewOf(plan.SessionID, actor, scope, plan.Manifest.CanonicalHash, required[0]))
	if review.Review == nil {
		t.Fatalf("%s: the stored scope review carries no attestation", a.kind())
	}

	// One required file is waived, and it was read first on purpose: Section
	// 17.1 counts a waived-and-read file as read, so the consolidation guard is
	// met without context.allow_exploratory_waiver_consolidation, while the
	// strict gate still shuts on the waiver -- which is what the capsule stamps.
	waived := a.waive(t, model.WaiverRequest{SessionID: plan.SessionID, ActorID: actor,
		FileID: required[len(required)-1].FileID, Reason: waiverReason})
	w.Waivers = []waiverKey{{FileID: waived.FileID, ContentHash: waived.ContentHash, Reason: waived.Reason}}

	// The three guarded transitions. Each presents the version the previous
	// answer reported, so a transition that moved the session without saying so
	// fails the next compare-and-swap instead of passing unnoticed.
	version := status.StateVersion
	for _, target := range []model.WorkflowState{model.StateVerifyOpen, model.StateConsolidateOpen, model.StateComplete} {
		moved, sealed := a.advance(t, model.AdvanceRequest{SessionID: plan.SessionID, ActorID: actor,
			Target: target, ExpectedVersion: version})
		if moved.State != target {
			t.Fatalf("%s: the advance to %s left the session in %s", a.kind(), target, moved.State)
		}
		version, w.FinalState, w.StrictGate = moved.StateVersion, moved.State, sealed.StrictGateSatisfied
	}
	// The waiver is an admission that a required file was not read, so the gate
	// the capsule stamps must be shut. Capsule.Validate refuses the other
	// combination outright, which is why this is an assertion and not a hope.
	if w.StrictGate {
		t.Errorf("%s: the completed session reports a satisfied strict gate beside a recorded waiver", a.kind())
	}

	for _, view := range capsuleViews {
		page := a.capsule(t, model.CapsuleRequest{SessionID: plan.SessionID, ActorID: actor, View: view})
		if w.CapsuleHash == "" {
			w.CapsuleHash = page.CanonicalHash
		} else if page.CanonicalHash != w.CapsuleHash {
			t.Errorf("%s: capsule view %s reports hash %s; %s reported %s",
				a.kind(), view, page.CanonicalHash, capsuleViews[0], w.CapsuleHash)
		}
		switch view {
		case model.CapsuleViewAcceptedFacts:
			w.Accepted = relationIDs(page.AcceptedFacts)
		case model.CapsuleViewRejectedFacts:
			w.Rejected = relationIDs(page.RejectedFacts)
		case model.CapsuleViewContradictions:
			w.Contradictions = observationKeys(page.Contradictions)
		case model.CapsuleViewUnresolved:
			w.Unresolved = observationKeys(page.Unresolved)
		case model.CapsuleViewCoverage:
			w.Coverage = page.Coverage
		case model.CapsuleViewWaivers:
			if len(page.Waivers) != 1 {
				t.Errorf("%s: the sealed capsule lists %d waivers; exactly one was recorded", a.kind(), len(page.Waivers))
			}
		}
	}
	return w
}

// The fixed inputs the scenario is written against. They are constants rather
// than literals at three call sites so the CLI argv and the MCP tool arguments
// cannot drift apart into two slightly different requests, which would make the
// comparison meaningless.
const (
	tinySeedSymbol = "TinyRun"
	waiverReason   = "generated fixture file, reviewed out of band"
	cliActor       = "cli-actor"
	mcpActor       = "mcp-actor"
	retainActor    = "retention-actor"
)

// requiredFiles keeps the required_full files of one coverage page, which are
// the only ones a session owes a read.
func requiredFiles(files []model.FileCoverage) []model.FileCoverage {
	out := make([]model.FileCoverage, 0, len(files))
	for _, f := range files {
		if f.Requirement == model.RequirementFull {
			out = append(out, f)
		}
	}
	return out
}

func impactKeys(entries []model.ImpactEntry) []impactKey {
	keys := make([]impactKey, 0, len(entries))
	for _, e := range entries {
		var relations bytes.Buffer
		for _, p := range e.Paths {
			for _, id := range p.Relations {
				fmt.Fprintf(&relations, "%s,", id)
			}
		}
		keys = append(keys, impactKey{
			NodeID: e.NodeID, FileID: e.FileID, Name: e.Name, Kind: e.Kind,
			Direction: e.Direction, Depth: e.Depth, Score: e.ScoreMicros,
			Relations: relations.String(),
		})
	}
	return keys
}

func relationIDs(facts []model.FactReference) []model.RelationID {
	out := make([]model.RelationID, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.RelationID)
	}
	return out
}

func observationKeys(refs []model.ObservationReference) []observationKey {
	out := make([]observationKey, 0, len(refs))
	for _, o := range refs {
		out = append(out, observationKey{Kind: o.Kind, References: len(o.References), Note: o.Note})
	}
	return out
}

// observationOf builds one ordinary observation request.
func observationOf(session model.SessionID, actor string, scope int, kind model.ObservationKind,
	note string, refs ...model.ClaimReference) model.ObservationRequest {
	return model.ObservationRequest{SessionID: session, ActorID: actor, ExpectedScope: scope,
		Kind: kind, References: refs, Note: note}
}

// scopeReviewOf builds the structured attestation: all eight Section 17.2
// categories answered exactly once, bound to this manifest hash and scope
// version, with complete_files_read backed by a citation of a file the session
// has actually confirmed served -- which is what reviewSupported checks, and
// what makes this a real review rather than eight notes.
func scopeReviewOf(session model.SessionID, actor string, scope int, manifestHash string,
	read model.FileCoverage) model.ObservationRequest {
	categories := []model.ScopeReviewCategory{
		model.ReviewCompleteFilesRead, model.ReviewCallersConsumers, model.ReviewContractsTypes,
		model.ReviewStateLifecycle, model.ReviewDependencies, model.ReviewIntegrationPoints,
		model.ReviewSharedUtilities, model.ReviewRemainingUncertainty,
	}
	entries := make([]model.ScopeReviewEntry, 0, len(categories))
	for _, c := range categories {
		entry := model.ScopeReviewEntry{Category: c, Note: "reviewed for " + string(c)}
		if c == model.ReviewCompleteFilesRead {
			entry.References = []model.ClaimReference{{Source: &model.SourceCitation{
				FileID: read.FileID, ContentHash: read.ContentHash,
				Bytes: model.ByteRange{Start: 0, End: uint64(read.Size)},
			}}}
		}
		entries = append(entries, entry)
	}
	return model.ObservationRequest{SessionID: session, ActorID: actor, ExpectedScope: scope,
		Kind: model.ObservationScopeReview, Note: "the scope was reviewed against the pinned manifest",
		Review: &model.ScopeReview{ManifestHash: manifestHash, ScopeVersion: scope, Entries: entries},
	}
}

// repoState is every file in the repository with its bytes, as one comparable
// value. It is what "the MCP leg writes nothing to the repository" is asserted
// against: Section 6 forbids codectx writing into the tree it indexes, and the
// MCP server is the one boundary that holds the workspace for a whole session.
func repoState(t *testing.T, dir string) string {
	t.Helper()
	var state bytes.Buffer
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		fmt.Fprintf(&state, "%s\x00%d\x00%s\x00", rel, len(body), body)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	return state.String()
}

// --- The CLI boundary --------------------------------------------------------

// The composite answers the Section 18.1 commands put in one envelope. They are
// redeclared here for the reason `envelope` is: internal/cli owns the product's
// types and this package must not import it, and a consumer-side copy is what
// the assertion is about, since an external client is what reads these bytes.
type (
	cliPlan struct {
		Plan    model.PlanResult    `json:"plan"`
		Session model.SessionStatus `json:"session"`
	}
	cliStatus struct {
		Session model.SessionStatus            `json:"session"`
		Files   model.Page[model.FileCoverage] `json:"files"`
	}
	cliWaiver struct {
		Waiver  model.WaiverRecord  `json:"waiver"`
		Session model.SessionStatus `json:"session"`
	}
	cliObservation struct {
		Observation model.Observation   `json:"observation"`
		Session     model.SessionStatus `json:"session"`
	}
	cliAdvance struct {
		Workflow model.WorkflowStatus `json:"workflow"`
		Session  model.SessionStatus  `json:"session"`
	}
)

// cliAdapter drives the built binary, one exec per step.
type cliAdapter struct{ s *sandbox }

func (cliAdapter) kind() string { return "cli" }

// impact names the start nodes as positional arguments. The command pins the
// direction to both itself (Section 14.3 makes the per-entry direction the
// discriminator, which only a two-way walk can populate), so the request's
// direction is asserted rather than passed.
func (a cliAdapter) impact(t *testing.T, req model.ImpactRequest) model.ImpactResult {
	t.Helper()
	if req.Direction != model.DirectionBoth {
		t.Fatalf("`codectx impact` walks both directions; the request asked for %q", req.Direction)
	}
	args := []string{"impact"}
	for _, node := range req.Start {
		args = append(args, string(node))
	}
	env, code := a.s.run(t, args...)
	return data[model.ImpactResult](t, env, code)
}

func (a cliAdapter) plan(t *testing.T, req model.PlanRequest) (model.PlanResult, model.SessionStatus) {
	t.Helper()
	args := []string{"context", "plan", "--actor", req.ActorID,
		"--task", req.Context.Task, "--phase", string(req.Context.Phase)}
	for _, s := range req.Context.Seeds {
		args = append(args, "--seed", s)
	}
	env, code := a.s.run(t, args...)
	out := data[cliPlan](t, env, code)
	return out.Plan, out.Session
}

func (a cliAdapter) coverage(t *testing.T, req model.SessionRequest) ([]model.FileCoverage, model.SessionStatus) {
	t.Helper()
	env, code := a.s.run(t, "context", "status", string(req.SessionID), "--actor", req.ActorID)
	out := data[cliStatus](t, env, code)
	return out.Files.Items, out.Session
}

func (a cliAdapter) read(t *testing.T, req model.ReadChunkRequest) model.ReadChunkResponse {
	t.Helper()
	args := []string{"context", "read", string(req.SessionID), string(req.FileID), "--actor", req.ActorID}
	if req.Offset != 0 {
		args = append(args, "--offset", fmt.Sprint(req.Offset))
	}
	for _, receipt := range req.ConfirmReceipts {
		args = append(args, "--confirm-receipt", receipt)
	}
	env, code := a.s.run(t, args...)
	return data[model.ReadChunkResponse](t, env, code)
}

func (a cliAdapter) acknowledge(t *testing.T, req model.AcknowledgeRequest) model.SessionStatus {
	t.Helper()
	args := []string{"context", "acknowledge", string(req.SessionID)}
	if req.Kind == model.AcknowledgeFile {
		args = append(args, string(req.FileID), "--file-review")
	}
	for _, receipt := range req.Receipts {
		args = append(args, "--receipt", receipt)
	}
	args = append(args, "--actor", req.ActorID)
	env, code := a.s.run(t, args...)
	return data[model.SessionStatus](t, env, code)
}

func (a cliAdapter) waive(t *testing.T, req model.WaiverRequest) model.WaiverRecord {
	t.Helper()
	env, code := a.s.run(t, "context", "waive", string(req.SessionID), string(req.FileID),
		"--actor", req.ActorID, "--reason", req.Reason)
	return data[cliWaiver](t, env, code).Waiver
}

// record uses the two input forms the command declares and the scenario needs:
// the flag form for an ordinary observation, and --input for the structured
// scope review, which is the only way to submit one.
func (a cliAdapter) record(t *testing.T, req model.ObservationRequest) model.Observation {
	t.Helper()
	args := []string{"context", "record", string(req.SessionID), "--actor", req.ActorID,
		"--kind", string(req.Kind), "--expected-scope", fmt.Sprint(req.ExpectedScope)}
	if req.Review != nil {
		args = append(args, "--input", a.writeObservation(t, req))
	} else {
		for _, ref := range req.References {
			switch {
			case ref.RelationID != "":
				args = append(args, "--relation", string(ref.RelationID))
			case ref.NodeID != "":
				args = append(args, "--node", string(ref.NodeID))
			default:
				args = append(args, "--source", fmt.Sprintf("%s:%s:%d:%d", ref.Source.FileID,
					ref.Source.ContentHash, ref.Source.Bytes.Start, ref.Source.Bytes.End))
			}
		}
		args = append(args, "--note", req.Note)
	}
	env, code := a.s.run(t, args...)
	return data[cliObservation](t, env, code).Observation
}

// writeObservation writes the --input document: the observation's BODY only.
// The session, actor, kind and scope version stay on the command line, which is
// the split Section 18.1 fixes so a file can never disagree with the session it
// is recorded against.
func (a cliAdapter) writeObservation(t *testing.T, req model.ObservationRequest) string {
	t.Helper()
	body := struct {
		References []model.ClaimReference `json:"references,omitempty"`
		Review     *model.ScopeReview     `json:"review,omitempty"`
		Note       string                 `json:"note"`
	}{References: req.References, Review: req.Review, Note: req.Note}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the observation document: %v", err)
	}
	// Outside the repository: nothing this test does may drop a file into the
	// tree under index, which is exactly what repoState asserts.
	path := filepath.Join(t.TempDir(), "observation.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (a cliAdapter) advance(t *testing.T, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus) {
	t.Helper()
	env, code := a.s.run(t, "context", "advance", string(req.SessionID), advanceWord(t, req.Target),
		"--actor", req.ActorID, "--expected-version", fmt.Sprint(req.ExpectedVersion))
	out := data[cliAdvance](t, env, code)
	return out.Workflow, out.Session
}

func (a cliAdapter) capsule(t *testing.T, req model.CapsuleRequest) model.CapsulePage {
	t.Helper()
	env, code := a.s.run(t, "context", "capsule", string(req.SessionID),
		"--actor", req.ActorID, "--view", string(req.View))
	return data[model.CapsulePage](t, env, code)
}

// advanceWord maps a stored workflow state onto the Section 18.1 argument word.
// The two vocabularies differ on purpose -- the state names the phase being
// open, the argument the phase being entered -- and the MCP tool takes the
// state, so the translation exists on this side only.
func advanceWord(t *testing.T, target model.WorkflowState) string {
	t.Helper()
	switch target {
	case model.StateVerifyOpen:
		return string(model.PhaseVerify)
	case model.StateConsolidateOpen:
		return string(model.PhaseConsolidate)
	case model.StateComplete:
		return string(model.StateComplete)
	}
	t.Fatalf("%q is not a transition `codectx context advance` takes", target)
	return ""
}

// --- The MCP stdio boundary --------------------------------------------------

// The composite tool answers. Unexported in internal/mcpserver and therefore
// unreachable from here, which is the same reason `toolResult` is redeclared.
type (
	mcpPlan struct {
		Plan   model.PlanResult    `json:"plan"`
		Status model.SessionStatus `json:"status"`
	}
	mcpStatus struct {
		Coverage model.Page[model.FileCoverage] `json:"coverage"`
		Status   model.SessionStatus            `json:"status"`
	}
	mcpWaiver struct {
		Waiver model.WaiverRecord  `json:"waiver"`
		Status model.SessionStatus `json:"status"`
	}
	mcpObservation struct {
		Observation model.Observation   `json:"observation"`
		Status      model.SessionStatus `json:"status"`
	}
	mcpAdvance struct {
		Workflow model.WorkflowStatus `json:"workflow"`
		Status   model.SessionStatus  `json:"status"`
	}
	mcpCapsule struct {
		Page *model.CapsulePage `json:"page,omitempty"`
	}
	// mcpSessionArgs is codectx_context_status's input: the session and actor
	// are TOOL ARGUMENTS, never wire state, which is what lets one stdio
	// session answer for whichever actor asks.
	mcpSessionArgs struct {
		SessionID model.SessionID   `json:"session_id"`
		ActorID   string            `json:"actor_id"`
		Page      model.PageRequest `json:"page"`
	}
)

// mcpAdapter drives `codectx mcp serve` over the real stdio transport.
type mcpAdapter struct {
	ctx     context.Context
	session *mcp.ClientSession
}

func (mcpAdapter) kind() string { return "mcp" }

func (a mcpAdapter) impact(t *testing.T, req model.ImpactRequest) model.ImpactResult {
	t.Helper()
	return callTool[model.ImpactResult](t, a.ctx, a.session, "codectx_impact", req)
}

func (a mcpAdapter) plan(t *testing.T, req model.PlanRequest) (model.PlanResult, model.SessionStatus) {
	t.Helper()
	out := callTool[mcpPlan](t, a.ctx, a.session, "codectx_context_plan", req)
	return out.Plan, out.Status
}

func (a mcpAdapter) coverage(t *testing.T, req model.SessionRequest) ([]model.FileCoverage, model.SessionStatus) {
	t.Helper()
	out := callTool[mcpStatus](t, a.ctx, a.session, "codectx_context_status",
		mcpSessionArgs{SessionID: req.SessionID, ActorID: req.ActorID})
	return out.Coverage.Items, out.Status
}

func (a mcpAdapter) read(t *testing.T, req model.ReadChunkRequest) model.ReadChunkResponse {
	t.Helper()
	return callTool[model.ReadChunkResponse](t, a.ctx, a.session, "codectx_read_source", req)
}

func (a mcpAdapter) acknowledge(t *testing.T, req model.AcknowledgeRequest) model.SessionStatus {
	t.Helper()
	return callTool[model.SessionStatus](t, a.ctx, a.session, "codectx_context_acknowledge", req)
}

func (a mcpAdapter) waive(t *testing.T, req model.WaiverRequest) model.WaiverRecord {
	t.Helper()
	return callTool[mcpWaiver](t, a.ctx, a.session, "codectx_context_waive", req).Waiver
}

func (a mcpAdapter) record(t *testing.T, req model.ObservationRequest) model.Observation {
	t.Helper()
	return callTool[mcpObservation](t, a.ctx, a.session, "codectx_context_record", req).Observation
}

func (a mcpAdapter) advance(t *testing.T, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus) {
	t.Helper()
	out := callTool[mcpAdvance](t, a.ctx, a.session, "codectx_context_advance", req)
	return out.Workflow, out.Status
}

func (a mcpAdapter) capsule(t *testing.T, req model.CapsuleRequest) model.CapsulePage {
	t.Helper()
	out := callTool[mcpCapsule](t, a.ctx, a.session, "codectx_context_capsule", req)
	if out.Page == nil {
		t.Fatalf("codectx_context_capsule returned no page for view %q", req.View)
	}
	return *out.Page
}

// TestE2EProductBoundary is the one Section 25.1 product-boundary scenario, as
// four rows over ONE pass of the harness above rather than four walkthroughs.
// The rows share the sandbox, the index and the seed, and each one depends on
// the state the previous row left, so a failure stops the sequence instead of
// reporting three consequential failures beside the real one.
//
//	(a) the whole scenario through the CLI and through `codectx mcp serve` over
//	    real stdio, producing the same domain data, with the MCP leg writing
//	    nothing into the repository.
//	(b) one file changed, refreshed: the new generation reflects it while a
//	    session pinned to the old one does not.
//	(c) that same old session, after the later activation: byte-identical
//	    content, and a receipt issued before the refresh that still verifies.
//	(d) the sealed, populated capsule rendered in the HUMAN form, every section
//	    carrying records.
func TestE2EProductBoundary(t *testing.T) {
	s := newSandbox(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	// index and search are TestE2ESearchParity's row and are not re-asserted
	// here; they run because the scenario begins with them and because the seed
	// both walks start from is what search resolves.
	indexEnv, indexCode := s.run(t, "index")
	if !indexEnv.OK || indexCode != 0 {
		t.Fatalf("index failed (exit %d): %+v", indexCode, indexEnv.Error)
	}
	searchEnv, searchCode := s.run(t, "search", tinySeedSymbol)
	hits := data[model.Page[model.SearchHit]](t, searchEnv, searchCode)
	var node model.NodeID
	for _, hit := range hits.Items {
		if hit.Name == tinySeedSymbol && hit.NodeID != "" {
			node = hit.NodeID
			break
		}
	}
	if node == "" {
		t.Fatalf("search resolved no node for %q in the generated repository: %+v", tinySeedSymbol, hitKeys(hits.Items))
	}
	impactEnv, impactCode := s.run(t, "impact", string(node))
	whole := data[model.ImpactResult](t, impactEnv, impactCode)
	var relation model.RelationID
	for _, entry := range whole.Entries {
		for _, path := range entry.Paths {
			if len(path.Relations) > 0 {
				relation = path.Relations[0]
			}
		}
	}
	if relation == "" {
		t.Fatalf("impact on %s produced no evidence-backed relation to cite", node)
	}
	// The `r` continuation, end to end through the CLI: one page of one, then
	// the token that page printed. Impact runs its whole walk on the first
	// request and serves the globally ranked remainder from a spool, so this is
	// the only boundary at which the ranked cursor's round trip -- signed,
	// bound to this query, and read back as a ranked spool rather than a walk
	// -- is exercised against the built binary.
	if len(whole.Entries) > 1 || len(whole.Packages) > 1 {
		firstEnv, firstCode := s.run(t, "impact", string(node), "--limit", "1")
		first := data[model.ImpactResult](t, firstEnv, firstCode)
		if first.Meta.NextCursor == "" {
			t.Fatalf("impact on %s holds %d entr(ies) and %d pair(s) but offered no continuation at a page of one",
				node, len(whole.Entries), len(whole.Packages))
		}
		nextEnv, nextCode := s.run(t, "impact", string(node), "--limit", "1",
			"--cursor", first.Meta.NextCursor)
		next := data[model.ImpactResult](t, nextEnv, nextCode)
		if len(next.Entries)+len(next.Packages) == 0 {
			t.Fatalf("the ranked continuation served nothing: %+v", next.Meta)
		}
	} else if _, code := s.run(t, "impact", string(node), "--cursor",
		"not-a-token"); code == 0 {
		// The generated repository's blast radius fits one page, so the token
		// this build would have to accept cannot be produced here. What is
		// still provable at this boundary is that --cursor REACHES the request:
		// a malformed one must be refused rather than ignored.
		t.Fatal("`impact --cursor` accepted a malformed token: the flag is not reaching the request")
	}
	// `codectx path`, end to end. This is the ONLY CLI-level coverage the
	// command has: nothing else in the tree runs it, and it spent a release
	// dying on every invocation with "flag accessed but not defined: limit"
	// (pageRequest read a --limit `path` deliberately does not declare) while
	// `go test ./...` stayed green. The assertion is deliberately weak on the
	// ROUTES -- the generated repository need not connect any two nodes -- and
	// strong on the invocation: it must reach the workspace and answer.
	//
	// The loop below is the only place it runs, so "the body never executed"
	// has to be a failure rather than a green test: an impact answer whose
	// only entry is the seed would otherwise invoke `path` zero times and
	// re-admit exactly the blind spot this leg exists to close.
	ranPath := false
	for _, entry := range whole.Entries {
		if entry.NodeID == node {
			continue
		}
		ranPath = true
		pathEnv, pathCode := s.run(t, "path", string(node), string(entry.NodeID))
		if !pathEnv.OK || pathCode != 0 {
			t.Fatalf("`codectx path %s %s` failed (exit %d): %+v", node, entry.NodeID, pathCode, pathEnv.Error)
		}
		_ = data[model.PathResult](t, pathEnv, pathCode)
		// --cursor reaches the request: `path` is resumable, and a page that
		// spends its deadline or its visited budget prints a token. A search
		// this small finishes in one page, so what is provable here is that the
		// flag is wired -- a malformed token must be refused, not ignored.
		if _, code := s.run(t, "path", string(node), string(entry.NodeID),
			"--cursor", "not-a-token"); code == 0 {
			t.Fatal("`path --cursor` accepted a malformed token: the flag is not reaching the request")
		}
		break
	}
	if !ranPath {
		t.Fatalf("`codectx path` never ran: the impact answer for %s holds %d entr(ies) and none of them "+
			"is a second node, so the command's only end-to-end coverage silently did not execute", node, len(whole.Entries))
	}

	start := seed{Node: node, Relation: relation}

	var cli, viaMCP walk
	if !t.Run("the same scenario through both product boundaries", func(t *testing.T) {
		cli = runScenario(t, cliAdapter{s: s}, cliActor, start)

		// The repository is captured before the server starts and compared
		// after it stops. `codectx mcp serve` holds the workspace for its whole
		// session, and it is the one boundary a client drives without a process
		// boundary between each step, so "codectx writes no file into the tree
		// it indexes" is asserted where it is hardest to keep.
		before := repoState(t, s.Repo)
		viaMCP = runScenario(t, mcpAdapter{ctx: ctx, session: s.mcpSession(t, ctx)}, mcpActor, start)
		if after := repoState(t, s.Repo); after != before {
			t.Errorf("the MCP leg changed the repository it indexes; Section 6 forbids any source write")
		}
		compareWalks(t, cli, viaMCP)
	}) {
		return
	}

	// (d) runs before the mutation because the capsule it renders is the one
	// (a) sealed, and a refresh would supersede the session that owns it.
	t.Run("a populated capsule renders every section in the human form", func(t *testing.T) {
		renderCapsule(t, s, cli)
	})

	var pinned, refreshed model.ReadChunkResponse
	var receipt string
	var retained model.SessionRequest
	if !t.Run("a refresh does not move an open session's source", func(t *testing.T) {
		retained, pinned, receipt = openRetainedSession(t, s)
		mutateStore(t, s.Repo)

		refreshEnv, refreshCode := s.run(t, "refresh")
		result := data[model.IndexResult](t, refreshEnv, refreshCode)
		// The run states what it did. A run that has just returned is
		// finished, and its accounting has to say so: the collector holds a
		// finished run in memory until its next write, so without the barrier
		// the coordinator waits on, this reads as a run still going and its
		// last stages are missing. `running` here is the failure mode --
		// a completed index reported as live, with an incomplete stage list
		// presented as the whole of it.
		if result.Run == nil {
			t.Fatalf("the refresh reported no run; the result carries no account of what it did")
		}
		if result.Run.Outcome == "running" || result.Run.FinishedAt == nil {
			t.Errorf("the run the refresh reported is %q with finished_at %v: a run that has returned is over",
				result.Run.Outcome, result.Run.FinishedAt)
		}
		// Activation is one of the last stages a run opens, so a result that
		// carries it carries the stages that ended after the collector's last
		// periodic write, and every row belongs to the run reported above and
		// not to another run of the same process.
		activated := false
		for _, stage := range result.Stages {
			if stage.RunID != result.Run.RunID {
				t.Fatalf("a stage of run %s was reported under run %s", stage.RunID, result.Run.RunID)
			}
			if stage.Stage == "activation" {
				activated = true
			}
		}
		if !activated {
			t.Errorf("the run reported %d stages and none of them is the activation it just performed",
				len(result.Stages))
		}
		if result.Binding.GenerationID <= pinned.Binding.GenerationID {
			t.Fatalf("the refresh published generation %d; the session is pinned to %d and a change was made",
				result.Binding.GenerationID, pinned.Binding.GenerationID)
		}
		// The new generation reflects the change.
		newEnv, newCode := s.run(t, "search", mutationSymbol)
		found := data[model.Page[model.SearchHit]](t, newEnv, newCode)
		if len(found.Items) == 0 {
			t.Fatalf("the refreshed generation does not know %q, which was just added to the workspace", mutationSymbol)
		}
		if found.Meta.Binding.GenerationID != result.Binding.GenerationID {
			t.Errorf("the search answered from generation %d; the refresh published %d",
				found.Meta.Binding.GenerationID, result.Binding.GenerationID)
		}
		// The pinned session does not.
		a := cliAdapter{s: s}
		refreshed = a.read(t, model.ReadChunkRequest{SessionID: retained.SessionID,
			ActorID: retained.ActorID, FileID: pinned.FileID})
		if refreshed.Binding.GenerationID != pinned.Binding.GenerationID {
			t.Errorf("the pinned session answered from generation %d after the refresh; it pinned %d",
				refreshed.Binding.GenerationID, pinned.Binding.GenerationID)
		}
		live, err := os.ReadFile(filepath.Join(s.Repo, mutatedFile))
		if err != nil {
			t.Fatal(err)
		}
		// The live bytes are clamped to the window the session pinned before
		// they are compared: the read is bounded by the size the session
		// recorded, so an unclamped comparison would differ for the trivial
		// reason that the file grew and would pass whatever was served.
		window := live
		if uint64(len(window)) > refreshed.ByteRange.End {
			window = window[:refreshed.ByteRange.End]
		}
		if refreshed.Content == string(window) {
			t.Errorf("the pinned session served the live working-tree bytes of %s, not the source it retained", mutatedFile)
		}
	}) {
		return
	}

	t.Run("an old session still reads its retained bytes and its receipt still verifies", func(t *testing.T) {
		if refreshed.Content != pinned.Content || refreshed.ContentHash != pinned.ContentHash ||
			refreshed.ByteRange != pinned.ByteRange {
			t.Fatalf("the retained read changed across the activation:\nbefore: %s [%d,%d) %q\nafter:  %s [%d,%d) %q",
				pinned.ContentHash, pinned.ByteRange.Start, pinned.ByteRange.End, pinned.Content,
				refreshed.ContentHash, refreshed.ByteRange.Start, refreshed.ByteRange.End, refreshed.Content)
		}
		// The receipt was issued before the refresh. Confirming it now is what
		// proves retention kept what the session would cite: a receipt whose
		// bytes are gone cannot earn coverage, and a capsule that cited them
		// would name source nobody can serve again.
		a := cliAdapter{s: s}
		status := a.acknowledge(t, model.AcknowledgeRequest{SessionID: retained.SessionID,
			ActorID: retained.ActorID, Kind: model.AcknowledgeReceipt, Receipts: []string{receipt}})
		if status.FullyServedFiles == 0 {
			t.Errorf("the receipt issued before the refresh earned no coverage after it")
		}
		if !status.Superseded {
			t.Errorf("the session reports no supersession although a newer generation is active")
		}
	})
}

// compareWalks requires the two boundaries to have produced the same domain
// facts. The capsule's canonical hash is deliberately NOT compared: Section
// 17.3 hashes the observation ids and the waiver's actor into it, both of which
// are per-actor by construction, so two actors sealing identical findings seal
// two different capsules. Everything the hash covers that is not per-actor is
// compared field by field below instead.
func compareWalks(t *testing.T, cli, viaMCP walk) {
	t.Helper()
	if cli.Generation != viaMCP.Generation {
		t.Errorf("the two boundaries answered from different generations: cli %d, mcp %d",
			cli.Generation, viaMCP.Generation)
	}
	// One request against one generation compiles to one content-addressed
	// manifest, whichever boundary asked: a difference here means the two are
	// not planning the same thing, and every later comparison would be vacuous.
	if cli.ManifestID != viaMCP.ManifestID || cli.ManifestHash != viaMCP.ManifestHash ||
		cli.RequestHash != viaMCP.RequestHash {
		t.Fatalf("the boundaries compiled different manifests:\ncli: %s %s %s\nmcp: %s %s %s",
			cli.ManifestID, cli.ManifestHash, cli.RequestHash,
			viaMCP.ManifestID, viaMCP.ManifestHash, viaMCP.RequestHash)
	}
	if cli.EntryCount != viaMCP.EntryCount || cli.SliceCount != viaMCP.SliceCount ||
		cli.ScopeComplete != viaMCP.ScopeComplete {
		t.Errorf("the boundaries reported different manifest shapes: cli %d/%d scope_complete=%t, mcp %d/%d scope_complete=%t",
			cli.EntryCount, cli.SliceCount, cli.ScopeComplete,
			viaMCP.EntryCount, viaMCP.SliceCount, viaMCP.ScopeComplete)
	}
	if cli.FinalState != viaMCP.FinalState || cli.StrictGate != viaMCP.StrictGate {
		t.Errorf("the boundaries left the session in different shape: cli %s strict=%t, mcp %s strict=%t",
			cli.FinalState, cli.StrictGate, viaMCP.FinalState, viaMCP.StrictGate)
	}
	compare(t, "impact entries", cli.Impact, viaMCP.Impact)
	compare(t, "served source chunks", cli.Chunks, viaMCP.Chunks)
	compare(t, "capsule coverage", cli.Coverage, viaMCP.Coverage)
	compare(t, "capsule accepted facts", cli.Accepted, viaMCP.Accepted)
	compare(t, "capsule rejected facts", cli.Rejected, viaMCP.Rejected)
	compare(t, "capsule contradictions", cli.Contradictions, viaMCP.Contradictions)
	compare(t, "capsule unresolved items", cli.Unresolved, viaMCP.Unresolved)
	compare(t, "capsule waivers", cli.Waivers, viaMCP.Waivers)
}

// compare reports every difference between two projections rather than the
// first: which records diverged is the diagnosis, and stopping at one hides it.
func compare[T comparable](t *testing.T, what string, cli, viaMCP []T) {
	t.Helper()
	if len(cli) == 0 {
		t.Errorf("%s: the CLI produced none, so the comparison would pass vacuously", what)
		return
	}
	if len(cli) != len(viaMCP) {
		t.Errorf("%s: cli has %d, mcp has %d\ncli: %+v\nmcp: %+v", what, len(cli), len(viaMCP), cli, viaMCP)
		return
	}
	for i := range cli {
		if cli[i] != viaMCP[i] {
			t.Errorf("%s: record %d differs\ncli: %+v\nmcp: %+v", what, i, cli[i], viaMCP[i])
		}
	}
}

// The inputs rows (b) and (c) mutate and look for. The file is one of the two
// Go files generateTinyRepo writes; the symbol is new, so finding it is proof
// the refreshed generation parsed the changed bytes rather than reusing a
// sealed unit.
const (
	retainSeedSymbol = "TinyStore"
	mutatedFile      = "src/go/store/store.go"
	mutationSymbol   = "TinyExtra"
	mutationBody     = "\n// TinyExtra is added by the incremental-mutation row.\nfunc TinyExtra() string {\n\treturn \"extra\"\n}\n"
	// The mutation also rewrites a comment near the TOP of the file. An
	// append alone would leave the first pinnedSize bytes byte-identical, so a
	// read that wrongly served the LIVE tree -- clamped to the size the session
	// pinned -- would return exactly the retained bytes and row (b) could not
	// tell the two apart.
	mutatedComment = "// TinyStore holds one record."
	mutatedTo      = "// TinyStore holds one record; amended by the mutation row."
)

// renderCapsule executes the HUMAN capsule renderer over every Section 17.3
// projection of a sealed, populated capsule and requires each one to carry its
// records.
//
// This is the wave-F gap. `context capsule` had only ever been rendered for a
// session with no capsule at all, so writeCapsulePage's populated branches --
// six of them, one per view -- had never run outside a unit fixture, and each
// one has an early return that prints "none on this page". A capsule that
// recorded four observations, seven files and a waiver but printed those lines
// is read by an operator as "nothing was found", which is the opposite of what
// the record says and is indistinguishable from a working empty session.
func renderCapsule(t *testing.T, s *sandbox, w walk) {
	t.Helper()
	witness := map[model.CapsuleView][]string{
		model.CapsuleViewAcceptedFacts:  {string(w.Accepted[0])},
		model.CapsuleViewRejectedFacts:  {string(w.Rejected[0])},
		model.CapsuleViewContradictions: {w.Contradictions[0].Note},
		model.CapsuleViewUnresolved:     {w.Unresolved[0].Note},
		model.CapsuleViewCoverage:       {string(w.Coverage[0].FileID), "waived"},
		model.CapsuleViewWaivers:        {string(w.Waivers[0].FileID), w.Waivers[0].Reason},
	}
	for _, view := range capsuleViews {
		rendered := []byte(s.runText(t, "context", "capsule", string(w.Session),
			"--actor", cliActor, "--view", string(view)))
		if bytes.Contains(rendered, []byte("none on this page")) {
			t.Errorf("the %s view of a populated capsule renders as empty:\n%s", view, rendered)
			continue
		}
		// The page has to identify the capsule it is a page of, or two views of
		// two capsules are indistinguishable to the operator reading them.
		for _, want := range append([]string{w.CapsuleHash}, witness[view]...) {
			if !bytes.Contains(rendered, []byte(want)) {
				t.Errorf("the %s view does not render %q:\n%s", view, want, rendered)
			}
		}
	}
}

// openRetainedSession opens a session over the file rows (b) and (c) mutate,
// reads it once and returns the session, that chunk and its receipt. The read
// happens before the mutation on purpose: what the rows are about is a chunk
// and a receipt that predate the activation.
func openRetainedSession(t *testing.T, s *sandbox) (model.SessionRequest, model.ReadChunkResponse, string) {
	t.Helper()
	a := cliAdapter{s: s}
	plan, _ := a.plan(t, model.PlanRequest{
		ActorID: retainActor,
		Context: model.ContextRequest{Task: "hold a pinned read across a refresh",
			Phase: model.PhaseSweep, Seeds: []string{retainSeedSymbol}},
	})
	session := model.SessionRequest{SessionID: plan.SessionID, ActorID: retainActor}
	files, _ := a.coverage(t, session)
	// The mutated file is identified by the bytes the session serves, not by a
	// file id written down here: the id is a per-generation identifier and
	// hard-coding one would tie this row to a fixture layout it does not own.
	for _, f := range requiredFiles(files) {
		chunk := a.read(t, model.ReadChunkRequest{SessionID: plan.SessionID,
			ActorID: retainActor, FileID: f.FileID})
		if bytes.HasPrefix([]byte(chunk.Content), []byte("package store")) {
			return session, chunk, chunk.Receipt
		}
	}
	t.Fatalf("the retained session pins no required file holding %s", mutatedFile)
	return model.SessionRequest{}, model.ReadChunkResponse{}, ""
}

// mutateStore appends one new symbol to the fixture's store file. It is the
// only write this package makes into the repository, and it is the workspace
// change rows (b) and (c) are about; nothing in codectx writes here.
func mutateStore(t *testing.T, repo string) {
	t.Helper()
	path := filepath.Join(repo, mutatedFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s to mutate it: %v", mutatedFile, err)
	}
	after := bytes.Replace(before, []byte(mutatedComment), []byte(mutatedTo), 1)
	if bytes.Equal(after, before) {
		t.Fatalf("%s does not carry %q, so the mutation would only append", mutatedFile, mutatedComment)
	}
	if err := os.WriteFile(path, append(after, mutationBody...), 0o600); err != nil {
		t.Fatalf("mutate %s: %v", mutatedFile, err)
	}
}

// TestE2EResourcesReportsAStoreWithNoLedger drives the most common state this
// code will ever be in and the one nothing exercised: a store whose run ledger
// is not there. A workspace indexed before the ledger existed, a first run
// after an upgrade and a store whose ledger a sweep removed all reach it, and
// every surface above ledger.OpenReader has an absent-ledger branch that no
// test ran.
//
// Two failures are guarded, and both are silent. The report must still be
// produced -- the resources block is the host's own measurements and does not
// depend on any run having been recorded, so an absent ledger must not fail the
// command. And it must render no run at all rather than a run of zeros: a row
// reading `0s`, `0` files and `0` units is a run that never happened, and an
// operator reading it cannot tell it from a run that did nothing.
//
// It is an end-to-end row rather than a unit one because the absent branch is
// in the composition -- the workspace's ledger adapter -- and only a real store
// with its ledger file removed puts it there.
//
// Mutation: in the workspace's run-ledger adapter, answer an unrecorded ledger
// with an empty model.RunRecord instead of none.
func TestE2EResourcesReportsAStoreWithNoLedger(t *testing.T) {
	s := newSandbox(t)
	indexEnv, indexCode := s.run(t, "index")
	if !indexEnv.OK || indexCode != 0 {
		t.Fatalf("index failed (exit %d): %+v", indexCode, indexEnv.Error)
	}

	// The ledger is removed the way a sweep or an older store leaves it: the
	// file is simply not there. Its sidecars go with it, since a stale
	// write-ahead log would describe a database that no longer exists.
	var removed int
	root := filepath.Join(s.Home, "data")
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "ledger.db") {
			return nil
		}
		removed++
		return os.Remove(path)
	}); err != nil {
		t.Fatalf("removing the run ledger: %v", err)
	}
	if removed == 0 {
		t.Fatalf("the index recorded no ledger under %s, so this row proves nothing about its absence", root)
	}

	// runText fails the test on any non-zero exit, which is the first
	// assertion: a store with no ledger still reports.
	report := s.runText(t, "status", "--resources")
	if !strings.Contains(report, "parent rss") {
		t.Fatalf("the resources block is missing from a report over a store with no ledger:\n%s", report)
	}
	if strings.Contains(report, "\nrun\n") {
		t.Fatalf("a store with no recorded run rendered a run table, which is a run that never happened:\n%s", report)
	}

	env, code := s.run(t, "status", "--resources")
	// The command's payload wraps the status under "index", beside the tool
	// report; only the status is asserted here.
	status := data[struct {
		Index model.IndexStatus `json:"index"`
	}](t, env, code).Index
	if status.Resources == nil {
		t.Fatal("the report carries no resources block, though the block measures the host and not any run")
	}
	if status.Resources.Run != nil {
		t.Fatalf("a store with no ledger answered with a run record: %+v", status.Resources.Run)
	}
	if len(status.Resources.Stages) != 0 {
		t.Fatalf("a store with no ledger answered with %d stages", len(status.Resources.Stages))
	}
}
