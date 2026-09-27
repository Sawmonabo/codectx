package worker

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// TestFlatMatchesNative protects the lowerings and the callable walk from a
// flat array that differs from the tree it was flattened from: a lowering
// reading it would build a different graph with no error anywhere. Every
// source (every grammar fixture and every golden case's source) is parsed
// with every grammar, so error, missing and extra nodes are covered, and for
// each tree it checks:
//
//   - the error figures a header's fallback is decided by: the array's error
//     flag and outermost ERROR bytes equal the native tree's, so a header is
//     never kept in the grammar with more errors. It is checked on its own,
//     before and whatever the node checks find;
//   - the handle: the root node Flatten reached through the tree wrapper's
//     field is the binding's root node (kind, byte range, child count), so a
//     binding that moves or replaces the handle fails here rather than
//     reading wrong memory;
//   - the record, where no accessor reads it as the native node answers:
//     each node's field and missing flag, and its next-sibling link, which is
//     structural and so is the parent's next child (Child(k+1)), zero-width
//     nodes included;
//   - the accessor: every method a lowering or the callable walk calls,
//     answered as the native node answers it -- NextSibling and
//     NextNamedSibling skipping a zero-width sibling at the node's end, as
//     the native calls do -- ChildByFieldId for every field id of the
//     grammar's field registry, answered as the library's field map answers
//     it (a field inherited through a hidden or an aliased child, an outer
//     field on a node that also has a nearer one, none on an ERROR node),
//     and the cursor stepped in lockstep with a native one from every node,
//     its FieldId the nearest field; every absent answer is the null handle,
//     as the zero Node is.
//
// Every mismatch is reported, not only the first, so one defect does not
// hide another.
//
// Mutations that fail it: record the node's grammar symbol instead of its
// public one; take the field from the parent's field instead of the cursor's;
// drop an extra from the sibling chain; count NamedChild without extras;
// let GotoNextSibling leave the cursor's root; answer a zero distance with
// the node itself instead of the null handle; drop the zero-width skip from
// NextSibling, or apply it to the record's link or to Child; sum error bytes
// over every node that contains an error instead of every ERROR node, or
// count an ERROR node nested in another; answer ChildByFieldId from the
// children's nearest fields instead of the field table (an inherited field,
// an outer field and an ERROR node fail); resolve an answer below an aliased
// child to that child instead of the answer itself.
func TestFlatMatchesNative(t *testing.T) {
	srcs := flatSources(t)
	for _, l := range lang.All {
		p, err := NewParser(l.Name)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range srcs {
			t.Run(l.Name+"/"+s.name, func(t *testing.T) {
				tree := Parse(p, s.src, nil)
				if tree == nil {
					t.Fatal("the parser produced no tree")
				}
				defer tree.Close()
				f, err := Flatten(tree, l.Name)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := f.parseErrors(grammars[l.Name].kindIDs().errorKind), nativeErrors(tree); got != want {
					t.Errorf("flat error figures %+v, native %+v", got, want)
				}
				checkFlat(t, grammars[l.Name].fieldRegistry(), tree, f)
			})
		}
		p.Close()
	}
}

// nativeErrors is tree's error figures read node by node from the native
// tree: its error flag, and the bytes of every ERROR node no ERROR node
// encloses.
func nativeErrors(tree *ts.Tree) lang.ParseErrors {
	root := tree.RootNode()
	pe := lang.ParseErrors{Any: root.HasError()}
	c := tree.Walk()
	defer c.Close()
	for {
		n := c.Node()
		if n.IsError() {
			pe.Bytes += uint64(n.EndByte() - n.StartByte())
		} else if c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return pe
			}
		}
	}
}

// flatSource is one source the flat array is checked over.
type flatSource struct {
	name string
	src  []byte
}

