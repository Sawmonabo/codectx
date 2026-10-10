package link

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestJoinRecordsRoundTrip carries a function taken as a value and a site
// that joins its signature through their encodings: the taken function is
// stored state a refresh reads back, and the cross-product matches the
// site's arguments against the function's parameters from what the two
// decode to. A parameter with no node, a variadic keyword-only one, a
// receiver-first value, and a keyword and a spread argument are each written
// after fields that existed before them, so a codec that skips or reorders
// one shifts every later byte.
//
// Mutation: decodeTaken reads the receiver-first flag before Params, or
// encodeParams drops a parameter's node -> the decoded record differs (or is
// refused as malformed).
func TestJoinRecordsRoundTrip(t *testing.T) {
	params := []boundParam{
		{paramInfo: paramInfo{Pos: 0, Name: "self"}, Node: "b.T.m.self"},
		{paramInfo: paramInfo{Pos: 1, Name: "x", Index: 1}},
		{paramInfo: paramInfo{Pos: 2, Name: "kw", Index: 2, Variadic: true, KeywordOnly: true}, Node: "b.T.m.kw"},
	}
	in := taken{Sig: "(int)(void)", From: "a", Target: target{File: "b", Node: "b.T.m"}, Params: encodeParams(params),
		ReceiverFirst: true}
	raw, err := encodeTaken(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeTaken(raw)
	if err != nil || out != in {
		t.Fatalf("the taken function decoded to %+v (%v), want %+v", out, err, in)
	}
	got, err := decodeParams(out.Params)
	if err != nil || !slices.Equal(got, params) {
		t.Fatalf("its parameters decoded to %+v (%v), want %+v", got, err, params)
	}

	args := []Argument{{Keyword: "kw"}, {Spread: wire.SpreadPositional}, {}}
	use := sigUse{Sig: "(int)(void)", Abstract: true, Args: argumentShapes(args), Params: encodeParams(params),
		Site: idBinding{Kind: FactCall, SiteFile: "a", Site: 4, TargetFile: "b", Target: "b.I.m", Detail: DetailAddressTaken,
			Dispatch: model.DispatchVirtual}}
	raw, err = encodeSigUse(use)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeSigUse(raw)
	if err != nil || back != use {
		t.Fatalf("the signature join decoded to %+v (%v), want %+v", back, err, use)
	}
	site, err := shapedSite(back.Args)
	if err != nil || !slices.EqualFunc(site.Arguments, args, func(a, b Argument) bool {
		return a.Keyword == b.Keyword && a.Spread == b.Spread
	}) {
		t.Fatalf("its arguments decoded to %+v (%v), want %+v", site.Arguments, err, args)
	}
}
