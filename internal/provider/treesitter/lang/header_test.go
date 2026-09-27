package lang

import (
	"errors"
	"testing"
)

// A header parsed with the grammar the repository does not use publishes
// facts from an error-riddled tree under a fingerprint that claims them.
// Each row names the mutation that fails it.
func TestHeaderGrammarFollowsTheRepositoryCensus(t *testing.T) {
	census := func(paths ...string) Census {
		var c Census
		for _, p := range paths {
			c.Add(p)
		}
		return c
	}
	for _, tc := range []struct {
		name  string
		paths []string
		want  HeaderPlan
	}{
		// Mutation: counting a header as a translation unit makes this
		// C repository mixed, and its headers C++.
		{"c only, headers of both spellings present", []string{"a.c", "a.h", "x.hpp", "y.hh", "z.hxx", "m.go"},
			HeaderPlan{First: "c", Fallback: "cpp"}},
		// Mutation: a C++ extension missing from the count leaves this
		// repository with no units.
		{"cpp only", []string{"a.cc", "b.cpp", "c.cxx", "a.h"},
			HeaderPlan{First: "cpp", Fallback: "c"}},
		// Mutation: folding the extension's case counts the C++ unit `.C`
		// as C and parses this repository's headers as C.
		{"an upper-case .C unit is not C", []string{"main.C", "a.h"},
			HeaderPlan{First: "cpp", Fallback: "c"}},
		// Mutation: letting the majority decide parses these headers as C.
		{"mixed parses c++ first", []string{"a.c", "b.c", "c.c", "d.cpp"},
			HeaderPlan{First: "cpp", Fallback: "c"}},
		{"no units parses c++ first", []string{"a.h", "b.hpp", "main.go"},
			HeaderPlan{First: "cpp", Fallback: "c"}},
	} {
		if got := census(tc.paths...).Header(); got != tc.want {
			t.Errorf("%s: plan %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Keeping the worse parse publishes facts from the tree with more errors;
// running the fallback on a clean header doubles its parse for nothing.
func TestHeaderFallbackKeepsTheParseWithFewerErrorBytes(t *testing.T) {
	plan := HeaderPlan{First: "cpp", Fallback: "c"}
	for _, tc := range []struct {
		name            string
		first, fallback ParseErrors
		kept            string
		reason          HeaderReason
		fallbackRuns    int
	}{
		// Mutation: always running the fallback.
		{"clean first is kept alone", ParseErrors{}, ParseErrors{}, "cpp", HeaderClean, 0},
		// Mutation: keeping the first whenever it has errors.
		{"fewer fallback bytes win", ParseErrors{Any: true, Bytes: 40}, ParseErrors{Any: true, Bytes: 3}, "c", HeaderFallbackKept, 1},
		// Mutation: <= in place of < hands ties to the fallback.
		{"equal bytes keep the first", ParseErrors{Any: true, Bytes: 7}, ParseErrors{Any: true, Bytes: 7}, "cpp", HeaderFirstKept, 1},
		// Mutation: an inverted comparison.
		{"more fallback bytes lose", ParseErrors{Any: true, Bytes: 7}, ParseErrors{Any: true, Bytes: 90}, "cpp", HeaderFirstKept, 1},
		// Mutation: triggering the fallback on error bytes alone never
		// repairs a header whose only errors are missing nodes.
		{"missing nodes only lose to a clean fallback", ParseErrors{Any: true}, ParseErrors{}, "c", HeaderFallbackKept, 1},
		{"missing nodes only keep against missing nodes", ParseErrors{Any: true}, ParseErrors{Any: true}, "cpp", HeaderFirstKept, 1},
	} {
		runs := 0
		got, err := plan.Choose(tc.first, func() (ParseErrors, error) { runs++; return tc.fallback, nil })
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Kept != tc.kept || got.Reason != tc.reason || runs != tc.fallbackRuns || got.First != "cpp" || got.FirstErr != tc.first {
			t.Errorf("%s: kept %s (%s) after %d fallback parses, disclosed %+v; want %s (%s) after %d",
				tc.name, got.Kept, got.Reason, runs, got, tc.kept, tc.reason, tc.fallbackRuns)
		}
		if runs > 0 && got.Fallback != tc.fallback {
			t.Errorf("%s: disclosed fallback errors %+v, want %+v", tc.name, got.Fallback, tc.fallback)
		}
	}
	// Mutation: keeping the first parse when the fallback parse failed
	// hides the failure behind a disclosure that says it was compared.
	boom := errors.New("worker lost")
	if _, err := plan.Choose(ParseErrors{Any: true, Bytes: 1}, func() (ParseErrors, error) { return ParseErrors{}, boom }); !errors.Is(err, boom) {
		t.Errorf("a failed fallback parse answered %v, want %v", err, boom)
	}
}
