package paced

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// inPlaceFrees is every call outside this package that gives disk space back
// without going through the reclaimer, keyed by the file and the function
// holding it, with the reason it is allowed to. It is keyed by function
// rather than by line so that an unrelated edit above one does not rewrite
// the list, and a call that moves to another function is a new entry that has
// to be read and justified.
//
// A site on this list is not an exception to the pace. Each of these is
// either charged where it stands -- the caller waits, at the same rate -- or
// frees nothing at all. What the list rules out is a raw truncation or unlink
// that frees an unbounded amount of space at neither.
var inPlaceFrees = map[string]string{
	"internal/snapshot/cas.go (*Batch).flush": "unlinks the staging name of a blob that has just been " +
		"published, so the object is reached by its name in the bucket and this unlink frees no blocks at all.",
}

// The requirement: every free in the product is governed by the pace. A burst
// of freed blocks on a host that discards them into a sparse image stalls
// every writer on the machine for about a minute, a minute later, and nothing
// inside the machine can observe or wait for it -- so a truncation or an
// unlink that escapes the reclaimer is the defect this whole mechanism
// exists to prevent, and one has escaped it three times already by being
// added somewhere nobody was looking.
//
// This enumerates the tree instead of trusting a document. A new call to
// os.Remove, os.RemoveAll, os.Truncate or a file's Truncate outside
// internal/paced fails here until it is either routed through this package or
// written down above with its reason.
//
// Mutation: free one raw -- put os.Remove back at any governed call site --
// and this names the file and the function.
func TestEveryFreeIsGovernedByThePace(t *testing.T) {
	root := moduleRoot(t)
	found := map[string]string{}
	for _, tree := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, tree), func(rel string, file *ast.File, fset *token.FileSet) {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !rawFree(call) {
					return true
				}
				found[rel+" "+enclosingFunc(file, n.Pos())] = fset.Position(n.Pos()).String()
				return true
			})
		})
	}

	var ungoverned []string
	for site, where := range found {
		if _, allowed := inPlaceFrees[site]; !allowed {
			ungoverned = append(ungoverned, site+" at "+where)
		}
	}
	if len(ungoverned) > 0 {
		t.Fatalf("%d free(s) escape the pace, neither routed through internal/paced nor written down with a reason:\n\t%s",
			len(ungoverned), strings.Join(ungoverned, "\n\t"))
	}
	for site := range inPlaceFrees {
		if _, still := found[site]; !still {
			t.Fatalf("the in-place free %q is written down and no longer there: the list has to say what the tree does", site)
		}
	}
}

// rawFree reports whether a call gives disk space back without this package:
// os.Remove, os.RemoveAll, os.Truncate, or Truncate on a file.
//
// A moment in time is truncated too, and says so: it is the result of an
// expression rather than a named file, and it is cut to a unit of time. Both
// are ruled out here, so a Truncate that reaches this test is one on
// something that holds blocks.
func rawFree(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, _ := sel.X.(*ast.Ident)
	if recv != nil && recv.Name == "paced" {
		return false
	}
	if recv != nil && recv.Name == "os" {
		switch sel.Sel.Name {
		case "Remove", "RemoveAll", "Truncate":
			return true
		}
		return false
	}
	if sel.Sel.Name != "Truncate" {
		return false
	}
	if _, chained := sel.X.(*ast.CallExpr); chained {
		return false
	}
	return len(call.Args) != 1 || !qualifiedBy(call.Args[0], "time")
}

// qualifiedBy reports whether e is pkg.Something.
func qualifiedBy(e ast.Expr, pkg string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// enclosingFunc names the function or method holding pos.
func enclosingFunc(file *ast.File, pos token.Pos) string {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos > fn.End() {
			continue
		}
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			return "(" + types(fn.Recv.List[0].Type) + ")." + fn.Name.Name
		}
		return fn.Name.Name
	}
	return "(file scope)"
}

func types(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "*" + types(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return types(t.X)
	case *ast.IndexListExpr:
		return types(t.X)
	}
	return "?"
}

// walkGoFiles parses every non-test Go file under dir except this package's
// own, whose whole purpose is to free.
func walkGoFiles(t *testing.T, dir string, fn func(rel string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	root := filepath.Dir(dir)
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if filepath.Base(path) == "paced" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		fn(filepath.ToSlash(rel), parsed, fset)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// moduleRoot is the directory holding this module's go.mod, found by walking
// up from this source file: a test that reads the tree must find it where it
// actually is and never at a path written down for one machine.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("this test cannot find its own source file")
	}
	for dir := filepath.Dir(self); ; {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("this test is not inside a module")
		}
		dir = parent
	}
}
