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
	"internal/snapshot/lock.go (*WorkspaceLock).record": "clears the workspace lock file before writing " +
		"the holder's one-line record into it. The file never holds more than maxHolderRecord bytes, so " +
		"this returns less than a single block and cannot burst whatever the workspace's history.",
	"internal/snapshot/lock.go (*WorkspaceLock).Close": "clears that same one-line record as the lock is " +
		"given up, so a waiter arriving before the next holder is told nobody recorded themselves rather " +
		"than handed the name of a process that has let go. Same bounded line, same nothing freed.",
	"internal/provider/dependence/neo4jcsv/keys.go (*scratch).saveKeys": "creates the key set over the " +
		"previous import's, which is the one of these that is unbounded -- 153 MB in one recorded run. " +
		"The old set is emptied through paced.Shrink immediately above the create, so by the time the " +
		"truncating open runs there is nothing left for it to free.",
	"internal/index/delta/delta.go previousState": "creates the file a predecessor's delta state is " +
		"streamed into, which a previous call may have left as large as that predecessor's unit. It is " +
		"emptied through paced.Shrink immediately above the open.",
	"internal/provider/scip/compdb.go writeFileInPlace": "truncates a compilation database to write the " +
		"rewritten one over it; the database is as large as the project it describes, so the old contents " +
		"go back through paced.Shrink immediately above the open.",
	"internal/tools/toollock/archive.go writeStream": "writes one entry of a verified archive into a " +
		"freshly made extraction directory, so the target does not exist and nothing is freed. An archive " +
		"naming one entry twice truncates the first, which is bounded by that entry's own size and so by " +
		"the payload size the lock pins.",
	"internal/tools/toollock/fetch.go downloadOnce": "creates the download destination, a path this " +
		"function mints for one attempt; a retry gets its own. Nothing is there to free.",
	"internal/tools/toollock/normalize.go packDeterministic": "creates the normalized archive beside the " +
		"tree it packs, at a path the caller mints per normalization.",
	"cmd/codectx/pprof.go startProfiling": "creates the CPU profile of one run, at a path named for the " +
		"process and the moment it started, so nothing is overwritten.",
	"cmd/codectx/pprof.go writeProfile": "creates one heap or block profile at the same kind of path, " +
		"and for the same reason frees nothing.",
	"internal/cli/init.go writeProjectConfig": "truncates the workspace's own config.toml to write the " +
		"rendered one over it. A configuration file is a page of text; the whole of what this can free " +
		"is smaller than one window.",
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
// Mutation: free one raw -- put os.Remove back at any governed call site, or
// an os.Create over an existing file -- and this names the file and the
// function. NOT RUN in this round (owner order 2026-09-17).
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
// os.Remove, os.RemoveAll, os.Truncate, Truncate on a file, or a TRUNCATING
// OPEN -- os.Create, and os.OpenFile with O_TRUNC.
//
// The truncating opens are here because they free exactly what a truncation
// frees: an existing file's blocks, all of them, in one act, before the first
// byte of the new contents is written. Reading the enumeration as unlink and
// truncate alone left nine of them unexamined, and a review proved the gap by
// putting one at a governed site and watching this test stay green.
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
		case "Remove", "RemoveAll", "Truncate", "Create":
			return true
		case "OpenFile":
			return truncatingFlags(call)
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

// truncatingFlags reports whether an os.OpenFile call names os.O_TRUNC among
// its flags. The flags are an or-ed expression, so the whole of it is walked
// rather than matched: a call that reaches O_TRUNC through a variable is not
// recognised here, which is why the enumeration is a floor and the list of
// written-down sites is read by a person.
func truncatingFlags(call *ast.CallExpr) bool {
	if len(call.Args) < 2 {
		return false
	}
	truncating := false
	ast.Inspect(call.Args[1], func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if recv, _ := sel.X.(*ast.Ident); recv != nil && recv.Name == "os" && sel.Sel.Name == "O_TRUNC" {
			truncating = true
		}
		return true
	})
	return truncating
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
