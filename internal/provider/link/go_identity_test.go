package link

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// fakeNames is one file for goIdentity: its package path, its imports by
// name and its dot imports.
type fakeNames struct {
	path    string
	imports map[string][]string
	dot     []string
	unread  []string
}

func (n fakeNames) pkg() string { return n.path }

func (n fakeNames) imported(name string) (string, bool, error) {
	if p := n.imports[name]; len(p) == 1 {
		return p[0], true, nil
	}
	return "", false, nil
}

func (n fakeNames) dots() ([]string, error) { return n.dot, nil }

func (n fakeNames) unresolved() ([]string, error) { return n.unread, nil }

// fakeTypes is every package's type declarations, by path then name: "" a
// defined type, any other value an alias's form written in aliasIn.
type fakeTypes struct {
	pkgs    map[string]map[string]string
	consts  map[string]map[string]string
	aliasIn goNames
}

func (t fakeTypes) lookup(_ context.Context, p, name string) (goTypeAnswer, bool, error) {
	pkg, ok := t.pkgs[p]
	if !ok {
		return goTypeAnswer{}, false, nil
	}
	form, ok := pkg[name]
	switch {
	case !ok:
		return goTypeAnswer{}, true, nil
	case form == "":
		return goTypeAnswer{found: true}, true, nil
	}
	return goTypeAnswer{found: true, alias: true, form: form, names: t.aliasIn}, true, nil
}

func (t fakeTypes) constant(_ context.Context, p, name string) ([]goDecl, error) {
	form, ok := t.consts[p][name]
	if !ok {
		return nil, nil
	}
	return []goDecl{{form: form, names: t.aliasIn}}, nil
}

