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
		{"c only, headers of both spellings present", []string{"a.c", "B.C", "a.h", "x.hpp", "y.hh", "z.hxx", "m.go"},
			HeaderPlan{First: "c", Fallback: "cpp", Basis: HeaderCOnly}},
		// Mutation: a C++ extension missing from the count leaves this
		// repository with no units.
		{"cpp only", []string{"a.cc", "b.cpp", "c.CXX", "a.h"},
			HeaderPlan{First: "cpp", Fallback: "c", Basis: HeaderCPPOnly}},
		// Mutation: letting the majority decide parses these headers as C.
		{"mixed parses c++ first", []string{"a.c", "b.c", "c.c", "d.cpp"},
			HeaderPlan{First: "cpp", Fallback: "c", Basis: HeaderMixed}},
		{"no units parses c++ first", []string{"a.h", "b.hpp", "main.go"},
			HeaderPlan{First: "cpp", Fallback: "c", Basis: HeaderNoUnits}},
	} {
		if got := census(tc.paths...).Header(); got != tc.want {
			t.Errorf("%s: plan %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// A census change that flips the header grammar must invalidate every
// header's unit; one that does not must not reparse them all.
func TestCensusChangeInvalidatesHeadersOnlyWhenTheirGrammarChanges(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, next Census
		want      bool
	}{
		// Mutation: comparing only the basis, or only presence of C, misses
		// the first C++ file of a C repository.
		{"first c++ unit in a c repository", Census{C: 5}, Census{C: 5, CPP: 1}, true},
		{"last c++ unit leaves a mixed repository", Census{C: 5, CPP: 1}, Census{C: 5}, true},
		{"last c unit leaves a c repository", Census{C: 1}, Census{}, true},
		// Mutation: comparing the counts reparses every header on every
		// added source file.
		{"another c unit", Census{C: 5}, Census{C: 6}, false},
		// Mutation: comparing the whole plan reparses on a basis change that
		// keeps the grammar.
		{"c++ repository gains c", Census{CPP: 2}, Census{C: 1, CPP: 2}, false},
		{"headers only gains c++", Census{}, Census{CPP: 1}, false},
	} {
		if got := tc.old.ChangesHeaders(tc.next); got != tc.want {
			t.Errorf("%s: changes %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Keeping the worse parse publishes facts from the tree with more errors;
// running the fallback on a clean header doubles its parse for nothing.
func TestHeaderFallbackKeepsTheParseWithFewerErrorBytes(t *testing.T) {
	plan := HeaderPlan{First: "cpp", Fallback: "c", Basis: HeaderMixed}
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

// Moving ".h" into the C++ declaration must not move any other extension,
// or ".h" out of the extension answer rows without a census rely on.
func TestHeaderCandidatesLeaveEveryOtherExtensionAlone(t *testing.T) {
	for _, l := range All {
		for _, ext := range l.Extensions {
			got, ok := ByExtension("x" + ext)
			cands := Candidates("x" + ext)
			if ext == ".h" {
				// Mutation: dropping ".h" from either declaration.
				if !ok || got.Name != "c" || len(cands) != 2 || cands[0].Name != "c" || cands[1].Name != "cpp" {
					t.Errorf(".h: ByExtension %q, candidates %d; want c and [c cpp]", got.Name, len(cands))
				}
				continue
			}
			// Mutation: an extension declared by a second grammar.
			if !ok || got.Name != l.Name || len(cands) != 1 || cands[0].Name != l.Name {
				t.Errorf("%s: ByExtension %q, %d candidates; want %s alone", ext, got.Name, len(cands), l.Name)
			}
		}
	}
}
