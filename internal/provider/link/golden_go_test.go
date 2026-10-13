package link_test

import "testing"

// TestGoGoldens runs the Go goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestGoGoldens(t *testing.T) { runGoldens(t, goGoldens) }

var goGoldens = []golden{
	{
		name:   "embedding-promotes",
		anchor: "The Go Programming Language Specification, \"Struct types\" (embedded fields, promoted methods), \"Selectors\", \"Method sets\"",
		files: map[string]string{
			"p.go": `package p

type Base struct{}

func (Base) Close() {} // @bclose

type Conn struct{ Base }

func (Conn) Close() {} // @cclose

type Wrap struct{ Base }

func useBase(b Base) { b.Close() } // @ub

func useConn(c Conn) { c.Close() } // @uc

func useWrap(w Wrap) { w.Close() } // @uw
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"ub Close -> bclose Close exact",
			"uc Close -> cclose Close exact",
			"uw Close -> bclose Close exact",
		},
		fails: "dropping promotion leaves `w.Close()` unbound; a concrete type's call marked virtual, as if an embedding type could override it",
	},
	{
		name:   "interface-embedding",
		anchor: "The Go Programming Language Specification, \"Interface types\" (embedded interfaces), \"Method sets\", \"Calls\"",
		files: map[string]string{
			"p.go": `package p

type R interface{ Read() } // @r

type W interface{ Write() } // @w

type RW interface {
	R
	W
}

type File struct{}

func (File) Read() {} // @fread

func (File) Write() {} // @fwrite

type Log struct{}

func (Log) Write() {} // @lwrite

func both(rw RW) {
	rw.Write() // @b1
	rw.Read()  // @b2
}

func writer(w W) {
	w.Write() // @w1
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"b1 Write -> w Write virtual",
			"b2 Read -> r Read virtual",
			"w1 Write -> w Write virtual",
		},
		fails: "a call through an interface published to each implementation binds File.Write and Log.Write; an interface not closed over its embedded interfaces leaves `rw.Write()` unbound",
	},
	{
		name:   "module-import-paths",
		anchor: "The Go Programming Language Specification, \"Import declarations\", \"Qualified identifiers\"; Go Modules Reference, \"go.mod files\" (module directive)",
		files: map[string]string{
			"go.mod": `module example.com/app

go 1.22
`,
			"pkg/errors/errors.go": `package errors

func Wrap(err error, msg string) error { return err } // @own
`,
			"log/log.go": `package log

func Printf(format string, args ...any) {} // @ownlog
`,
			"main.go": `package main

import (
	"log"

	appErrors "example.com/app/pkg/errors"
	"github.com/pkg/errors"
)

func main() {
	err := errors.Wrap(nil, "x")   // @ext
	err = appErrors.Wrap(err, "y") // @int
	log.Printf("%v", err)          // @std
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"int Wrap -> own Wrap exact",
		},
		fails: "an import bound by directory suffix binds github.com/pkg/errors and the standard log package to the module's own directories",
	},
	{
		name:   "method-expression-values",
		anchor: "The Go Programming Language Specification, \"Method expressions\", \"Function types\", \"Calls\"",
		files: map[string]string{
			"p.go": `package p

type T struct{}

func (T) M(x int) int { return x } // @tm

func other(x int) int { return x + 1 } // @other

func unused(x int) int { return x + 2 }

func run() int {
	var f func(T, int) int = T.M
	var g func(int) int = other
	return f(T{}, 1) + g(2) // @call
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"call f -> tm M exact",
			"call g -> other other exact",
		},
		fails: "a method expression joined on the method's own signature binds `g(2)` to T.M and leaves `f(T{}, 1)` unbound",
	},
	{
		name:   "results-and-package-scope",
		anchor: "The Go Programming Language Specification, \"Calls\", \"Declarations and scope\" (the package block), \"Method sets\"",
		files: map[string]string{
			"a.go": `package p

type Server struct{}

func NewServer() *Server { return &Server{} } // @new

func (s *Server) Start() {} // @start

func boot() {
	NewServer().Start() // @boot
	helper()            // @h
}
`,
			"b.go": `package p

func helper() {} // @helper
`,
			"other/o.go": `package other

func helper() {} // @otherhelper
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"boot NewServer -> new NewServer exact",
			"boot Start -> start Start exact",
			"h helper -> helper helper exact",
		},
		fails: "a call result receiver resolved to nothing leaves Start unbound; a bare call looked up outside the package binds other.helper",
	},
	{
		name:   "field-hides-promotion",
		anchor: "The Go Programming Language Specification, \"Selectors\" (the shallowest depth decides; a field hides a deeper method), \"Method sets\", \"Struct types\"",
		files: map[string]string{
			"p.go": `package p

type Closer interface{ Close() } // @closer

type Base struct{}

func (Base) Close() {} // @bclose

type T struct {
	Base
	Close func()
}

func stop() {} // @stop

func build() T { return T{Close: stop} }

func viaField(t T) { t.Close() } // @vf

func viaIface(c Closer) { c.Close() } // @vi
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"vf Close -> stop stop exact",
			"vi Close -> closer Close virtual",
		},
		fails: "a promoted method not hidden by a shallower field binds `t.Close()` to Base.Close; a call through Closer published to Base.Close instead of the method Closer declares",
	},
	{
		name:   "field-and-method-at-one-depth",
		anchor: "The Go Programming Language Specification, \"Selectors\" (a selector with more than one field or method at the shallowest depth is illegal), \"Struct types\"",
		files: map[string]string{
			"p.go": `package p

type A struct{}

func (A) Run() {} // @arun

type B struct{ Run func() }

type X struct {
	A
	B
}

type Y struct {
	A
}

func useX(x X) { x.Run() } // @ux

func useY(y Y) { y.Run() } // @uy
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. The selector `x.Run` is illegal (a method
		// and a field at the shallowest depth), so the program never runs that call; both take that as no
		// target.
		want: []string{
			"uy Run -> arun Run exact",
		},
		fails: "a selector resolved at the shallowest depth despite a second name there binds `x.Run()` to A.Run",
	},
	{
		name:   "field-receivers-across-files",
		anchor: "The Go Programming Language Specification, \"Selectors\" (a selector of a field, including a promoted field of an embedded pointer, has the field's type; a method call on it selects that type's method), \"Struct types\" (embedded fields, promoted fields)",
		files: map[string]string{
			"server.go": `package p

type Server struct {
	cfg Config
	*DBHolder
}

type DBHolder struct {
	db DB
}
`,
			"types.go": `package p

type Config struct {
	store Store
}

func (Config) Load() {} // @load

type DB struct{}

func (*DB) Query() {} // @query
`,
			"store.go": `package p

type Store struct{}

func (Store) Get() {} // @get
`,
			"use.go": `package p

import "example.com/ext"

func (s *Server) Start() {
	s.cfg.Load()  // @u1
	s.db.Query()  // @u2
	s.cfg.store.Get() // @u4
}

func other(v ext.Server) {
	v.Cfg.Load() // @u3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. Both read `s.db.Query()` as legal: the
		// field reached through the embedded pointer is addressable, so the pointer-receiver method is called
		// on its address ("Calls").
		want: []string{
			"u1 Load -> load Load exact",
			"u2 Query -> query Query exact",
			"u4 Get -> get Get exact",
		},
		fails: "a member call through a field typed in another file, or through a promoted field of an embedded pointer, or through a chain of fields, left unbound; a field read on a receiver of unknown type bound by the field's name",
	},
	{
		name:   "method-sets-by-type-identity",
		anchor: "The Go Programming Language Specification, \"Type identity\", \"Alias declarations\", \"Numeric types\" (byte is uint8), \"Interface types\" (any), \"Import declarations\" (dot imports), \"Uniqueness of identifiers\", \"Method sets\"",
		files: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.27\n",
			"store/store.go": `package store

type ID string

type Item struct{}

type Getter interface { // @tgetter
	Get(id ID) (*Item, error) // @sget
	Raw(b []byte) any // @sraw
}

type hidden interface {
	peek()
}
`,
			"mem/mem.go": `package mem

import s "example.com/m/store"

type Key = s.ID

type M struct{} // @tm

func (M) Get(id Key) (*s.Item, error) { return nil, nil } // @mget

func (M) Raw(b []uint8) interface{} { return nil } // @mraw

func (M) peek() {}

type ID string

type N struct{}

func (N) Get(id ID) (*s.Item, error) { return nil, nil }

func (N) Raw(b []byte) any { return nil }
`,
			"dot/dot.go": `package dot

import . "example.com/m/store"

type D struct{} // @td

func (D) Get(id ID) (*Item, error) { return nil, nil } // @dget

func (D) Raw(b []byte) any { return nil } // @draw
`,
			"gen/gen.go": `package gen

type Container[T any] interface { // @tcont
	Add(v T) // @cadd
	Len() int // @clen
}

type Set[E comparable] struct{} // @tset

func (s *Set[E]) Add(v E) {} // @sadd

func (s *Set[E]) Len() int { return 0 } // @slen

type Ints struct{}

func (Ints) Add(v int) {}

func (Ints) Len() int { return 0 }
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. M's Key is store.ID through an alias, its
		// []uint8 is []byte and its interface{} is any, so M's method set has Getter's; N's ID is mem's own
		// type, a different type; D names store's types through its dot import; Set's and Container's type
		// parameters stand in the same position, Ints's int does not stand for T; M's peek is mem's, never
		// store's unexported hidden.peek.
		want: []string{},
		facts: []string{
			"tm M implements tgetter Getter",
			"mget Get overrides sget Get",
			"mraw Raw overrides sraw Raw",
			"td D implements tgetter Getter",
			"dget Get overrides sget Get",
			"draw Raw overrides sraw Raw",
			"tset Set implements tcont Container",
			"sadd Add overrides cadd Add",
			"slen Len overrides clen Len",
		},
		// Mutation: compare signatures as written -> M and D implement nothing. Mutation: read an alias as
		// a type of its own -> M's Get misses. Mutation: keep byte and uint8, any and interface{} apart ->
		// M's Raw misses. Mutation: qualify an unqualified name by the file's package alone -> N implements
		// Getter. Mutation: key an unexported method by its name alone -> M implements hidden. Mutation:
		// spell a type parameter by name -> Set implements nothing.
		fails: "a method set compared by spelling instead of type identity: an alias, byte/uint8, any/interface{}, a dot import or a renamed import missing its implements; a type of another package of the same name, or an unexported method of another package, taken as the interface's",
	},
	{
		name:   "external-interfaces-by-method-set",
		anchor: "The Go Programming Language Specification, \"Interface types\", \"Method sets\", \"Struct types\" (promoted methods), \"Type identity\"; Go Modules Reference, \"Module cache\"",
		files: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.27\n\nrequire (\n\texample.com/dep v1.2.0\n\texample.com/old v2.0.0\n)\n",
			"p.go": `package p

import (
	"io"

	"example.com/dep"
	"example.com/old"
)

type F struct{} // @tf

func (*F) Read(b []byte) (int, error) { return 0, nil } // @fread

func (*F) Write(b []uint8) (n int, err error) { return 0, nil } // @fwrite

func (*F) ReadRune() (rune, int, error) { return 0, 0, nil }

type E struct { // @te
	io.Reader
}

type S struct{}

func (S) Read(b []rune) (int, error) { return 0, nil }

type W struct { // @tw
	dep.Base
}

type V struct{} // @tv

func (V) Name() string { return "" } // @vname

type P struct{}

func (P) Ping() {}

var _ old.Pinger
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. io is read from the standard library and
		// dep from the module cache at the required v1.2.0; old is required at v2.0.0, which the cache does
		// not hold, so no Pinger is read and P proves nothing. F's method set covers io.Reader, io.Writer and
		// io.ReadWriter; RuneScanner's unexported method keeps it out; E and W implement through the methods
		// their embedded io.Reader and dep.Base promote; S's []rune is []int32, never []byte.
		want: []string{},
		facts: []string{
			"tf F implements external io.Reader",
			"tf F implements external io.Writer",
			"tf F implements external io.ReadWriter",
			"fread Read overrides external io.Reader.Read",
			"fread Read overrides external io.ReadWriter.Read",
			"fwrite Write overrides external io.Writer.Write",
			"fwrite Write overrides external io.ReadWriter.Write",
			"te E implements external io.Reader",
			"tw W implements external example.com/dep.Named",
			"tv V implements external example.com/dep.Named",
			"vname Name overrides external example.com/dep.Named.Name",
		},
		// Mutation: read another cached version of a required module -> P implements old.Pinger.
		// Mutation: leave out promotion through an external type -> E and W implement nothing.
		// Mutation: materialize an interface with an unexported method -> F implements io.RuneScanner.
		// Mutation: unify rune with uint8 -> S implements io.Reader.
		fails: "an interface outside the unit left without its implementations and overrides; one read from a module version the go.mod does not require; an interface only its own package can implement taken as satisfied",
	},
	{
		name:   "conversions-prove-external-interfaces",
		anchor: "The Go Programming Language Specification, \"Assignability\", \"Variable declarations\", \"Conversions\"; Effective Go, \"Interface checks\"",
		files: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.27\n\nrequire example.com/absent v1.0.0\n",
			"p.go": `package p

import "example.com/absent"

type T struct{} // @tt

func (T) Name() string { return "" }

type U struct{} // @tu

var _ absent.Named = T{}

var _ absent.Named = (*U)(nil)

func use() {
	var n absent.Named = &T{}
	_ = n
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. The cache holds no example.com/absent, so
		// only the conversions prove T and U implement absent.Named, and name none of its methods: no
		// overrides.
		want: []string{},
		facts: []string{
			"tt T implements external example.com/absent.Named",
			"tu U implements external example.com/absent.Named",
		},
		// Mutation: drop the conversions when the interface's source is unread -> no implements. Mutation:
		// publish overrides from a conversion -> T's Name overrides absent.Named.Name.
		fails: "a compile-time interface proof of a type of the unit toward an interface outside it left unpublished when its source is unread",
	},
}
