package pagination

import (
	"slices"
	"testing"
)

// markedSort feeds arrivals into a folding sort with a run buffer of five,
// setting a mark before arrival at (so the mark falls inside a buffer that
// later spills), and answers the sort.
func markedSort(t *testing.T, arrivals []resumeRecord, at int) *ExternalSort[resumeRecord] {
	t.Helper()
	s, err := NewExternalSort(t.TempDir(), 5, encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	s.WithFold(foldResume)
	for i, v := range arrivals {
		if i == at {
			if err := s.Mark(); err != nil {
				t.Fatalf("Mark: %v", err)
			}
		}
		if err := s.Add(v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return s
}

func answer(t *testing.T, s *ExternalSort[resumeRecord]) []resumeRecord {
	t.Helper()
	run, err := s.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer run.Close()
	return collectResume(t, run)
}

// TestAMarkedSortKeepsOrRollsBackExactly pins Mark: a sort whose mark is kept
// answers exactly what an unmarked sort of the same arrivals answers (same
// records, order and fold survivors, whose payloads spell arrival order), and
// a sort rolled back answers exactly what a sort of the arrivals before the
// mark answers, then goes on taking records as if the rest never came. The
// mark falls inside a buffer that spills several times after it.
//
// Mutations caught: a spill that writes a buffer holding both sides of the
// mark as one run (the roll back then keeps records from after the mark, or
// drops some from before it); roll back leaving the buffer or the runs after
// the mark in place; a split spill writing the later half first (the fold
// survivors then spell the wrong arrival order).
func TestAMarkedSortKeepsOrRollsBackExactly(t *testing.T) {
	arrivals := resumeArrivals(122) // two records still buffered at the roll back
	const at = 43                   // inside the ninth five-record buffer

	plain := markedSort(t, arrivals, -1)
	want := answer(t, plain)

	kept := markedSort(t, arrivals, at)
	kept.Keep()
	if got := answer(t, kept); !slices.Equal(got, want) {
		t.Fatalf("a kept mark changed the answer:\n got %v\nwant %v", got, want)
	}

	before := markedSort(t, arrivals[:at], -1)
	tail := resumeArrivals(150)[122:]
	for _, v := range tail {
		if err := before.Add(v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	wantBack := answer(t, before)

	back := markedSort(t, arrivals, at)
	if err := back.RollBack(); err != nil {
		t.Fatalf("RollBack: %v", err)
	}
	for _, v := range tail {
		if err := back.Add(v); err != nil {
			t.Fatalf("Add after RollBack: %v", err)
		}
	}
	if back.Len() != int64(at+len(tail)) {
		t.Fatalf("a rolled-back sort counts %d records, want %d", back.Len(), at+len(tail))
	}
	if got := answer(t, back); !slices.Equal(got, wantBack) {
		t.Fatalf("a rolled-back sort answered other records than the arrivals before its mark:\n got %v\nwant %v",
			got, wantBack)
	}
}
