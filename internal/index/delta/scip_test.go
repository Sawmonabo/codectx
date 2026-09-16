package delta_test

import (
	"encoding/binary"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/delta"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
)

const (
	symFoo = "scip-go gomod example.com/mod . pkg/Foo()."
	symBar = "scip-go gomod example.com/mod . pkg/Bar()."
)

// TestSCIPRefresh drives the SCIP applier's refresh path against a real store
// with a synthetic index: one document is reparsed, one is not.
//
// Requirement (Section 11.4): a SCIP unit built as a delta holds exactly the
// facts a full import of the same index holds. It replaces the bucket of every
// document whose canonical hash moved and inherits the rest, and it never
// drops the unit's index-level bucket, because a delta run republishes the
// index's external symbols only for the documents it reparsed.
//
// Mutation that fails it: make changedDocuments name every document rather
// than the moved ones (drop the `if c.Class == scip.ClassUnchanged` guard in
// scip.go): the unchanged document's bucket is replaced, nothing republishes
// it, and the refreshed unit seals missing the facts the full import holds.
func TestSCIPRefresh(t *testing.T) {
	f := newFixture(t)
	const aOld = "package pkg\n\nfunc Foo() {}\n"
	const aNew = "package pkg\n\nfunc  Foo() {}\n"
	const bSrc = "package pkg\n\nfunc Bar() {}\n"
	inputs := []string{"index.scip", "pkg/a.go", "pkg/b.go"}
	scope := scip.ImportScope("index.scip")

	p, err := scip.New(f.ctx, scip.Options{Import: "index.scip", WorkDir: filepath.Join(f.dir, "scip")})
	if err != nil {
		t.Fatal(err)
	}
	a := delta.NewSCIP(f.store, p, limits, f.pool)
	if a.Kind() != delta.KindSCIP {
		t.Fatalf("applier kind %q, want %q", a.Kind(), delta.KindSCIP)
	}

	base := f.snapshot(map[string]string{
		"pkg/a.go": aOld, "pkg/b.go": bSrc,
		"index.scip": string(miniIndex("scip-fixture", "0.1.0",
			documentRecord("pkg/a.go", "go", 1, occurrenceRecord(symFoo, 1, 2, 5, 8)),
			documentRecord("pkg/b.go", "go", 1, occurrenceRecord(symBar, 1, 2, 5, 8)))),
	})
	prev := f.request(p, build{tree: base, gen: f.generation(base), cfgHash: "config-base",
		scopeKey: scope, paths: inputs})
	if res := applyWithin(t, a, f.ctx, prev, time.Minute); !res.Full || res.FullReason != delta.FullNoPredecessor {
		t.Fatalf("the first build is %+v, want a full build with no predecessor", res)
	}
	// The predecessor is committed before the run that carries from it, as it
	// is by the activation of the generation it belongs to.
	f.flush()

	// pkg/a.go is reformatted: its document moves, pkg/b.go's does not.
	fresh := f.snapshot(map[string]string{
		"pkg/a.go": aNew, "pkg/b.go": bSrc,
		"index.scip": string(miniIndex("scip-fixture", "0.1.0",
			documentRecord("pkg/a.go", "go", 1, occurrenceRecord(symFoo, 1, 2, 6, 9)),
			documentRecord("pkg/b.go", "go", 1, occurrenceRecord(symBar, 1, 2, 5, 8)))),
	})
	full := f.request(p, build{tree: fresh, gen: f.generation(fresh), cfgHash: "config-full",
		scopeKey: scope, paths: inputs})
	if out := applyWithin(t, a, f.ctx, full, time.Minute); !out.Full {
		t.Fatalf("the baseline import is %+v, want a full build", out)
	}

	refresh := f.request(p, build{tree: fresh, gen: f.generation(fresh), cfgHash: "config-refresh",
		scopeKey: scope, paths: inputs, previous: prev.Build.Spec.ID})
	out := applyWithin(t, a, f.ctx, refresh, time.Minute)
	if out.Full {
		t.Fatalf("the refresh is %+v, want a delta against the predecessor", out)
	}
	if !out.Filtered {
		t.Error("Filtered = false: a SCIP refresh with a predecessor always reparses only the moved documents")
	}
	if want := (delta.Stats{Changed: 1, Unchanged: 1}); out.Delta != want {
		t.Errorf("Delta = %+v, want %+v: one document moved and one did not", out.Delta, want)
	}
	if out.Carried.Nodes == 0 {
		t.Errorf("carried %+v: the unchanged document's facts are inherited, not re-imported", out.Carried)
	}
	f.flush()
	sameFacts(t, f.raw(), full.Build.Spec.ID, refresh.Build.Spec.ID)
}

// miniIndex, documentRecord and occurrenceRecord hand-encode a SCIP index.
// Only the fields the provider reads are written; the wire form is the one
// scip.proto defines.
func miniIndex(tool, version string, docs ...[]byte) []byte {
	meta := appendBytes(nil, 2, appendBytes(appendBytes(nil, 1, []byte(tool)), 2, []byte(version)))
	out := appendBytes(nil, 1, meta)
	for _, d := range docs {
		out = appendBytes(out, 2, d)
	}
	return out
}

func documentRecord(path, language string, encoding uint64, occurrences ...[]byte) []byte {
	d := appendBytes(nil, 1, []byte(path))
	d = appendBytes(d, 4, []byte(language))
	for _, o := range occurrences {
		d = appendBytes(d, 2, o)
	}
	return appendVarint(d, 6<<3|0, encoding)
}

func occurrenceRecord(symbol string, roles uint64, vals ...int32) []byte {
	o := appendBytes(appendBytes(nil, 1, packed(vals...)), 2, []byte(symbol))
	if roles != 0 {
		o = appendVarint(o, 3<<3|0, roles)
	}
	return o
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
