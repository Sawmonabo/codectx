package worker

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goTypeParamsSource declares a package type T beside type parameters named
// T: a function's, a method receiver's, a generic type's, and one whose
// constraint names no single type.
const goTypeParamsSource = `package p

type T struct{}

func (T) M() {}

type I interface{ M() }

type List[E I] []E

type Box[P I] struct{ v P }

func F[T I](v T) { v.M() }

func G[T int | string](v T) { v.M() }

func (l List[T]) H(v T) { v.M() }

func (b Box[Q]) J() { b.v.M() }

func K(v T) { v.M() }
`

// TestGoTypeParameterBindsToItsDeclaration protects the scope of a Go type
// parameter: a parameter used in a signature or a body reduces to what its
// declaration says (its constraint's one type, or nothing), never to the
// package type of the same name, and a name outside every type parameter's
// scope keeps the package type.
//
// Mutation: reduce a type name by its text alone -> F's and H's receivers
// name the package type T. Mutation: read only the method's own type
// parameter list -> H's receiver-declared T names the package type T.
// Mutation: reduce a union constraint to its first term -> G's receiver is
// int. Mutation: leave a generic type's field types out of its parameters'
// scope -> J's `b.v` names P, not I. Mutation:
// reduce a name by every type parameter the file declares -> K's receiver
// is I instead of the package type T.
func TestGoTypeParameterBindsToItsDeclaration(t *testing.T) {
	src := goTypeParamsSource
	l, _ := linkOf(t, "go", "p.go", src)
	cases := []struct {
		name, call string
		want       wire.Type
	}{
		{"a function's type parameter takes its constraint", "func F[T I](v T) { v.M", wire.Type{Kind: wire.TypeNamed, Name: "I"}},
		{"a union constraint names no type", "func G[T int | string](v T) { v.M", wire.Type{}},
		{"a receiver's type parameter takes the generic type's constraint", "H(v T) { v.M", wire.Type{Kind: wire.TypeNamed, Name: "I"}},
		{"a generic type's field takes its parameter's constraint", "J() { b.v.M", wire.Type{Kind: wire.TypeNamed, Name: "I"}},
		{"outside every type parameter's scope the package type is kept", "func K(v T) { v.M", wire.Type{Kind: wire.TypeNamed, Name: "T"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at := textAt(t, src, c.call, 0)
			s := siteNamed(l, wire.Span{Start: at.End - 1, End: at.End})
			if s == nil {
				t.Fatalf("no site for %q", c.call)
			}
			if s.Shape != wire.ShapeMember || s.Receiver != c.want {
				t.Fatalf("%q is a %v call on %+v, want a member call on %+v", c.call, s.Shape, s.Receiver, c.want)
			}
		})
	}
}
