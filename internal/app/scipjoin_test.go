package app

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The call-site join of Section 11.3, end to end: a call site the structural
// provider and the call linker publish and a SCIP reference occurrence at
// exactly the callee token's bytes give the caller a `calls` relation to the
// compiler-resolved symbol, with the compiler evidence row at the call-site
// key, while the linker's own row stays.

// joinCase is one language's file: run calls helper once, and the supplied
// index defines both and references helper at the call.
type joinCase struct {
	path, language, source string
	run, helper            string
	// enclosing gives run's definition its enclosing range. scip-clang emits
	// none, so the C++ case has none: its caller can only come from the
	// structural side.
	enclosing bool
}

var joinCases = []joinCase{
	{path: "g/g.go", language: "go", source: "package g\n\nfunc helper() {}\n\nfunc run() { helper() }\n",
		run: "scip-go gomod example.com/m v0 g/run().", helper: "scip-go gomod example.com/m v0 g/helper().", enclosing: true},
	{path: "web/b.ts", language: "typescript", source: "export function helper() {}\n\nexport function run() { helper(); }\n",
		run: "scip-typescript npm web 1.0.0 `b.ts`/run().", helper: "scip-typescript npm web 1.0.0 `b.ts`/helper().", enclosing: true},
	{path: "py/c.py", language: "python", source: "def helper():\n    pass\n\n\ndef run():\n    helper()\n",
		run: "scip-python python py 0.1 c/run().", helper: "scip-python python py 0.1 c/helper().", enclosing: true},
	{path: "java/B.java", language: "java", source: "class B {\n  static void helper() {}\n  static void run() { helper(); }\n}\n",
		run: "semanticdb maven . . B#run().", helper: "semanticdb maven . . B#helper().", enclosing: true},
	{path: "rs/src/lib.rs", language: "rust", source: "pub fn helper() {}\n\npub fn run() { helper(); }\n",
		run: "rust-analyzer cargo rs 0.1.0 run().", helper: "rust-analyzer cargo rs 0.1.0 helper().", enclosing: true},
	{path: "cc/a.cpp", language: "cpp", source: "void helper() {}\n\nvoid run() { helper(); }\n",
		run: "cxx . . $ run(49f6e7a06ebc5aa8).", helper: "cxx . . $ helper(49f6e7a06ebc5aa8)."},
}

// TestASCIPOccurrenceAtACallSiteGivesTheCallItsCompilerTarget indexes one
// file per language with an index that resolves the call, and asks the
// product for the callees of the structural caller. The answer must hold a
// `calls` relation to the node the index resolved the occurrence to, with a
// compiler-precision evidence row keyed by the call site.
//
// Mutation caught: a join that publishes the call-site key and nothing that
// reads it (the reference stays a `references` edge from the index's own
// enclosing node, and `calls` holds the linker's target alone).
func TestASCIPOccurrenceAtACallSiteGivesTheCallItsCompilerTarget(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	userCfg := filepath.Join(home, "config", "codectx")
	files := map[string]string{
		"go.mod":           "module example.com/m\n\ngo 1.27\n",
		"web/package.json": "{\"name\":\"web\",\"version\":\"1.0.0\"}\n",
		"rs/Cargo.toml":    "[package]\nname = \"rs\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
	}
	var docs [][]byte
	for _, c := range joinCases {
		files[c.path] = c.source
		docs = append(docs, c.document(t))
	}
	files["index.scip"] = string(joinIndex(docs...))
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, path), []byte(body))
	}
	if err := os.MkdirAll(userCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[storage]\ndata_dir = \"" + filepath.Join(home, "data") + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = true\n[providers.lsp]\nenabled = false\n"
	writeFile(t, filepath.Join(userCfg, "config.toml"), []byte(cfg))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	ws, err := OpenWorkspace(ctx, repo, OpenOptions{Operation: "index", SCIPImport: "index.scip"})
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	result, err := ws.Services().Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	gen := result.Binding.GenerationID
	reader, err := ws.s.store.PinGeneration(ctx, ws.s.repo, gen, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	for _, c := range joinCases {
		t.Run(c.language, func(t *testing.T) {
			caller := providerNode(t, reader, c.path, "run", tslang.ProviderID)
			target := providerNode(t, reader, c.path, "helper", scip.ID)
			start, end := token(c.source, "helper", 1)
			key := "callsite:" + c.path + ":" + strconv.Itoa(start+1) + "-" + strconv.Itoa(end)
			answer, err := ws.Services().Graph(ctx, model.GraphRequest{GenerationID: gen, Start: []model.NodeID{caller},
				Relations: []model.RelationKind{model.RelCalls}, Direction: model.DirectionOutgoing, MaxDepth: 1})
			if err != nil {
				t.Fatalf("callees of run: %v", err)
			}
			var to []string
			for _, rel := range answer.Relations {
				to = append(to, string(rel.To))
				if rel.To != target || rel.Kind != model.RelCalls {
					continue
				}
				rows, err := reader.Evidence(ctx, "", rel.ID, "", model.MaxPageItems)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					if r.Evidence.Precision == model.PrecisionCompiler && r.Evidence.NativeKey == key {
						return
					}
				}
				t.Fatalf("run calls the index's helper %s with no compiler evidence row keyed %s: %+v", target, key, rows)
			}
			t.Fatalf("the callees of run (%s) are %v; none is the index's helper %s at %s", caller, to, target, key)
		})
	}
}

