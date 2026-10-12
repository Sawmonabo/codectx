package link

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestArgumentFlowsMatchEachArgumentToItsParameter pins the argument-to-
// parameter rule of ADR-0017 decision 7 on each argument form a language
// writes. Each case names the wrong rule it catches.
func TestArgumentFlowsMatchEachArgumentToItsParameter(t *testing.T) {
	param := func(pos int32, name string, index uint32) paramInfo {
		return paramInfo{Pos: pos, Name: name, Index: index}
	}
	variadic := func(p paramInfo) paramInfo { p.Variadic = true; return p }
	keywordOnly := func(p paramInfo) paramInfo { p.KeywordOnly = true; return p }
	pos := func() Argument { return Argument{} }
	kw := func(name string) Argument { return Argument{Keyword: name} }
	spread := func() Argument { return Argument{Spread: wire.SpreadPositional} }
	kwSpread := func() Argument { return Argument{Spread: wire.SpreadKeyword} }

	cases := []struct {
		name          string
		params        []paramInfo
		args          []Argument
		receiverFirst bool
		want          [][2]int32 // (argument, parameter position)
		fails         string
	}{{
		name:   "positional",
		params: []paramInfo{param(0, "a", 1), param(1, "b", 2)},
		args:   []Argument{pos(), pos()},
		want:   [][2]int32{{0, 0}, {1, 1}},
		fails:  "an argument bound by anything but its position",
	}, {
		name:   "self is not a positional parameter",
		params: []paramInfo{param(0, "self", 0), param(1, "x", 1)},
		args:   []Argument{pos()},
		want:   [][2]int32{{0, 1}},
		fails:  "the receiver parameter counted as the first positional parameter of a call on an instance",
	}, {
		name:          "a call through the class passes the receiver first",
		params:        []paramInfo{param(0, "self", 0), param(1, "x", 1)},
		args:          []Argument{pos(), pos()},
		receiverFirst: true,
		want:          [][2]int32{{0, 0}, {1, 1}},
		fails:         "the first argument of `C.m(obj, x)` bound to x",
	}, {
		name:   "a keyword argument binds by name after the positional ones",
		params: []paramInfo{param(0, "a", 1), param(1, "b", 2), param(2, "c", 3)},
		args:   []Argument{pos(), kw("c")},
		want:   [][2]int32{{0, 0}, {1, 2}},
		fails:  "matching by position across a keyword argument binds `c=` to b",
	}, {
		name:   "a keyword-only parameter is reached by its keyword",
		params: []paramInfo{param(0, "a", 1), keywordOnly(param(1, "timeout", 2))},
		args:   []Argument{pos(), kw("timeout")},
		want:   [][2]int32{{0, 0}, {1, 1}},
		fails:  "a keyword-only parameter left out of keyword matching",
	}, {
		name:   "an unknown keyword binds **kwargs",
		params: []paramInfo{param(0, "a", 1), variadic(keywordOnly(param(1, "kwargs", 2)))},
		args:   []Argument{kw("other")},
		want:   [][2]int32{{0, 1}},
		fails:  "a keyword no parameter names dropped instead of reaching **kwargs",
	}, {
		name:   "extra positionals bind *args",
		params: []paramInfo{param(0, "a", 1), variadic(param(1, "args", 2))},
		args:   []Argument{pos(), pos(), pos()},
		want:   [][2]int32{{0, 0}, {1, 1}, {2, 1}},
		fails:  "arguments past the declared parameters dropped instead of reaching *args",
	}, {
		name:   "a spread binds the variadic parameter and so does what follows it",
		params: []paramInfo{param(0, "a", 1), param(1, "b", 2), variadic(param(2, "rest", 3))},
		args:   []Argument{pos(), spread(), pos()},
		want:   [][2]int32{{0, 0}, {1, 2}, {2, 2}},
		fails:  "a spread treated as one positional argument binds `...xs` to b",
	}, {
		name:   "a spread with no variadic parameter binds nothing",
		params: []paramInfo{param(0, "a", 1), param(1, "b", 2)},
		args:   []Argument{spread()},
		fails:  "a spread bound to the next positional parameter, whose value it may not hold",
	}, {
		name:   "**kw binds **kwargs",
		params: []paramInfo{param(0, "a", 1), variadic(keywordOnly(param(1, "kwargs", 2)))},
		args:   []Argument{pos(), kwSpread()},
		want:   [][2]int32{{0, 0}, {1, 1}},
		fails:  "a keyword spread bound by position",
	}, {
		name:   "an omitted default gets no edge",
		params: []paramInfo{param(0, "a", 1), param(1, "b", 2)},
		args:   []Argument{pos()},
		want:   [][2]int32{{0, 0}},
		fails:  "a parameter left at its default given an edge",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got [][2]int32
			s := &Site{Described: true, Arguments: c.args}
			err := argumentFlows(s, c.params, c.receiverFirst, func(arg, pos int32) error {
				got = append(got, [2]int32{arg, pos})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %v, want %v (fails under: %s)", got, c.want, c.fails)
			}
		})
	}
}
