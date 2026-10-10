package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestMain makes this test binary the parser worker, as the codectx binary is
// in production: indexing parses in a child process of the running binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// The two versions of the one Go file the scenario reads. Alpha moves between
// them (three lines are inserted above it) and Beta exists only in the first.
const (
	sourceV1 = "package svc\n\n" +
		"// Alpha is long enough to need several small chunks.\n" +
		"func Alpha(n int) int {\n\ttotal := 0\n\tfor i := 0; i < n; i++ {\n\t\ttotal += i * 2\n\t}\n\treturn total\n}\n\n" +
		"// Beta is removed by the second generation.\n" +
		"func Beta() string {\n\treturn \"beta\"\n}\n"
	sourceV2 = "package svc\n\n" +
		"// Gamma pushes Alpha down the file.\n" +
		"func Gamma() int {\n\treturn 3\n}\n\n" +
		"// Alpha is long enough to need several small chunks.\n" +
		"func Alpha(n int) int {\n\ttotal := 0\n\tfor i := 0; i < n; i++ {\n\t\ttotal += i * 2\n\t}\n\treturn total\n}\n"
	// sourceWorktree is written after the second index and never indexed: a
	// read that returns any of it read the working tree.
	sourceWorktree = "package svc\n\nfunc Alpha(n int) int { return -1 }\n"
)

// latin1 is not valid UTF-8, so the planner must carry it as base64.
var latin1 = []byte("caf\xe9 cr\xe8me\n\xff\xfe end\n")

