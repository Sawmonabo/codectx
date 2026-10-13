package pagination

import (
	"slices"
	"strconv"
	"testing"
)

// TestACollapseReReadsOnlyWhatTheFanInRequires guards the collapse's first-pass
// sizing: past MaxSortFanIn runs, the sort merges just enough runs for the final
// merge to read exactly MaxSortFanIn, and the answer is exactly a stable sort of
// the arrivals (folded left to right when a fold is set), at every run count.
//
// Mutation: a collapse that merges every run in groups of MaxSortFanIn, level
// after level, re-reads all 18 runs at 18 (and every byte twice at 300) and
// fails the byte bounds. Mutation: a collapse that merges
// the globally smallest runs rather than a contiguous window reorders equal
// records and fails the order check (the payloads record arrival order).
func TestACollapseReReadsOnlyWhatTheFanInRequires(t *testing.T) {
	const bufRecords = 5
	for _, runs := range []int{MaxSortFanIn, MaxSortFanIn + 1, MaxSortFanIn + 2, 31, 40, 101, 300} {
		for _, fold := range []bool{false, true} {
			t.Run(strconv.Itoa(runs)+"/fold="+strconv.FormatBool(fold), func(t *testing.T) {
				arrivals := resumeArrivals(runs * bufRecords)
				s, err := NewExternalSort(t.TempDir(), bufRecords, encodeResume, decodeResume, compareResume)
				if err != nil {
					t.Fatalf("NewExternalSort: %v", err)
				}
				defer s.Close()
				if fold {
					s.WithFold(foldResume)
				}
				for _, v := range arrivals {
					if err := s.Add(v); err != nil {
						t.Fatalf("Add: %v", err)
					}
				}
				if err := s.spill(); err != nil {
					t.Fatalf("spill: %v", err)
				}
				if len(s.runs) != runs {
					t.Fatalf("the fixture spilled %d runs, want %d", len(s.runs), runs)
				}
				var total, largest int64
				for _, r := range s.runs {
					total += r.bytes
					largest = max(largest, r.bytes)
				}
				run, err := s.Sorted()
				if err != nil {
					t.Fatalf("Sorted: %v", err)
				}
				defer run.Close()

				want := slices.Clone(arrivals)
				slices.SortStableFunc(want, compareResume)
				if fold {
					want = foldStable(want)
				}
				if got := collectResume(t, run); !slices.Equal(got, want) {
					t.Fatalf("the collapsed sort answered %d records out of a stable sort's order:\ngot  %v\nwant %v", len(got), got, want)
				}

				// Merges re-read at most the runs Knuth's sizing names: none at
				// or under the fan-in, one first merge of (runs-2) mod
				// (fan-in-1) + 2 runs, then full merges of runs already
				// merged or not. Bounded by run counts times the largest
				// spilled run, with the merged runs' own bytes counted as the
				// spilled bytes they hold.
				switch {
				case runs <= MaxSortFanIn:
					if s.collapsedBytes != 0 {
						t.Fatalf("%d runs need no collapse, but it re-read %d bytes", runs, s.collapsedBytes)
					}
				case runs == MaxSortFanIn+2:
					if s.collapsedBytes > 3*largest {
						t.Fatalf("18 runs re-read %d bytes before the final merge, want at most three runs' worth (%d); every run's total is %d",
							s.collapsedBytes, 3*largest, total)
					}
				case runs > MaxSortFanIn*MaxSortFanIn:
					// One full level re-reads every run once; the last level
					// re-reads only the four merged runs its first group needs,
					// where a second full level would re-read every byte again.
					if s.collapsedBytes >= total+total/2 {
						t.Fatalf("%d runs re-read %d bytes before the final merge, want under one and a half times every run's total %d", runs, s.collapsedBytes, total)
					}
				default:
					if s.collapsedBytes >= total {
						t.Fatalf("%d runs re-read %d bytes before the final merge, as much as every run's total %d", runs, s.collapsedBytes, total)
					}
				}
			})
		}
	}
}

// foldStable folds each run of equal keys of a stably sorted slice left to
// right, which is what WithFold promises.
func foldStable(sorted []resumeRecord) []resumeRecord {
	var out []resumeRecord
	for _, v := range sorted {
		if n := len(out); n > 0 && compareResume(out[n-1], v) == 0 {
			out[n-1], _ = foldResume(out[n-1], v)
			continue
		}
		out = append(out, v)
	}
	return out
}