// providerNode is the one node called name in path that provider publishes
// evidence for.
func providerNode(t *testing.T, reader *sqlite.PinnedReader, path, name, provider string) model.NodeID {
	t.Helper()
	ctx := context.Background()
	nodes, err := reader.Nodes(ctx, sqlite.NodeFilter{Name: name}, "", model.MaxPageItems)
	if err != nil {
		t.Fatal(err)
	}
	var found []model.NodeID
	for _, n := range nodes {
		if n.Path != path {
			continue
		}
		rows, err := reader.Evidence(ctx, n.Node.ID, "", "", model.MaxPageItems)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.Evidence.ProviderID == provider {
				found = append(found, n.Node.ID)
				break
			}
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d nodes named %s in %s carry %s evidence, want one", len(found), name, path, provider)
	}
	return found[0]
}

// token is the half-open byte range of the n-th (0-based) occurrence of word
// in src.
func token(src, word string, n int) (int, int) {
	at := -1
	for range n + 1 {
		i := strings.Index(src[at+1:], word)
		if i < 0 {
			panic(word + " does not occur often enough")
		}
		at += 1 + i
	}
	return at, at + len(word)
}

// scipRange is the SCIP [line, start column, end column] of the half-open
// byte range [start,end) on one line of src, columns in UTF-8 bytes.
func scipRange(src string, start, end int) []int32 {
	line := int32(strings.Count(src[:start], "\n"))
	col := start - (strings.LastIndexByte(src[:start], '\n') + 1)
	return []int32{line, int32(col), int32(col + end - start)}
}

// document is c's SCIP document: run and helper defined, helper referenced
// at the call.
func (c joinCase) document(t *testing.T) []byte {
	t.Helper()
	hs, he := token(c.source, "helper", 0)
	rs, re := token(c.source, "run", 0)
	cs, ce := token(c.source, "helper", 1)
	runDef := scipOccurrence(c.run, 1, scipRange(c.source, rs, re), nil)
	if c.enclosing {
		// From the definition's line to the end of the file.
		line := int32(strings.Count(c.source[:rs], "\n"))
		last := int32(strings.Count(c.source, "\n"))
		runDef = scipOccurrence(c.run, 1, scipRange(c.source, rs, re), []int32{line, 0, last, 0})
	}
	d := scipBytes(nil, 1, []byte(c.path))
	d = scipBytes(d, 4, []byte(c.language))
	for _, o := range [][]byte{scipOccurrence(c.helper, 1, scipRange(c.source, hs, he), nil), runDef,
		scipOccurrence(c.helper, 0, scipRange(c.source, cs, ce), nil)} {
		d = scipBytes(d, 2, o)
	}
	// position_encoding UTF-8: the columns above are bytes.
	return scipVarint(d, 6, 1)
}

func scipOccurrence(symbol string, roles uint64, rng, enclosing []int32) []byte {
	o := scipBytes(scipBytes(nil, 1, scipPacked(rng)), 2, []byte(symbol))
	if roles != 0 {
		o = scipVarint(o, 3, roles)
	}
	if enclosing != nil {
		o = scipBytes(o, 7, scipPacked(enclosing))
	}
	return o
}

// joinIndex hand-encodes the SCIP index (scip.proto): metadata naming a tool,
// then the documents.
func joinIndex(docs ...[]byte) []byte {
	meta := scipBytes(nil, 2, scipBytes(scipBytes(nil, 1, []byte("scip-fixture")), 2, []byte("0.1.0")))
	out := scipBytes(nil, 1, meta)
	for _, d := range docs {
		out = scipBytes(out, 2, d)
	}
	return out
}

func scipPacked(vals []int32) []byte {
	var out []byte
	for _, v := range vals {
		out = binary.AppendUvarint(out, uint64(v))
	}
	return out
}

func scipVarint(b []byte, field int, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(b, uint64(field)<<3), v)
}

func scipBytes(b []byte, field int, payload []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}
