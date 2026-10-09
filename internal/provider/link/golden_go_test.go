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
			"ub Close -> bclose Close",
			"uc Close -> cclose Close",
			"uw Close -> bclose Close",
		},
		fails: "an embedded field treated as a subtype dispatches `b.Close()` into Conn; dropping promotion leaves `w.Close()` unbound",
	},
	{
		name:   "interface-embedding",
		anchor: "The Go Programming Language Specification, \"Interface types\" (embedded interfaces), \"Method sets\", \"Calls\"",
		files: map[string]string{
			"p.go": `package p

type R interface{ Read() }

type W interface{ Write() }

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
			"b1 Write -> fwrite Write",
			"b2 Read -> fread Read",
			"w1 Write -> fwrite Write",
			"w1 Write -> lwrite Write",
		},
		fails: "an interface not closed over its embedded interfaces lets Log satisfy RW, or lets nothing satisfy it",
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
			"int Wrap -> own Wrap",
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
			"call f -> tm M",
			"call g -> other other",
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
			"boot NewServer -> new NewServer",
			"boot Start -> start Start",
			"h helper -> helper helper",
		},
		fails: "a call result receiver resolved to nothing leaves Start unbound; a bare call looked up outside the package binds other.helper",
	},
}
