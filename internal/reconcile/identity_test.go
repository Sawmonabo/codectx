package reconcile_test

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/reconcile"
)

// TestDuplicateIdentityFollowsContentNotOrder protects ADR-0014's rule for
// exact duplicates (a Python `def f` written twice at one level): each is
// matched by its content, so an edit to one, or a copy inserted before them,
// leaves every other duplicate's disambiguator, and so its node, where it was;
// two declarations with one range are one entity.
//
// Mutations it catches: disambiguate duplicates by their ordinal (inserting a
// copy at the top renumbers the rest); key every duplicate by content,
// the first included (a body edit to the first moves it); give two
// declarations sharing one range two identities (C's `int x, x;`).
func TestDuplicateIdentityFollowsContentNotOrder(t *testing.T) {
	f := func(start uint64, body string) reconcile.Declared {
		return reconcile.Declared{Kind: model.NodeFunction, Descriptor: "m.f", Start: start, End: start + uint64(len(body)),
			Content: reconcile.ContentDigest([]byte(body))}
	}
	before := reconcile.SettleDuplicates([]reconcile.Declared{f(0, "def f(): return 1"), f(40, "def f(): return 2")})
	if before[0] != "" || before[1] == "" || before[0] == before[1] {
		t.Fatalf("two duplicates = %q, want the first plain and the second keyed by its content", before)
	}

	// The first is edited (its length changes); the second's identity holds.
	edited := reconcile.SettleDuplicates([]reconcile.Declared{f(0, "def f(): return 100"), f(42, "def f(): return 2")})
	if edited[0] != before[0] || edited[1] != before[1] {
		t.Fatalf("after a body edit to the first duplicate = %q, want %q", edited, before)
	}

	// A third copy appended after them is keyed by its own content.
	appended := reconcile.SettleDuplicates([]reconcile.Declared{f(0, "def f(): return 1"), f(40, "def f(): return 2"),
		f(80, "def f(): return 3")})
	if appended[0] != before[0] || appended[1] != before[1] || appended[2] == before[1] || appended[2] == "" {
		t.Fatalf("after appending a duplicate = %q, want the first two unchanged (%q) and a third of its own", appended, before)
	}

	// Two copies of the same bytes at different ranges are two entities;
	// two declarations with one range are one.
	copies := reconcile.SettleDuplicates([]reconcile.Declared{f(0, "def f(): pass"), f(20, "def f(): pass"),
		f(40, "def f(): pass"), f(40, "def f(): pass")})
	if copies[0] == copies[1] || copies[1] == copies[2] || copies[0] == copies[2] || copies[2] != copies[3] {
		t.Fatalf("copies = %q, want three distinct identities and the shared range settled once", copies)
	}

	// A declaration without a descriptor is file-scoped and untouched.
	scoped := reconcile.SettleDuplicates([]reconcile.Declared{{Kind: model.NodeFunction, Disambiguator: "x"},
		{Kind: model.NodeFunction, Disambiguator: "x"}})
	if scoped[0] != "x" || scoped[1] != "x" {
		t.Fatalf("file-scoped declarations = %q, want their disambiguators unchanged", scoped)
	}
}

// TestDeclarationKeyIgnoresRange protects ADR-0014 decision 1: a declaration
// with a descriptor path keys on file, kind, language, descriptor and
// disambiguator, never its byte range, while a file-scoped candidate (no
// descriptor) still keys on its range.
//
// Mutations it catches: keep the range in a descriptor key (a length-changing
// edit moves the node); drop the disambiguator (two overloads merge); drop the
// range from a file-scoped key (two locals of one name merge).
func TestDeclarationKeyIgnoresRange(t *testing.T) {
	file := model.FileID(model.H("file", "a.java"))
	at := func(start, end uint64) *model.SourceRange {
		return &model.SourceRange{Start: model.Position{Byte: start, Line: 1}, End: model.Position{Byte: end, Line: 1}}
	}
	decl := func(r *model.SourceRange, dis string) model.NodeCandidate {
		return model.NodeCandidate{ProviderID: "p", ScopeKey: "file:a.java", NativeKey: "A.parse", Kind: model.NodeMethod,
			Language: "java", Name: "parse", QualifiedName: "A.parse", FileID: file, Range: r, Descriptor: "A.parse", Disambiguator: dis}
	}
	k1, basis := reconcile.CanonicalKey(decl(at(10, 40), "params=(String)"))
	k2, _ := reconcile.CanonicalKey(decl(at(90, 140), "params=(String)"))
	if k1 != k2 || basis != model.MatchQualifiedSignature {
		t.Fatalf("one declaration at two ranges keys %s and %s (basis %s), want one key", k1, k2, basis)
	}
	if k3, _ := reconcile.CanonicalKey(decl(at(10, 40), "params=(int)")); k3 == k1 {
		t.Fatal("two overloads share a key")
	}
	local := func(r *model.SourceRange) model.NodeCandidate {
		c := decl(r, "")
		c.Kind, c.Descriptor = model.NodeVariable, ""
		return c
	}
	l1, _ := reconcile.CanonicalKey(local(at(10, 11)))
	l2, _ := reconcile.CanonicalKey(local(at(20, 21)))
	if l1 == l2 {
		t.Fatal("two file-scoped candidates at different ranges share a key")
	}
}