// TestReadOutsideASession proves the read outside a session end to end over a
// real workspace with two generations: what it reads, from which generation,
// how it pages, what it refuses, and that it touches no session.
func TestReadOutsideASession(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	userCfg := filepath.Join(home, "config", "codectx")
	for _, dir := range []string{filepath.Join(repo, "svc"), filepath.Join(repo, "data"), userCfg} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "[storage]\ndata_dir = \"" + filepath.Join(home, "data") + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = false\n[providers.lsp]\nenabled = false\n"
	writeFile(t, filepath.Join(userCfg, "config.toml"), []byte(cfg))
	writeFile(t, filepath.Join(repo, "go.mod"), []byte("module example.com/svc\n\ngo 1.27\n"))
	writeFile(t, filepath.Join(repo, "svc", "a.go"), []byte(sourceV1))
	writeFile(t, filepath.Join(repo, "data", "latin1.txt"), latin1)
	// Both exist on disk and neither is in the snapshot: one is outside the
	// tree, the other is a symbolic link, which capture skips by default.
	writeFile(t, filepath.Join(home, "outside.txt"), []byte("outside the tree\n"))
	if err := os.Symlink("a.go", filepath.Join(repo, "svc", "link.go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	ws, err := OpenWorkspace(ctx, repo, OpenOptions{Operation: "index"})
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	svc := ws.Services()
	first, err := svc.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("index the first generation: %v", err)
	}
	g1 := first.Binding.GenerationID
	alpha1 := functionNode(t, ws, g1, "Alpha")
	beta1 := functionNode(t, ws, g1, "Beta")
	wantAlpha1 := sourceV1[alpha1.Bytes.Start:alpha1.Bytes.End]

	// A node read returns exactly the node's bytes, whole, with the binding of
	// the generation asked for.
	whole := readOnce(t, svc, model.ReadChunkRequest{NodeID: alpha1.Node.ID, GenerationID: g1})
	if whole.Content != wantAlpha1 || whole.ByteRange != *alpha1.Bytes || whole.NextCursor != "" || whole.NextOffset != nil {
		t.Fatalf("node read = %q %+v cursor=%q, want exactly %q at %+v in one chunk",
			whole.Content, whole.ByteRange, whole.NextCursor, wantAlpha1, *alpha1.Bytes)
	}
	if whole.Binding.GenerationID != g1 || whole.NodeID != alpha1.Node.ID || whole.FileID != alpha1.Node.FileID {
		t.Fatalf("node read binding = %+v node %s file %s, want generation %d, node %s, file %s",
			whole.Binding, whole.NodeID, whole.FileID, g1, alpha1.Node.ID, alpha1.Node.FileID)
	}
	// A read outside a session carries no session block at all.
	if whole.Session != nil {
		t.Fatalf("a read outside a session carried a session block %+v", *whole.Session)
	}

	// A span longer than one chunk pages. The first page is taken now, while
	// the first generation is active; the rest are taken after a second
	// generation is published and a retention pass has run.
	const small = 16
	page := readOnce(t, svc, model.ReadChunkRequest{NodeID: alpha1.Node.ID, GenerationID: g1, MaxBytes: small})
	if page.NextCursor == "" {
		// Mutation caught: clip at one chunk -- a span longer than the chunk
		// ends after its first chunk with no way to continue.
		t.Fatalf("a %d-byte node read with %d-byte chunks returned no continuation", len(wantAlpha1), small)
	}
	pages := []model.ReadChunkResponse{page}

	writeFile(t, filepath.Join(repo, "svc", "a.go"), []byte(sourceV2))
	second, err := svc.Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("index the second generation: %v", err)
	}
	g2 := second.Binding.GenerationID
	if g2 == g1 {
		t.Fatalf("the second index published generation %d again", g2)
	}
	if _, err := ws.s.store.RetainByRef(ctx, ws.s.repo, sqlite.RetentionPolicy{RetainRefs: 1}, time.Now()); err != nil {
		t.Fatalf("retention pass: %v", err)
	}

	// While the cursor still retains the first generation, a node read naming
	// it reads that generation's range, not the active one's.
	alpha2 := functionNode(t, ws, g2, "Alpha")
	if alpha2.Node.ID != alpha1.Node.ID || *alpha2.Bytes == *alpha1.Bytes {
		t.Fatalf("fixture: Alpha must keep its id and move, got %s %+v then %s %+v",
			alpha1.Node.ID, *alpha1.Bytes, alpha2.Node.ID, *alpha2.Bytes)
	}
	old := readOnce(t, svc, model.ReadChunkRequest{NodeID: alpha1.Node.ID, GenerationID: g1})
	if old.ByteRange != *alpha1.Bytes || old.Content != wantAlpha1 || old.Binding.GenerationID != g1 {
		// Mutation caught: resolve the node in the active generation instead of
		// the asked one -- the range and bytes would be the second generation's.
		t.Fatalf("read of generation %d = %+v %q, want %+v %q", g1, old.ByteRange, old.Content, *alpha1.Bytes, wantAlpha1)
	}
	wantAlpha2 := sourceV2[alpha2.Bytes.Start:alpha2.Bytes.End]
	moved := readOnce(t, svc, model.ReadChunkRequest{NodeID: alpha2.Node.ID, GenerationID: g2})
	if moved.ByteRange != *alpha2.Bytes || moved.Content != wantAlpha2 || moved.Binding.GenerationID != g2 {
		t.Fatalf("read of generation %d = %+v %q, want %+v %q", g2, moved.ByteRange, moved.Content, *alpha2.Bytes, wantAlpha2)
	}

	// The paged read continues from the first generation past the retention
	// pass and ends at the node's end, with no gap and no overlap.
	for page.NextCursor != "" {
		page = readOnce(t, svc, model.ReadChunkRequest{Cursor: page.NextCursor, MaxBytes: small})
		pages = append(pages, page)
	}
	at := alpha1.Bytes.Start
	for i, p := range pages {
		if p.ByteRange.Start != at {
			t.Fatalf("page %d starts at %d, want %d: a gap or an overlap", i, p.ByteRange.Start, at)
		}
		if p.Binding.GenerationID != g1 || p.NodeID != alpha1.Node.ID {
			// Mutation caught: resolve the node in the active generation on a
			// continuation.
			t.Fatalf("page %d read generation %d node %s, want %d %s", i, p.Binding.GenerationID, p.NodeID, g1, alpha1.Node.ID)
		}
		at = p.ByteRange.End
	}
	if at != alpha1.Bytes.End {
		t.Fatalf("the pages end at %d, want the node's end %d", at, alpha1.Bytes.End)
	}
	if got := string(joined(t, pages)); got != wantAlpha1 {
		// Mutation caught: clip at one chunk.
		t.Fatalf("pages joined = %q, want the node's bytes %q", got, wantAlpha1)
	}
	// With its cursor's lease released by the last page, the same pass now
	// collects the first generation: the pass above was a real one, and only
	// the lease kept the generation for the pages after it.
	report, err := ws.s.store.RetainByRef(ctx, ws.s.repo, sqlite.RetentionPolicy{RetainRefs: 1}, time.Now())
	if err != nil {
		t.Fatalf("second retention pass: %v", err)
	}
	if !slices.Contains(report.GenerationsDeleted, int64(g1)) {
		t.Fatalf("retention kept generation %d after its last read ended (deleted %v); the paging proof proves nothing", g1, report.GenerationsDeleted)
	}

	// A node the asked generation does not hold is "not in this generation",
	// whether that generation is named or is the active one.
	for _, gen := range []model.GenerationID{g2, 0} {
		_, err := svc.Read(ctx, model.ReadChunkRequest{NodeID: beta1.Node.ID, GenerationID: gen})
		wantNotInGeneration(t, err, "a node deleted by the second generation")
	}

	// The working tree changes after the second index; reads still answer
	// from the content store of the generation.
	writeFile(t, filepath.Join(repo, "svc", "a.go"), []byte(sourceWorktree))
	fileRead := readAll(t, svc, model.ReadChunkRequest{Path: "svc/a.go"}, 0)
	if got := string(joined(t, fileRead)); got != sourceV2 {
		// Mutation caught: read the working tree.
		t.Fatalf("path read = %q, want the second generation's bytes %q", got, sourceV2)
	}
	if again := readOnce(t, svc, model.ReadChunkRequest{NodeID: alpha2.Node.ID}); again.Content != wantAlpha2 {
		// Mutation caught: read the working tree.
		t.Fatalf("node read after a working-tree edit = %q, want %q", again.Content, wantAlpha2)
	}

	// A file that is not valid UTF-8 comes back as base64 with byte ranges.
	raw := readAll(t, svc, model.ReadChunkRequest{Path: "data/latin1.txt"}, 0)
	if len(raw) != 1 || raw[0].Encoding != model.EncodingBase64 ||
		raw[0].ByteRange != (model.ByteRange{Start: 0, End: uint64(len(latin1))}) {
		t.Fatalf("latin-1 read = %d chunks, first %s %+v; want one base64 chunk over [0,%d)",
			len(raw), raw[0].Encoding, raw[0].ByteRange, len(latin1))
	}
	if got := joined(t, raw); !bytes.Equal(got, latin1) {
		t.Fatalf("latin-1 read decoded to %q, want %q", got, latin1)
	}

	// A path the snapshot does not hold is "not in this generation", though
	// the first two exist on disk: nothing is opened on a path.
	for _, path := range []string{"../outside.txt", "svc/link.go", "svc/missing.go"} {
		_, err := svc.Read(ctx, model.ReadChunkRequest{Path: path})
		// Mutation caught: read the working tree.
		wantNotInGeneration(t, err, path)
	}

	// A session over the active generation, to show that a read outside a
	// session changes nothing of it and that a session read still issues.
	actor := "source-read-actor"
	plan, _, err := svc.Plan(ctx, model.PlanRequest{ActorID: actor,
		Context: model.ContextRequest{Task: "read Alpha", Phase: model.PhaseSweep, Seeds: []string{"svc/a.go"}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	session := model.SessionRequest{SessionID: plan.SessionID, ActorID: actor}
	fileID := model.NewFileID(ws.s.repo, "svc/a.go")
	issuedBefore, coverageBefore := sessionState(t, ws, session)
	readAll(t, svc, model.ReadChunkRequest{Path: "svc/a.go"}, small)
	readAll(t, svc, model.ReadChunkRequest{FileID: fileID}, 0)
	readAll(t, svc, model.ReadChunkRequest{NodeID: alpha2.Node.ID}, small)
	issuedAfter, coverageAfter := sessionState(t, ws, session)
	if issuedAfter != issuedBefore || coverageAfter != coverageBefore {
		// Mutation caught: a read outside a session routed through the coverage
		// service, issuing a chunk or crediting coverage.
		t.Fatalf("reads outside the session changed it: issued %d -> %d, coverage %+v -> %+v",
			issuedBefore, issuedAfter, coverageBefore, coverageAfter)
	}

	// The session read is unchanged: it carries its session block, issues one
	// chunk and serves the session's pinned bytes.
	in := readOnce(t, svc, model.ReadChunkRequest{SessionID: plan.SessionID, ActorID: actor, FileID: fileID})
	if in.Session == nil || in.Session.Receipt == "" || !in.Session.Coverage.Valid() {
		t.Fatalf("a session read carried no receipt: %+v", in.Session)
	}
	if !strings.HasPrefix(sourceV2, in.Content) || in.ByteRange.Start != 0 || in.NodeID != "" {
		t.Fatalf("session read = %q %+v node %q, want a prefix of the pinned file from byte 0", in.Content, in.ByteRange, in.NodeID)
	}
	if issued, _ := sessionState(t, ws, session); issued != issuedAfter+1 {
		t.Fatalf("a session read issued %d chunks, want 1", issued-issuedAfter)
	}
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// functionNode is the one function node called name in gen, with its range.
func functionNode(t *testing.T, ws *Workspace, gen model.GenerationID, name string) sqlite.StoredNode {
	t.Helper()
	ctx := context.Background()
	reader, err := ws.s.store.PinGeneration(ctx, ws.s.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("pin generation %d: %v", gen, err)
	}
	defer reader.Close()
	nodes, err := reader.Nodes(ctx, sqlite.NodeFilter{Name: name, Kinds: []model.NodeKind{model.NodeFunction}}, "", model.MaxPageItems)
	if err != nil {
		t.Fatalf("look up %s in generation %d: %v", name, gen, err)
	}
	if len(nodes) != 1 || nodes[0].Bytes == nil {
		t.Fatalf("generation %d holds %d function nodes named %s, want exactly one with a range", gen, len(nodes), name)
	}
	return nodes[0]
}

func readOnce(t *testing.T, svc *Services, req model.ReadChunkRequest) model.ReadChunkResponse {
	t.Helper()
	resp, err := svc.Read(context.Background(), req)
	if err != nil {
		t.Fatalf("read %+v: %v", req, err)
	}
	return resp
}

// readAll reads a span to its end by its continuations.
func readAll(t *testing.T, svc *Services, req model.ReadChunkRequest, maxBytes uint32) []model.ReadChunkResponse {
	t.Helper()
	req.MaxBytes = maxBytes
	out := []model.ReadChunkResponse{readOnce(t, svc, req)}
	for out[len(out)-1].NextCursor != "" {
		out = append(out, readOnce(t, svc, model.ReadChunkRequest{Cursor: out[len(out)-1].NextCursor, MaxBytes: maxBytes}))
	}
	for _, c := range out {
		if c.Session != nil {
			t.Fatalf("a read outside a session carried a session block %+v", *c.Session)
		}
	}
	return out
}

// joined is the bytes the chunks carry, base64 chunks decoded.
func joined(t *testing.T, chunks []model.ReadChunkResponse) []byte {
	t.Helper()
	var b []byte
	for _, c := range chunks {
		if c.Encoding == model.EncodingBase64 {
			raw, err := base64.StdEncoding.DecodeString(c.Content)
			if err != nil {
				t.Fatalf("chunk %+v is not base64: %v", c.ByteRange, err)
			}
			b = append(b, raw...)
			continue
		}
		b = append(b, c.Content...)
	}
	return b
}

func wantNotInGeneration(t *testing.T, err error, what string) {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid || !strings.Contains(typed.Message, "not in this generation") {
		t.Fatalf("%s: got %v, want CTX_ARGUMENT_INVALID \"not in this generation\"", what, err)
	}
}

// sessionState is everything a read could change about a session: the chunks
// issued and not yet confirmed, and its coverage counts.
func sessionState(t *testing.T, ws *Workspace, s model.SessionRequest) (int64, sqlite.CoverageCounts) {
	t.Helper()
	ctx := context.Background()
	issued, err := ws.s.store.UnconfirmedChunks(ctx, s.SessionID)
	if err != nil {
		t.Fatalf("unconfirmed chunks: %v", err)
	}
	counts, err := ws.s.store.CoverageSummary(ctx, s.SessionID, s.ActorID)
	if err != nil {
		t.Fatalf("coverage summary: %v", err)
	}
	return issued, counts
}
