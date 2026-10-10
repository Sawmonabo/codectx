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
// Mutation: count exports at 40 bytes instead of 39 in DecodeLink -> the
// decode fails. After the exports' count, n minimal exports leave 39n bytes
// plus 28 for the seven empty trailing lists' counts, and 40n exceeds that
// once n is 29 or more, which is why the message carries more than that.
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
