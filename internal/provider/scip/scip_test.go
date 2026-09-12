package scip_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
)

// The canonical fixture was encoded with the real scip Go bindings
// (github.com/scip-code/scip/bindings/go/scip v0.10.0) by a throwaway
// generator; only the bytes are committed (ruling R9-2). It holds a Go
// document (UTF-8, a multi-byte rune before the definitions), a TypeScript
// document (UTF-16, a surrogate pair before every definition), a Python
// document (UTF-32, a non-BMP rune before the definition), a forward
// reference to a symbol defined in a later document, an external symbol
// known only from external_symbols, the same edge occurring at two distinct
// ranges, `local 0` in two documents, an import occurrence and an
// implementation relationship. index-stale.scip embeds a.go text one byte
// different from the captured file.
const (
	symFoo     = "scip-go gomod example.com/mod . pkg/Foo()."
	symBar     = "scip-go gomod example.com/mod . pkg/Bar()."
	symPrintln = "scip-go gomod github.com/golang/go/std . fmt/Println()."
	symFmt     = "scip-go gomod github.com/golang/go/std . fmt/"
	symBaz     = "scip-typescript npm web 1.0.0 b/Baz()."
	symI       = "scip-typescript npm web 1.0.0 b/I#"
	symC       = "scip-typescript npm web 1.0.0 b/C#"
	symQux     = "scip-python pypi py 0.1 c/qux."
)

var sourcePaths = []string{"pkg/a.go", "pkg/d.go", "web/b.ts", "py/c.py"}

