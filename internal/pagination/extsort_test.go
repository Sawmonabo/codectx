package pagination

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// resumeRecord has a key many records share and a payload that names the
// arrival. Equal keys are what make the test discriminating: with unique keys
// the merge's run-index tie-break is unobservable, and an adopted run placed
// AFTER the new runs would still produce the right answer.
type resumeRecord struct {
	Key     int    `json:"k"`
	Payload string `json:"p"`
}

func encodeResume(v resumeRecord) ([]byte, error) { return json.Marshal(v) }

func decodeResume(b []byte) (resumeRecord, error) {
	var v resumeRecord
	err := json.Unmarshal(b, &v)
	return v, err
}

func compareResume(a, b resumeRecord) int {
	switch {
	case a.Key < b.Key:
		return -1
	case a.Key > b.Key:
		return 1
	}
	return 0
}

// foldResume concatenates the payloads of equal records in the order they
// reach the fold, so the survivor records the ARRIVAL ORDER of every record it
// absorbed. It is neither commutative nor associative-with-order, which is
// exactly the fold shape WithFold documents and the one an out-of-order adopted
// run would corrupt.
func foldResume(a, b resumeRecord) (resumeRecord, error) {
	a.Payload += "+" + b.Payload
	return a, nil
}

func resumeArrivals(n int) []resumeRecord {
	out := make([]resumeRecord, 0, n)
	for i := range n {
		out = append(out, resumeRecord{Key: (i * 7) % 37, Payload: strconv.Itoa(i)})
	}
	return out
}

func collectResume(t *testing.T, run *SortedRun[resumeRecord]) []resumeRecord {
	t.Helper()
	var out []resumeRecord
	if err := run.Each(func(v resumeRecord) error {
		out = append(out, v)
		return nil
	}); err != nil {
		t.Fatalf("Each: %v", err)
	}
	return out
}

// TestAdoptRunsResumesAnInterruptedSortIdentically is the invariant behind the
// adopt constructor: a sort stopped by a page deadline and resumed through
// AdoptRuns must answer exactly what one uninterrupted sort of the same
// arrival sequence answers -- same records, same order, same fold survivors.
func TestAdoptRunsResumesAnInterruptedSortIdentically(t *testing.T) {
	arrivals := resumeArrivals(200)
	const bufRecords = 5 // 40 runs over 200 records, so collapse() really runs
	const interruptAfter = 83

	whole, err := NewExternalSort(t.TempDir(), "whole-", bufRecords,
		encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	defer whole.Close()
	whole.WithFold(foldResume)
	for _, v := range arrivals {
		if err := whole.Add(v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	wholeRun, err := whole.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer wholeRun.Close()
	want := collectResume(t, wholeRun)

	dir := t.TempDir()
	first, err := NewExternalSort(dir, "resume-", bufRecords,
		encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	first.WithFold(foldResume)
	for _, v := range arrivals[:interruptAfter] {
		if err := first.Add(v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	runs, err := first.Detach()
	if err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("Detach returned no runs; the interruption would have nothing to resume from")
	}
	// Ownership moved: the interrupted sort's own Close must not remove the
	// runs the continuation is going to adopt.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, name := range runs {
		if _, err := os.Stat(name); err != nil {
			t.Fatalf("detached run %s was removed by the detached sort: %v", name, err)
		}
	}

	second, err := AdoptRuns(dir, "resume-", bufRecords, runs,
		encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("AdoptRuns: %v", err)
	}
	defer second.Close()
	second.WithFold(foldResume)
	for _, v := range arrivals[interruptAfter:] {
		if err := second.Add(v); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	secondRun, err := second.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer secondRun.Close()
	got := collectResume(t, secondRun)

	if len(got) != len(want) {
		t.Fatalf("resumed sort answered %d records, the uninterrupted sort %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resumed sort diverges at record %d of %d:\n got %+v\nwant %+v",
				i, len(want), got[i], want[i])
		}
	}
	if secondRun.Len() != wholeRun.Len() {
		t.Fatalf("resumed SortedRun.Len is %d, the uninterrupted one %d",
			secondRun.Len(), wholeRun.Len())
	}
}