// flatSources is every grammar fixture and every golden case's source. The
// golden cases are literals inside the golden tests, so their sources are
// read from those files' syntax: each goldenCase literal's src, a string
// literal or a sum of them. A src of any other form fails, as does a file
// that runs golden cases with no source found, so no case is silently left
// out.
func flatSources(t *testing.T) []flatSource {
	t.Helper()
	var out []flatSource
	fixtures, err := filepath.Glob(filepath.Join("..", "testdata", "*"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no grammar fixtures: %v", err)
	}
	for _, path := range fixtures {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, flatSource{name: filepath.Base(path), src: src})
	}
	files, err := filepath.Glob("lower_*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		cases, golden := 0, false
		ast.Inspect(file, func(x ast.Node) bool {
			if call, ok := x.(*ast.CallExpr); ok {
				if f, ok := call.Fun.(*ast.Ident); ok && f.Name == "runGolden" {
					golden = true
				}
			}
			lit, ok := x.(*ast.CompositeLit)
			if !ok {
				return true
			}
			var elems []ast.Expr
			switch typ := lit.Type.(type) {
			case *ast.Ident:
				if typ.Name == "goldenCase" {
					elems = []ast.Expr{lit}
				}
			case *ast.ArrayType:
				if id, ok := typ.Elt.(*ast.Ident); ok && id.Name == "goldenCase" {
					elems = lit.Elts
				}
			}
			for _, e := range elems {
				c, ok := e.(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s: a golden case that is not a literal", fset.Position(e.Pos()))
				}
				for _, kv := range c.Elts {
					if kv, ok := kv.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "src" {
							cases++
							out = append(out, flatSource{name: fset.Position(kv.Pos()).String(), src: []byte(stringLit(t, fset, kv.Value))})
						}
					}
				}
			}
			return true
		})
		if golden && cases == 0 {
			t.Fatalf("%s runs golden cases, and none has a source this reader finds", path)
		}
	}
	return out
}

// stringLit is the value of a string literal or of a sum of them.
func stringLit(t *testing.T, fset *token.FileSet, e ast.Expr) string {
	t.Helper()
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			if err != nil {
				t.Fatalf("%s: %v", fset.Position(e.Pos()), err)
			}
			return s
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return stringLit(t, fset, e.X) + stringLit(t, fset, e.Y)
		}
	case *ast.ParenExpr:
		return stringLit(t, fset, e.X)
	}
	t.Fatalf("%s: a golden source that is not a string literal", fset.Position(e.Pos()))
	return ""
}

// checkFlat compares f with tree; see TestFlatMatchesNative.
func checkFlat(t *testing.T, reg *fieldRegistry, tree *ts.Tree, f *Flat) {
	t.Helper()
	// The native nodes in preorder, each with the field the cursor reports
	// for it, and each native id's preorder index.
	var native []*ts.Node
	var fields []uint16
	index := map[uintptr]int{}
	c := tree.Walk()
	defer c.Close()
	for {
		n := c.Node()
		index[n.Id()] = len(native)
		native = append(native, n)
		fields = append(fields, c.FieldId())
		if c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				goto walked
			}
		}
	}
