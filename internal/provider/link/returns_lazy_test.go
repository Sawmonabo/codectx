package link

import (
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestOutsideReturnsAreReadForTheBoundCallablesAlone runs the inference over
// b.js alone, a.js's nodes entering as their stored contributions, the way a
// refresh does: b.js calls a.js's make and calls write on its result, and
// a.js also exports other, which nothing calls. The inference asks for the
// stored returns of the callables it binds alone, make and then write, never
// other's, and binds `l.write()` as the full run does.
//
// Mutations that fail it: read every stored return each round (other's
// returns are asked for); drop the stored returns from a bound site's
// result (`l.write()` is unbound).
func TestOutsideReturnsAreReadForTheBoundCallablesAlone(t *testing.T) {
	result := func(i uint32) wire.Type { return wire.Type{Kind: wire.TypeResult, Index: i} }
	a := js("src/a.js",
		[]Decl{decl(model.NodeClass, "Logger", -1), method("write", 0, "Logger"),
			returning(exported(decl(model.NodeFunction, "make", -1)), result(0)),
			returning(exported(decl(model.NodeFunction, "other", -1)), result(1))},
		[]Site{
			{Caller: 2, Callee: "Logger", Shape: wire.ShapeConstructor, Candidates: []int32{0}},
			{Caller: 3, Callee: "Logger", Shape: wire.ShapeConstructor, Candidates: []int32{0}},
		}, nil)
	b := js("src/b.js", []Decl{decl(model.NodeFunction, "use", -1)},
		[]Site{
			{Caller: 0, Callee: "make", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeModule, Name: "./a.js"}},
			{Caller: 0, Callee: "write", Shape: wire.ShapeMember, Receiver: result(0)},
		}, nil)
	ctx := context.Background()
	u := goldenUnit(t, a, b)
	h, err := buildHierarchy(ctx, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	full, err := inferTypes(ctx, u, h, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer full.close()
	write := Ref{File: 1, Index: 1}
	calls := func(ty *types) []Ref {
		var out []Ref
		if err := ty.callsAt(write, func(x binding) error {
			out = append(out, x.Target)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	want := calls(full)
	if !slices.Equal(want, []Ref{{File: 0, Index: 1}}) {
		t.Fatalf("the full run binds l.write() to %v", want)
	}
	var asked []variable
	inputs := func(wants *pagination.SortedRun[variable]) (*pagination.SortedRun[contribution], error) {
		return collect(u.dir, contributionCodec, earliest, func(add func(contribution) error) error {
			return wants.Each(func(v variable) error {
				if v.Kind == varReturn {
					asked = append(asked, v)
				}
				return full.contributions.Each(func(c contribution) error {
					if cmpVar(c.Entry, v) != 0 || c.From.File == 1 {
						return nil
					}
					c.From = Ref{}
					return add(c)
				})
			})
		})
	}
	only, err := collect(u.dir, refCodec, nil, func(add func(Ref) error) error {
		return add(Ref{File: 1, Index: wholeFile})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer only.Close()
	part, err := inferTypes(ctx, u, h, only, inputs, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer part.close()
	if got := calls(part); !slices.Equal(got, want) {
		t.Errorf("the run over b.js binds l.write() to %v, the full run to %v", got, want)
	}
	// make is bound in the first round and write in the second; other never.
	if wantAsked := []variable{{Kind: varReturn, File: 0, Decl: 2}, {Kind: varReturn, File: 0, Decl: 1}}; !slices.Equal(asked, wantAsked) {
		t.Errorf("the run over b.js asked for the returns of %v, want those of the callables it bound, %v", asked, wantAsked)
	}
}
