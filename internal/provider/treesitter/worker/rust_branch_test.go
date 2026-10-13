package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// rustDecls runs the worker over the Rust source src and answers the
// declarations it sends, in order.
func rustDecls(t *testing.T, src string) []wire.Decl {
	t.Helper()
	decls, _ := rustFacts(t, src)
	return decls
}

// rustFacts runs the worker over the Rust source src and answers the
// declarations and references it sends, in order.
func rustFacts(t *testing.T, src string) ([]wire.Decl, []wire.Ref) {
	t.Helper()
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	if _, fail, err := w.parse(&out, wire.Request{Language: "rust", Path: "lib.rs", SourceBytes: uint64(len(src))}, []byte(src)); err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	var (
		decls []wire.Decl
		refs  []wire.Ref
	)
	for {
		kind, payload, err := wire.ReadMessage(&out, 0)
		if errors.Is(err, io.EOF) {
			return decls, refs
		}
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case wire.KindDecl:
			var d wire.Decl
			if err := json.Unmarshal(payload, &d); err != nil {
				t.Fatal(err)
			}
			decls = append(decls, d)
		case wire.KindRef:
			var r wire.Ref
			if err := json.Unmarshal(payload, &r); err != nil {
				t.Fatal(err)
			}
			refs = append(refs, r)
		}
	}
}

// branchesOf is the Branch of every declaration named name, in source
// order.
func branchesOf(decls []wire.Decl, name string) []string {
	var out []string
	for _, d := range decls {
		if d.Name == name {
			out = append(out, d.Branch)
		}
	}
	return out
}

// wantBranches fails the test unless the declarations named name have
// exactly the branches want, in source order.
func wantBranches(t *testing.T, decls []wire.Decl, name string, want ...string) {
	t.Helper()
	got := branchesOf(decls, name)
	if len(got) != len(want) {
		t.Fatalf("%d declarations of %s (branches %q), want %d (%q)", len(got), name, got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("declaration %d of %s has branch %q, want %q", i, name, got[i], want[i])
		}
	}
}

// TestRustMatchArmCfgIsABranchElement protects the cfg of a match arm: an
// arm's outer attributes are its own children, so two arms of one pattern,
// gated `#[cfg(unix)]` and `#[cfg(windows)]`, each defining h, get distinct
// branches, the arm's element first and its cfg after it.
//
// Mutation: read only the attributes before a node (the arm's own are
// ignored) -> both h spell `match "x" "0"` and collide.
// Mutation: push the arm's cfg before its match element -> the order differs.
func TestRustMatchArmCfgIsABranchElement(t *testing.T) {
	src := `fn f(x: u8) {
    match x {
        #[cfg(unix)]
        0 => { fn h() {} }
        #[cfg(windows)]
        0 => { fn h() {} }
        _ => {}
    }
}
`
	wantBranches(t, rustDecls(t, src), "h",
		`match "x" "0" / cfg "(unix)"`,
		`match "x" "0" / cfg "(windows)"`)
}

// TestRustCfgAttrAppliesItsCfgAttributes protects cfg_attr: each `cfg(q)` a
// `#[cfg_attr(p, ...)]` applies is one element prefixed by its predicate, a
// nested cfg_attr prefixes both predicates, an attribute it applies that is
// no cfg (a derive, a doc) adds nothing, and a plain `#[cfg]` keeps its one
// element.
//
// Mutation: read only `cfg` attributes -> g, k and v have no branch.
// Mutation: emit every payload a cfg_attr applies -> g gains a derive
// element.
// Mutation: drop the outer predicate of a nested cfg_attr -> g spells
// `cfg_attr "b" cfg "(c)"`.
func TestRustCfgAttrAppliesItsCfgAttributes(t *testing.T) {
	src := `#[cfg_attr(a, cfg_attr(b, cfg(c)), derive(Debug))]
fn g() {}

#[cfg_attr(feature = "x", doc = "k", cfg(all(unix, not(test))))]
#[cfg(target_os = "linux")]
fn k() {}

#[cfg_attr(any(a, b), derive(Clone))]
fn plain() {}

pub enum E {
    #[cfg_attr(unix, cfg(feature = "v"))]
    V,
}
`
	decls := rustDecls(t, src)
	wantBranches(t, decls, "g", `cfg_attr "a" cfg_attr "b" cfg "(c)"`)
	wantBranches(t, decls, "k", `cfg_attr "feature = \"x\"" cfg "(all(unix, not(test)))" / cfg "(target_os = \"linux\")"`)
	wantBranches(t, decls, "plain", "")
	wantBranches(t, decls, "V", `cfg_attr "unix" cfg "(feature = \"v\")"`)
}