// TestGoIdentityResolvesEveryName protects the canonical spelling method
// sets are compared by: a named type is its package's import path and name
// however the file writes it, an alias is the type it denotes, a
// predeclared alias its type, a package's own declaration shadows a
// predeclared name, and a name that cannot be decided makes the spelling
// unknown rather than a guess.
//
// Mutation: keep byte and
// any as written -> the uint8 and interface{} cases differ. Mutation: pick
// the first dot import that declares a name -> the ambiguous case resolves.
// Mutation: resolve an unknown qualifier as written -> it is not "?".
// Mutation: leave a dot-imported unread package's name to the file's
// package -> the unread dot import's case is not its unresolved spelling.
// Mutation: drop the bijection (one unread import) -> the go-yaml case is
// spelled unresolved. Mutation: spell an unbound qualifier `?yaml.Node`
// without its candidates -> two files importing different unread packages
// would compare equal; the several-imports case differs. Mutation: stop at
// the first unread dot import -> the read one no longer decides. Mutation:
// let a dot import supply an unexported name -> `local` is spelled
// unresolved. Mutation: keep an interface literal's elements in written
// order -> the sorted case differs. Mutation: leave an unexported field or
// method name unqualified -> two packages' `a` compare equal. Mutation:
// spell a constant length without folding -> `[ 12 ]` is not written.
// Mutation: leave a generic alias unknown, or substitute its arguments in
// another order -> the instantiated cases differ. Mutation: accept a
// generic alias without its argument list -> that case is not "?".
func TestGoIdentityResolvesEveryName(t *testing.T) {
	ctx := context.Background()
	store := fakeNames{path: "example.com/m/store"}
	types := fakeTypes{consts: map[string]map[string]string{
		"example.com/m/store": {"N": "( 1 << 4 )", "Odd": "?"},
	}, pkgs: map[string]map[string]string{
		"example.com/m/store": {"ID": "", "Item": "", "Key": ".ID", "error": "", "Vec": "$$1 [ ] $0",
			"Pair": "$$2 map [ $0 ] $1"},
		"example.com/m/mem": {"Local": ""},
		"example.com/a":     {"T": ""},
		"example.com/b":     {"T": ""},
	}, aliasIn: store}
	mem := fakeNames{path: "example.com/m/mem", imports: map[string][]string{"s": {"example.com/m/store"},
		"x": {"example.com/x", "example.com/y"}}}
	cases := []struct {
		name string
		n    goNames
		form string
		want string
	}{
		{"qualified through a renamed import", mem, "( :s.ID ) ( * :s.Item , .error )", "( example.com/m/store.ID ) ( * example.com/m/store.Item , error )"},
		{"alias", mem, "( :s.Key )", "( example.com/m/store.ID )"},
		{"predeclared aliases", mem, "( [ ] .byte , .rune , .any )", "( [ ] uint8 , int32 , interface{} )"},
		{"own type", mem, "( .Local )", "( example.com/m/mem.Local )"},
		{"own type shadows predeclared", store, "( .error )", "( example.com/m/store.error )"},
		{"undeclared name is the package's", mem, "( .Elsewhere )", "( example.com/m/mem.Elsewhere )"},
		{"ambiguous qualifier", mem, "( :x.T )", "?"},
		{"unknown qualifier", mem, "( :nope.T )", "?"},
		{"dot import", fakeNames{path: "example.com/m/dot", dot: []string{"example.com/a"}}, "( .T )", "( example.com/a.T )"},
		{"two dot imports declare it", fakeNames{path: "example.com/m/dot", dot: []string{"example.com/a", "example.com/b"}}, "( .T )", "?"},
		{"dot import not read", fakeNames{path: "example.com/m/dot", dot: []string{"example.com/unread"}}, "( .T )",
			"( ?.example.com/m/dot|example.com/unread.T )"},
		{"a read dot import decides past an unread one", fakeNames{path: "example.com/m/dot",
			dot: []string{"example.com/unread", "example.com/a"}}, "( .T )", "( example.com/a.T )"},
		{"an unexported name is never a dot import's", fakeNames{path: "example.com/m/dot",
			dot: []string{"example.com/unread"}}, "( .int , .local )", "( int , example.com/m/dot.local )"},
		{"one import whose clause is unread binds the unbound qualifier", fakeNames{path: "example.com/m/mem",
			unread: []string{"example.com/go-yaml"}}, "( :yaml.Node )", "( example.com/go-yaml.Node )"},
		{"several such imports spell the qualifier unresolved", fakeNames{path: "example.com/m/mem",
			unread: []string{"example.com/p1", "example.com/p2"}}, "( :yaml.Node , .int )",
			"( ?example.com/p1|example.com/p2:yaml.Node , int )"},
		{"type parameter", mem, "( $0 ) ( $1 )", "( $0 ) ( $1 )"},
		{"generic alias instantiated", mem, "( :s.Vec [ .byte ] )", "( [ ] uint8 )"},
		{"generic alias of generic alias arguments", mem, "( :s.Pair [ .string , :s.Vec [ $0 ] ] )",
			"( map [ string ] [ ] $0 )"},
		{"generic alias without its arguments", mem, "( :s.Vec )", "?"},
		{"interface literal elements sorted", mem, "( interface{ @Z ( ) ( ) ; ^ :s.Item ; @A ( .byte ) ( ) } )",
			"( interface{ @A ( uint8 ) ( ) ; @Z ( ) ( ) ; ^ example.com/m/store.Item } )"},
		{"unexported field and method names are their package's", mem,
			"( struct{ @a .int ; @B * :s.ID } , interface{ @m ( ) ( ) } )",
			"( struct{ @example.com/m/mem.a int ; @B * example.com/m/store.ID } , interface{ @example.com/m/mem.m ( ) ( ) } )"},
		{"constant length folds", mem, "( [# ( :s.N - 4 ) ] .byte )", "( [ 12 ] uint8 )"},
		{"length of an unfoldable constant is spelled", mem, "( [# :s.Odd ] .byte )",
			"( [# example.com/m/store.Odd ] uint8 )"},
		{"unspellable form", mem, "?", "?"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := goIdentity(ctx, types, c.n, c.form)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("goIdentity(%q) = %q, want %q", c.form, got, c.want)
			}
		})
	}
}

