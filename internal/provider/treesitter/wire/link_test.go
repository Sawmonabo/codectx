package wire

import (
	"slices"
	"testing"
)

// TestLinkOfMinimalExportsRoundTrips decodes a link message whose exports
// are all as small as the encoding allows: no receiver, no signature, an
// unknown return type, and nothing else in the message. A file of small
// untyped functions and no calls (a Python or JavaScript module of `def f():
// pass`) sends exactly this. The decoder refuses a list whose count times
// its element's smallest size exceeds the bytes left, so a smallest export
// counted larger than the layout writes refuses this valid message, the
// worker is retired as broken, the structural unit fails and the generation
// fails with it.
//
// Mutation: count exports at 55 bytes instead of 54 in DecodeLink -> the
// decode fails. After the exports' count, n minimal exports leave 54n bytes
// plus 36 for the nine empty trailing lists' counts, and 55n exceeds that
// once n is 37 or more, which is why the message carries more than that.
func TestLinkOfMinimalExportsRoundTrips(t *testing.T) {
	var in Link
	for i := range uint32(64) {
		in.Exports = append(in.Exports, Export{Name: Span{Start: 10 * i, End: 10*i + 1}, Params: i % 3,
			Static: i%2 == 0, Final: i%5 == 0})
	}
	out, err := DecodeLink(AppendLink(nil, &in))
	if err != nil {
		t.Fatalf("DecodeLink refused a message of %d minimal exports: %v", len(in.Exports), err)
	}
	if !slices.Equal(out.Exports, in.Exports) {
		t.Fatalf("exports changed across the round trip:\n got %+v\nwant %+v", out.Exports, in.Exports)
	}
	if len(out.Supertypes)+len(out.Sites)+len(out.Values)+len(out.Reexports)+len(out.Unions) != 0 {
		t.Fatalf("a message with only exports decoded other entries: %+v", out)
	}
}

// TestLinkFactsBeyondCallsRoundTrip carries every field the linker's facts
// beyond calls read through one encode and decode: an export's override mark,
// alias, property and exported marks, identity and trait, a Go conversion, a
// value's declaration, a supertype's interface mark, an argument's reads, keyword and spread, a dynamic import's
// fragments and constant flag, a parameter's marks, and a computed site with
// its arguments. Each is written after fields that existed before it, so a
// codec that skips or reorders one shifts every later byte.
//
// Mutation: AppendLink drops the alias, the exported mark, the identity, the
// trait, a conversion's interface, a value's declaration or a supertype's
// interface mark, or
// DecodeLink reads a computed site's arguments without their reads -> the
// decoded message differs (or is refused as malformed).
func TestLinkFactsBeyondCallsRoundTrip(t *testing.T) {
	in := Link{
		Exports: []Export{
			{Name: Span{Start: 1, End: 4}, Receiver: "C", Override: true},
			{Name: Span{Start: 9, End: 12}, Alias: "log_impl"},
			{Name: Span{Start: 13, End: 16}, Exported: true},
			{Name: Span{Start: 17, End: 19}, Property: true},
			{Name: Span{Start: 91, End: 92}, Receiver: "T", Identity: "( :st.ID ) ( .error )"},
			{Name: Span{Start: 24, End: 27}, Receiver: "T", Override: true, Trait: "fmt::Display"},
		},
		Conversions: []Conversion{{Type: "T", Interface: "io.Reader"}, {Type: "U", Interface: "fmt.Stringer"}},
		Values: []Value{{At: Span{Start: 81, End: 90}, Field: "run", Type: Type{Kind: TypeFunction, Name: "obj.run"},
			Declares: Span{Start: 13, End: 16}}},
		Supertypes: []Supertype{{Type: "A", Super: "Thread"}, {Type: "A", Super: "Runnable", Interface: true}},
		Sites: []Site{{Name: Span{Start: 20, End: 21}, Shape: ShapeBare, Args: []Type{{}, {Kind: TypeNamed, Name: "T"}},
			Arguments: []Argument{
				{At: Span{Start: 22, End: 23}, Reads: []Span{{Start: 2, End: 3}, {Start: 5, End: 6}}},
				{At: Span{Start: 27, End: 29}, Keyword: "kw", Spread: SpreadKeyword},
			}}},
		Parameters: []Parameter{
			{Callable: Span{Start: 1, End: 4}, Name: Span{Start: 5, End: 6}, Index: 1, Variadic: true},
			{Callable: Span{Start: 1, End: 4}, Name: Span{Start: 7, End: 8}, Index: 2, Variadic: true, KeywordOnly: true},
		},
		DynamicImports: []DynamicImport{
			{At: Span{Start: 30, End: 50}, Fragments: []string{"./x/", ".js"}},
			{At: Span{Start: 51, End: 60}, Fragments: []string{"./m"}, Constant: true},
			{At: Span{Start: 61, End: 70}, Fragments: []string{}},
		},
		Computed: []Site{{Name: Span{Start: 71, End: 77}, Shape: ShapeIndirect, Args: []Type{{}},
			Arguments: []Argument{{At: Span{Start: 78, End: 80}, Spread: SpreadPositional, Reads: []Span{{Start: 5, End: 6}}}}}},
	}
	out, err := DecodeLink(AppendLink(nil, &in))
	if err != nil {
		t.Fatalf("DecodeLink refused the message: %v", err)
	}
	if !slices.Equal(out.Exports, in.Exports) || !slices.Equal(out.Parameters, in.Parameters) {
		t.Fatalf("exports or parameters changed:\n got %+v %+v\nwant %+v %+v", out.Exports, out.Parameters, in.Exports, in.Parameters)
	}
	if !slices.Equal(out.Values, in.Values) {
		t.Fatalf("values changed:\n got %+v\nwant %+v", out.Values, in.Values)
	}
	if !slices.Equal(out.Supertypes, in.Supertypes) {
		t.Fatalf("supertypes changed:\n got %+v\nwant %+v", out.Supertypes, in.Supertypes)
	}
	if !slices.Equal(out.Conversions, in.Conversions) {
		t.Fatalf("conversions changed:\n got %+v\nwant %+v", out.Conversions, in.Conversions)
	}
	sameSites := func(a, b []Site) bool {
		return slices.EqualFunc(a, b, func(x, y Site) bool {
			return x.Name == y.Name && x.Shape == y.Shape && slices.Equal(x.Args, y.Args) &&
				slices.EqualFunc(x.Arguments, y.Arguments, func(p, q Argument) bool {
					return p.At == q.At && p.Keyword == q.Keyword && p.Spread == q.Spread && slices.Equal(p.Reads, q.Reads)
				})
		})
	}
	if !sameSites(out.Sites, in.Sites) || !sameSites(out.Computed, in.Computed) {
		t.Fatalf("sites changed:\n got %+v %+v\nwant %+v %+v", out.Sites, out.Computed, in.Sites, in.Computed)
	}
	if !slices.EqualFunc(out.DynamicImports, in.DynamicImports, func(x, y DynamicImport) bool {
		return x.At == y.At && x.Constant == y.Constant && slices.Equal(x.Fragments, y.Fragments)
	}) {
		t.Fatalf("dynamic imports changed:\n got %+v\nwant %+v", out.DynamicImports, in.DynamicImports)
	}
}
