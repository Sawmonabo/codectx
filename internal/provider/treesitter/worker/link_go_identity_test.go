package worker

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

const goIdentitySource = `package p

import (
	"io"
	st "example.com/store"
)

type A = io.Reader

type G[T any] = []T

type I[K comparable, V any] interface {
	Get(K) (V, bool)
}

type Set[T any] struct{}

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

type T struct{}

var _ io.Reader = (*T)(nil)
var _ io.Reader = &T{}
var _ io.Reader = T{}
var _ io.Reader = new(T)
var _ *io.Reader = nil
var _ io.Reader = T.M

func f() {
	var w io.Writer = T{}
	_ = w
}
`

// TestGoIdentityForms protects the identity form the linker resolves method
// sets by: every type marked by how it is written, a receiver's or an
// interface's type parameters by position, the channel direction and an
// array's length kept, one type per declared name, a struct or interface
// literal with its members, an array length as its value or constant
// expression, an alias's form behind "=" and a constant's value behind "#".
//
// Mutation: spell a qualified type without its package marker (`.ID`) ->
// Load's form names a type of its own package. Mutation: write a type
// parameter by name -> Get and Add no longer spell their parameter `$0`.
// Mutation: drop the anonymous channel tokens -> `<-chan int` and
// `chan<- T` lose their direction. Mutation: count a grouped declaration
// once -> Pair takes one string. Mutation: spell a struct literal with
// members as `struct{}` -> Lit matches any method of an empty struct.
// Mutation: keep an integer literal as written -> `0x10` is not `16`.
// Mutation: drop a field's tag or the embedded marker -> Iface differs.
// Mutation: give every constant spec iota 0, or skip the implicit
// repetition -> Kth or Lth differ. Mutation: spell a call in a length -> Bad is
// not "?".
// Mutation: drop the "=" -> an alias reads as a method's signature.
// Mutation: spell a generic alias's parameters by name or leave out its
// arity -> the linker cannot substitute its arguments; G differs.
func TestGoIdentityForms(t *testing.T) {
	src := goIdentitySource
	l, _ := linkOf(t, "go", "p.go", src)
	cases := []struct {
		name string
		at   string
		nth  int
		want string
	}{
		{"alias", "A", 0, "=:io.Reader"},
		{"generic alias", "G", 0, "=$$1 [ ] $0"},
		{"interface method", "Get", 0, "( $0 ) ( $1 , .bool )"},
		{"literal types", "Add", 0, "( $0 , <- chan .int , chan <- $0 , map [ .string ] [ 4 ] .byte , func ( .int ) ( .error ) , interface{} , struct{} , ... [ ] .byte ) ( )"},
		{"qualified", "Load", 0, "( :st.ID ) ( * :st.Item , .error )"},
		{"struct literal", "Lit", 0, "( struct{ @a .int } ) ( )"},
		{"array lengths", "Arr", 0, "( [# .Size ] .byte , [ 16 ] .byte , [# ( :st.N + 1 ) ] .int ) ( )"},
		{"a length the form cannot spell", "Bad", 0, "?"},
		{"interface and struct literals", "Iface", 0,
			`( interface{ @Close ( ) ( .error ) ; ^ :io.Reader } , struct{ ^ * .Base ; @B .int #"json:\"b\x20c\"" } ) ( )`},
		{"constant", "Size", 0, "#( 1 << 4 )"},
		{"constant of iota", "Kth", 0, "#( 1 * 2 )"},
		{"constant repeating its declaration's value", "Lth", 0, "#( 2 * 2 )"},
		{"grouped names", "Pair", 0, "( .string , .string ) ( )"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := exportNamed(l, textAt(t, src, c.at, c.nth))
			if e == nil {
				t.Fatalf("no export for %s", c.at)
			}
			if e.Identity != c.want {
				t.Fatalf("identity of %s = %q, want %q", c.at, e.Identity, c.want)
			}
		})
	}
}

// TestGoConversionsNameTheConvertedType protects Go's compile-time proofs:
// a variable declared of a named type whose value's syntax spells its type
// (`(*T)(nil)`, `&T{}`, `T{}`, `new(T)`), at package level or in a function,
// is a conversion of T to that type; a declared type that is not a bare
// name, and a value whose type is not written in it, are none.
//
// Mutation: reduce the declared type with baseName -> `var _ *io.Reader`
// converts to io.Reader. Mutation: accept any call -> `T.M`'s method value
// counts. Mutation: skip function bodies -> the io.Writer conversion is
// missing.
func TestGoConversionsNameTheConvertedType(t *testing.T) {
	l, _ := linkOf(t, "go", "p.go", goIdentitySource)
	want := []wire.Conversion{
		{Type: "T", Interface: "io.Reader"},
		{Type: "T", Interface: "io.Reader"},
		{Type: "T", Interface: "io.Reader"},
		{Type: "T", Interface: "io.Reader"},
		{Type: "T", Interface: "io.Writer"},
	}
	if !slices.Equal(l.Conversions, want) {
		t.Fatalf("conversions = %+v, want %+v", l.Conversions, want)
	}
}