// TestGoASTFormsMatchTheWorkers protects the one identity both sides of a
// method set are spelled by: the forms read from a package outside the unit
// (go/parser) equal the worker's forms of the same declarations
// (worker.TestGoIdentityForms, which pins the worker's spelling of this
// source), so a unit method and an external interface's compare equal
// exactly when their types are identical.
//
// Mutation: spell a receive-only channel `chan <-` -> Add differs. Mutation:
// write results without their parentheses -> Load differs. Mutation: drop
// the variadic marker -> Add differs. Mutation: spell a struct literal with
// members as "?" -> Lit differs. Mutation: read an array length's literal
// as written -> Arr's `0x10` differs. Mutation: leave an embedded field's
// `*` out -> Iface differs. Mutation: give each constant spec iota 0 ->
// Kth and Lth differ.
func TestGoASTFormsMatchTheWorkers(t *testing.T) {
	const src = `package p

import st "example.com/store"

type A = io.Reader

type I[K comparable, V any] interface {
	Get(K) (V, bool)
}

func (s *Set[T]) Add(v T, ch <-chan int, out chan<- T, m map[string][4]byte, f func(int) error, e interface{}, z struct{}, xs ...[]byte) {
}

func (s *Set[T]) Load(id st.ID) (*st.Item, error) { return nil, nil }

func (s *Set[T]) Lit(x struct{ a int }) {}

func (s Set[T]) Pair(a, b string) {}

const (
	Size = 1 << 4
	Kth  = iota * 2
	Lth
)

func (s *Set[T]) Arr(a [Size]byte, b [0x10]byte, c [st.N + 1]int) {}

func (s *Set[T]) Bad(d [len("x")]int) {}

func (s *Set[T]) Iface(x interface {
	Close() error
	io.Reader
}, y struct {
	*Base
	B int ` + "`json:\"b c\"`" + `
}) {
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	pkg := &goExtPackage{path: "p", types: map[string]*goExtType{}}
	pkg.addFile(f, false)
	got := map[string]string{"A": pkg.types["A"].form}
	for name, cs := range pkg.consts {
		got[name] = "#" + cs[0].form
	}
	for _, m := range pkg.types["I"].methods {
		got[m.name] = m.form
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			_, params := goReceiverBase(fd.Recv.List[0].Type)
			got[fd.Name.Name] = goASTSignature(fd.Type, params)
		}
	}
	want := map[string]string{
		"A":    ":io.Reader",
		"Get":  "( $0 ) ( $1 , .bool )",
		"Add":  "( $0 , <- chan .int , chan <- $0 , map [ .string ] [ 4 ] .byte , func ( .int ) ( .error ) , interface{} , struct{} , ... [ ] .byte ) ( )",
		"Load": "( :st.ID ) ( * :st.Item , .error )",
		"Lit":  "( struct{ @a .int } ) ( )",
		"Pair": "( .string , .string ) ( )",
		"Arr":  "( [# .Size ] .byte , [ 16 ] .byte , [# ( :st.N + 1 ) ] .int ) ( )",
		"Bad":  "?",
		"Iface": `( interface{ @Close ( ) ( .error ) ; ^ :io.Reader } , ` +
			`struct{ ^ * .Base ; @B .int #"json:\"b\x20c\"" } ) ( )`,
		"Size": "#( 1 << 4 )",
		"Kth":  "#( 1 * 2 )",
		"Lth":  "#( 2 * 2 )",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("form of %s = %q, want %q", k, got[k], w)
		}
	}
}

// TestGoImportPathOfADirectory protects the package identity the canonical
// spelling names a unit's own types by: a vendored package is its import
// path, so a vendored type and the same type named by import are one; a
// module's package is the module path and its directory below the root; an
// external test package is its own package.
//
// Mutation: keep vendor/ in a vendored package's path -> the vendored case
// differs. Mutation: join the directory without the module root's offset ->
// the nested-root case differs. Mutation: drop the _test suffix -> the test
// package is the package itself.
func TestGoImportPathOfADirectory(t *testing.T) {
	cases := []struct{ module, root, dir, pkg, want string }{
		{"example.com/m", ".", ".", "m", "example.com/m"},
		{"example.com/m", ".", "a/b", "b", "example.com/m/a/b"},
		{"example.com/m", ".", "vendor/github.com/x/y", "y", "github.com/x/y"},
		{"example.com/sub", "tools/sub", "tools/sub/cmd", "main", "example.com/sub/cmd"},
		{"example.com/m", ".", "a", "a_test", "example.com/m/a_test"},
		{"", ".", "a", "a", "./a"},
	}
	for _, c := range cases {
		if got := goImportPath(c.module, c.root, c.dir, c.pkg); got != c.want {
			t.Errorf("goImportPath(%q, %q, %q, %q) = %q, want %q", c.module, c.root, c.dir, c.pkg, got, c.want)
		}
	}
}