func fixture(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, p := range sourcePaths {
		files[p] = readFixture(t, filepath.Join("src", filepath.FromSlash(p)))
	}
	files["index.scip"] = readFixture(t, "index.scip")
	files["index-stale.scip"] = readFixture(t, "index-stale.scip")
	return files
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func newProvider(t *testing.T, importPath string) *scip.Provider {
	t.Helper()
	p, err := scip.New(scip.Options{Import: importPath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// facts records every fact a run persisted, so the test can check the exact
// bytes each identity claims.
type facts struct {
	provider.UnitOutput
	mu        sync.Mutex
	nodes     map[model.NodeID]model.NodeFact
	relations map[model.RelationID]model.RelationFact
}

func (f *facts) PutNodes(ctx context.Context, list []model.NodeFact) error {
	f.mu.Lock()
	for _, n := range list {
		f.nodes[n.Node.ID] = n
	}
	f.mu.Unlock()
	return f.UnitOutput.PutNodes(ctx, list)
}

func (f *facts) PutRelations(ctx context.Context, list []model.RelationFact) error {
	f.mu.Lock()
	for _, r := range list {
		f.relations[r.Relation.ID] = r
	}
	f.mu.Unlock()
	return f.UnitOutput.PutRelations(ctx, list)
}

// byNativeKey finds the one node whose evidence carries the SCIP symbol in
// the given file (locals recur per document).
func (f *facts) byNativeKey(t *testing.T, key string, file model.FileID) model.NodeFact {
	t.Helper()
	var out []model.NodeFact
	for _, n := range f.nodes {
		for _, e := range n.Evidence {
			if e.NativeKey == key && (file == "" || e.FileID == file) {
				out = append(out, n)
				break
			}
		}
	}
	if len(out) != 1 {
		t.Fatalf("want exactly one node for %q in %q, got %d", key, file, len(out))
	}
	return out[0]
}

func (f *facts) edge(t *testing.T, from model.NodeID, kind model.RelationKind, to model.NodeID) model.RelationFact {
	t.Helper()
	for _, r := range f.relations {
		if r.Relation.From == from && r.Relation.Kind == kind && r.Relation.To == to {
			return r
		}
	}
	t.Fatalf("no %s edge %s -> %s among %d relations", kind, from, to, len(f.relations))
	return model.RelationFact{}
}

func run(t *testing.T, h *providertest.Harness, p *scip.Provider, scope string, inputs []string) (*facts, model.ProviderResult) {
	t.Helper()
	u := h.Plan(t, p, scope, inputs)
	rec := &facts{UnitOutput: h.Begin(t, u, inputs), nodes: map[model.NodeID]model.NodeFact{}, relations: map[model.RelationID]model.RelationFact{}}
	result, err := provider.RunUnit(context.Background(), p, u.Request, rec, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit: %v", err)
	}
	if state, ok := h.UnitState(t, u.Build.Spec.ID); !ok || state != model.UnitSealed {
		t.Fatalf("unit state = %q (exists %v), want sealed", state, ok)
	}
	return rec, result
}

// wantRange asserts that a node's definition range is exactly the bytes of
// needle's nth occurrence in the source: a range that disagrees attributes a
// compiler fact to the wrong source bytes, which nothing downstream can
// detect.
func wantRange(t *testing.T, n model.NodeFact, src, needle string, nth int) {
	t.Helper()
	off := -1
	for i := 0; i <= nth; i++ {
		next := strings.Index(src[off+1:], needle)
		if next < 0 {
			t.Fatalf("fixture lacks occurrence %d of %q", nth, needle)
		}
		off += 1 + next
	}
	if n.Node.Range == nil || n.Node.Range.Start.Byte != uint64(off) || n.Node.Range.End.Byte != uint64(off+len(needle)) {
		t.Fatalf("node %s range = %+v, want bytes [%d,%d) of %q", n.Node.Name, n.Node.Range, off, off+len(needle), needle)
	}
}

// TestCanonicalFixture is the single conformance and fact-set check of the
// SCIP provider. Failure modes: identities that depend on order (Conform);
// UTF-8/16/32 coordinates converted to the wrong bytes (wrong source served
// as compiler evidence); a forward or external reference minted as a
// different identity than its definition (a broken graph); two occurrences
// of one edge collapsed to one range or two document-local symbols merged
// (lost or wrong facts); and a supplied index whose embedded text is not the
// captured file admitted as exact-source evidence (false readiness).
func TestCanonicalFixture(t *testing.T) {
	files := fixture(t)
	inputs := append([]string{"index.scip"}, sourcePaths...)
	p := newProvider(t, "index.scip")
	providertest.Conform(t, p, files, scip.ImportScope("index.scip"), inputs)

	h := providertest.New(t, files)
	ctx := context.Background()
	if b, err := p.Verify(ctx, h.View, scip.ImportScope("index.scip")); err != nil || b != model.SourceBindingVerified {
		t.Fatalf("Verify(index.scip) = %q, %v; want verified", b, err)
	}
	got, result := run(t, h, p, scip.ImportScope("index.scip"), inputs)
	for _, c := range result.Capabilities {
		if c.State != model.CapabilityFresh || c.DiagnosticCode != "" {
			t.Fatalf("capability %s = %s (%s), want fresh over a verified index", c.Capability, c.State, c.DiagnosticCode)
		}
	}
	a, d, b, c := h.File(t, "pkg/a.go"), h.File(t, "pkg/d.go"), h.File(t, "web/b.ts"), h.File(t, "py/c.py")

	// Definition ranges in every position encoding land on the exact bytes.
	foo := got.byNativeKey(t, symFoo, a.ID)
	wantRange(t, foo, files["pkg/a.go"], "Foo", 0)
	bar := got.byNativeKey(t, symBar, d.ID)
	wantRange(t, bar, files["pkg/d.go"], "Bar", 0)
	wantRange(t, got.byNativeKey(t, symBaz, b.ID), files["web/b.ts"], "Baz", 0)
	wantRange(t, got.byNativeKey(t, symQux, c.ID), files["py/c.py"], "qux", 0)
	if foo.Node.Kind != model.NodeFunction || foo.Node.Signature != "func Foo()" || foo.Node.QualifiedName != "pkg/Foo()." {
		t.Fatalf("Foo node = %+v, want a function with its SCIP signature and descriptor name", foo.Node)
	}

	// The forward reference Foo -> Bar resolves to the identity d.go minted,
	// with one evidence row per distinct occurrence range.
	refs := got.edge(t, foo.Node.ID, model.RelReferences, bar.Node.ID)
	if len(refs.Evidence) != 2 {
		t.Fatalf("Foo -> Bar has %d occurrences, want 2 distinct ranges", len(refs.Evidence))
	}
	first, second := strings.Index(files["pkg/a.go"], "Bar("), strings.LastIndex(files["pkg/a.go"], "Bar(")
	seen := map[uint64]bool{}
	for _, e := range refs.Evidence {
		if e.FileID != a.ID || e.Range == nil || e.Range.End.Byte-e.Range.Start.Byte != 3 {
			t.Fatalf("occurrence evidence %+v does not point at a 3-byte range in a.go", e)
		}
		seen[e.Range.Start.Byte] = true
	}
	if !seen[uint64(first)] || !seen[uint64(second)] {
		t.Fatalf("occurrence starts %v, want both %d and %d", seen, first, second)
	}

	// The external symbol exists once, from external_symbols, and is the
	// target of the reference inside Foo.
	println := got.byNativeKey(t, symPrintln, "")
	if println.Node.Kind != model.NodeFunction || println.Node.Range != nil || !strings.Contains(string(println.Node.Metadata), "scip_external") {
		t.Fatalf("external Println node = %+v, want an unlocated external function", println.Node)
	}
	got.edge(t, foo.Node.ID, model.RelReferences, println.Node.ID)

	// Locals are document scoped: `local 0` in a.go and d.go are two nodes.
	xa, xd := got.byNativeKey(t, "local 0", a.ID), got.byNativeKey(t, "local 0", d.ID)
	if xa.Node.ID == xd.Node.ID {
		t.Fatal("local 0 of two documents merged into one identity")
	}
	wantRange(t, xa, files["pkg/a.go"], "x", 0)
	got.edge(t, foo.Node.ID, model.RelReads, xa.Node.ID)

	// Import and implementation relationships.
	fileA := got.byNativeKey(t, "file:pkg/a.go", a.ID)
	got.edge(t, fileA.Node.ID, model.RelImports, got.byNativeKey(t, symFmt, "").Node.ID)
	got.edge(t, got.byNativeKey(t, symC, b.ID).Node.ID, model.RelImplements, got.byNativeKey(t, symI, b.ID).Node.ID)

	// A supplied index whose embedded text is not the captured file is never
	// exact: Verify says unverified, the run reports every capability partial
	// with CTX_SOURCE_BINDING_UNVERIFIED and every node says so.
	stale := newProvider(t, "index-stale.scip")
	staleInputs := append([]string{"index-stale.scip"}, sourcePaths...)
	if b, err := stale.Verify(ctx, h.View, scip.ImportScope("index-stale.scip")); err != nil || b != model.SourceBindingUnverified {
		t.Fatalf("Verify(index-stale.scip) = %q, %v; want unverified", b, err)
	}
	h2 := providertest.New(t, files)
	got2, result2 := run(t, h2, stale, scip.ImportScope("index-stale.scip"), staleInputs)
	if len(result2.Capabilities) == 0 {
		t.Fatal("stale run reported no capability states")
	}
	for _, c := range result2.Capabilities {
		if c.State != model.CapabilityPartial || c.DiagnosticCode != model.CodeSourceBindingUnverified {
			t.Fatalf("capability %s = %s (%s) over a stale index, want partial CTX_SOURCE_BINDING_UNVERIFIED", c.Capability, c.State, c.DiagnosticCode)
		}
	}
	for _, n := range got2.nodes {
		var meta map[string]any
		if err := json.Unmarshal(n.Node.Metadata, &meta); err != nil || meta["source_binding"] != string(model.SourceBindingUnverified) {
			t.Fatalf("node %s metadata %s does not declare the unverified binding", n.Node.Name, n.Node.Metadata)
		}
	}
}

// TestStreamedLargeDocumentBoundedHeap protects the Section 11.4 memory
// contract: a single Document record far larger than any record bound (a
// text field of tens of MiB plus thousands of occurrences) is walked
// incrementally, so the heap grows by a bounded amount rather than by the
// document's size, and every byte is still processed. Failure mode: a
// decoder that materializes a document (or the index) would exhaust memory
// on a real index and lose or truncate facts.
func TestStreamedLargeDocumentBoundedHeap(t *testing.T) {
	const textBytes = 24 << 20
	const occurrences = 20000
	src := "package big\n\nfunc F() { G() }\n"
	index := bigIndex(textBytes, occurrences)
	indexLen := len(index)
	files := map[string]string{"big.go": src, "index.scip": string(index)}
	index = nil
	h := providertest.New(t, files)
	files = nil
	p := newProvider(t, "index.scip")

	// The live heap is sampled for the duration of the run and the peak is
	// kept: a before/after pair proves nothing, because a buffer that held the
	// whole document is already garbage by the time the run returns. Sampling
	// every 5 ms is often enough to catch a peak that lasts as long as a
	// document walk and rare enough that the stop-the-world of ReadMemStats
	// does not distort the run. TotalAlloc is deliberately not used: it counts
	// bytes allocated over time, which a correct streaming decoder also grows.
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	peak.Store(base.HeapAlloc)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&m)
				for seen := peak.Load(); m.HeapAlloc > seen; seen = peak.Load() {
					if peak.CompareAndSwap(seen, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	got, result := run(t, h, p, scip.ImportScope("index.scip"), []string{"index.scip", "big.go"})
	close(done)
	<-stopped

	if result.BytesProcessed != uint64(2*indexLen) {
		t.Fatalf("processed %d index bytes, want %d (two full streaming passes); bytes were skipped", result.BytesProcessed, 2*indexLen)
	}
	if len(result.Capabilities) == 0 {
		t.Fatal("the run reported no capability states")
	}
	if result.Capabilities[0].DiagnosticCode != model.CodeSourceBindingUnverified {
		t.Fatalf("a document whose text is not the captured file must be unverified, got %+v", result.Capabilities[0])
	}
	if len(got.nodes) < 2 || len(got.relations) < 1 {
		t.Fatalf("large document yielded %d nodes and %d relations; occurrences were lost", len(got.nodes), len(got.relations))
	}
	growth := int64(peak.Load()) - int64(base.HeapAlloc)
	if growth > textBytes/2 {
		t.Fatalf("live heap peaked %d bytes above the baseline while walking a %d-byte document; the document was materialized", growth, textBytes)
	}
	t.Logf("peak live heap %d bytes above baseline for a %d-byte document with %d occurrences", growth, textBytes, occurrences)
}

// bigIndex hand-encodes one SCIP index: metadata, then a single document
// whose text field is textBytes long and which carries one definition and
// n references, all in UTF-8 positions.
func bigIndex(textBytes, n int) []byte {
	var doc []byte
	doc = appendBytes(doc, 1, []byte("big.go"))
	doc = appendBytes(doc, 4, []byte("go"))
	def := appendBytes(appendBytes(nil, 1, packed(2, 5, 6)), 2, []byte("scip-go gomod example.com/big . big/F()."))
	def = appendVarint(def, 3<<3|0, 1)
	doc = appendBytes(doc, 2, def)
	ref := appendBytes(appendBytes(nil, 1, packed(2, 11, 12)), 2, []byte("scip-go gomod example.com/big . big/G()."))
	for i := 0; i < n; i++ {
		doc = appendBytes(doc, 2, ref)
	}
	doc = appendBytes(doc, 3, appendVarint(appendBytes(nil, 1, []byte("scip-go gomod example.com/big . big/F().")), 5<<3|0, 17))
	doc = appendBytes(doc, 5, []byte(strings.Repeat("x", textBytes)))
	doc = appendVarint(doc, 6<<3|0, 1)
	meta := appendBytes(nil, 2, appendBytes(appendBytes(nil, 1, []byte("fixture")), 2, []byte("0")))
	return appendBytes(appendBytes(nil, 1, meta), 2, doc)
}

func packed(vals ...int32) []byte {
	var out []byte
	for _, v := range vals {
		out = binary.AppendUvarint(out, uint64(v))
	}
	return out
}

func appendVarint(b []byte, tag uint64, v uint64) []byte {
	return binary.AppendUvarint(binary.AppendUvarint(b, tag), v)
}

func appendBytes(b []byte, field int, payload []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}