walked:
	nat := func(n *ts.Node) int {
		if n == nil {
			return -1
		}
		i, ok := index[n.Id()]
		if !ok {
			t.Fatalf("a native node the cursor never visited: %s at %d", n.Kind(), n.StartByte())
		}
		return i
	}
	// flat is n's index, -1 for the null handle; a handle on another Flat
	// fails.
	flat := func(n Node) int {
		if n.IsNull() {
			return -1
		}
		if n.f != f {
			t.Fatalf("a handle on another array")
		}
		return int(n.i)
	}
	// dist is the index a record's distance names, -1 for none.
	dist := func(i int, d uint32, forward bool) int {
		switch {
		case d == 0:
			return -1
		case forward:
			return i + int(d)
		}
		return i - int(d)
	}

	root, froot := tree.RootNode(), f.Root()
	var null Node
	if !null.IsNull() || froot.IsNull() || !froot.Parent().IsNull() || !froot.NextSibling().IsNull() ||
		!froot.PrevSibling().IsNull() || !froot.ChildByFieldId(0).IsNull() || !froot.Child(froot.ChildCount()).IsNull() {
		t.Fatal("the zero Node is not null, the root is, or an absent answer from the root is not null")
	}
	if froot.KindId() != root.KindId() || froot.StartByte() != root.StartByte() || froot.EndByte() != root.EndByte() ||
		froot.ChildCount() != root.ChildCount() {
		t.Fatalf("the handle's root is %d [%d,%d) with %d children, the binding's %d [%d,%d) with %d",
			froot.KindId(), froot.StartByte(), froot.EndByte(), froot.ChildCount(),
			root.KindId(), root.StartByte(), root.EndByte(), root.ChildCount())
	}
	if len(f.nodes) != len(native) {
		t.Fatalf("%d flat nodes, %d native", len(f.nodes), len(native))
	}

	nc, fc := root.Walk(), froot.Walk()
	defer nc.Close()
	defer fc.Close()
	for i, n := range native {
		fn := Node{f: f, i: uint32(i)}
		at := func(what string, got, want any) {
			t.Helper()
			if got != want {
				t.Errorf("node %d (%s [%d,%d)): %s = %v, native %v", i, n.Kind(), n.StartByte(), n.EndByte(), what, got, want)
			}
		}
		// The record, where no accessor answers for it: the field, the missing
		// flag, and the structural next-sibling link, which is the parent's
		// next child whatever its width.
		r := f.nodes[i]
		at("record field", uint16(r.field), fields[i])
		at("record missing", r.flags&flatMissing != 0, n.IsMissing())
		structural := -1
		if parent := n.Parent(); parent != nil {
			for k := range parent.ChildCount() {
				if c := parent.Child(k); c != nil && c.Id() == n.Id() {
					structural = nat(parent.Child(k + 1))
					break
				}
			}
		}
		at("record next sibling", dist(i, uint32(r.next), true), structural)

		// The accessor.
		at("KindId", fn.KindId(), n.KindId())
		at("IsNamed", fn.IsNamed(), n.IsNamed())
		at("IsExtra", fn.IsExtra(), n.IsExtra())
		at("HasError", fn.HasError(), n.HasError())
		at("StartByte", fn.StartByte(), n.StartByte())
		at("EndByte", fn.EndByte(), n.EndByte())
		at("Parent", flat(fn.Parent()), nat(n.Parent()))
		at("NextSibling", flat(fn.NextSibling()), nat(n.NextSibling()))
		at("PrevSibling", flat(fn.PrevSibling()), nat(n.PrevSibling()))
		at("NextNamedSibling", flat(fn.NextNamedSibling()), nat(n.NextNamedSibling()))
		at("ChildCount", fn.ChildCount(), n.ChildCount())
		for j := range n.ChildCount() + 1 {
			at("Child("+strconv.Itoa(int(j))+")", flat(fn.Child(j)), nat(n.Child(j)))
		}
		at("NamedChildCount", fn.NamedChildCount(), n.NamedChildCount())
		for j := range n.NamedChildCount() + 1 {
			at("NamedChild("+strconv.Itoa(int(j))+")", flat(fn.NamedChild(j)), nat(n.NamedChild(j)))
		}
		for _, id := range reg.ids {
			at("ChildByFieldId("+strconv.Itoa(int(id))+")", flat(fn.ChildByFieldId(id)), nat(n.ChildByFieldId(id)))
		}

		// The cursor, rooted at this node and stepped in lockstep with a
		// native one over the node's subtree: Walk at the root, Reset after.
		if i > 0 {
			nc.Reset(*n)
			fc.Reset(fn)
		}
		lockstep(func(what string, got, want any) { at("cursor from here: "+what, got, want) }, nc, fc, nat, flat)
	}
}

// lockstep walks c, a native cursor, and fc, a flat one, over the subtree of
// their common root in preorder, reporting through at every answer of fc
// that differs from c's.
func lockstep(at func(what string, got, want any), c *ts.TreeCursor, fc *Cursor, nat func(*ts.Node) int, flat func(Node) int) {
	for step := 0; ; step++ {
		where := "step " + strconv.Itoa(step) + " "
		at(where+"Node", flat(fc.Node()), nat(c.Node()))
		at(where+"FieldId", fc.FieldId(), c.FieldId())
		down := c.GotoFirstChild()
		at(where+"GotoFirstChild", fc.GotoFirstChild(), down)
		if down {
			continue
		}
		for {
			right := c.GotoNextSibling()
			at(where+"GotoNextSibling", fc.GotoNextSibling(), right)
			if right {
				break
			}
			up := c.GotoParent()
			at(where+"GotoParent", fc.GotoParent(), up)
			if !up {
				return
			}
		}
	}
}
