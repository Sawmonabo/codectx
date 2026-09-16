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
// exactly the folding contract WithFold documents and the one an out-of-order adopted
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

	whole, err := NewExternalSort(t.TempDir(), bufRecords,
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
	first, err := NewExternalSort(dir, bufRecords,
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
	state := t.TempDir()
	runs, err := first.DetachTo(state, func(i int) string { return "run" + strconv.Itoa(i) })
	if err != nil {
		t.Fatalf("DetachTo: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("DetachTo returned no runs; the interruption would have nothing to resume from")
	}
	// Ownership moved: the interrupted sort's own Close must not remove the
	// runs the continuation is going to adopt.
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, ref := range runs {
		st, err := os.Stat(ref.Path)
		if err != nil {
			t.Fatalf("detached run %s was removed by the detached sort: %v", ref.Path, err)
		}
		if ref.Bytes <= 0 || ref.Bytes > st.Size() {
			t.Fatalf("detached run %s recorded %d bytes of a %d-byte file", ref.Path, ref.Bytes, st.Size())
		}
	}

	second, err := AdoptRuns(state, bufRecords, runs,
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

// TestAPooledRunFileNeverYieldsThePreviousTenantsRecords is the invariant the
// scratch pool makes load-bearing. A run file is taken from the pool and never
// truncated, so a short run written into a surface a long run left behind has
// the long run's bytes sitting right after its own. A reader that stopped at
// end of file instead of at the run's recorded length would decode those bytes
// as records of THIS sort and merge them into the answer: a query served
// records from a previous, unrelated query, which no error would report.
//
// Both halves are covered, because both read run files: the merge (the k-way
// read of the spilled runs) and SortedRun.Each (the re-readable walk of the
// merged output).
func TestAPooledRunFileNeverYieldsThePreviousTenantsRecords(t *testing.T) {
	dir := t.TempDir()
	// A small run buffer so both sorts really spill and really merge.
	const bufRecords = 4

	long, err := NewExternalSort(dir, bufRecords, encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	for i := range 200 {
		if err := long.Add(resumeRecord{Key: 1000 + i, Payload: "the previous tenant's record"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	longRun, err := long.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	if long.SpilledRuns() == 0 {
		t.Fatal("the long sort did not spill; the pool would hold no surface to inherit")
	}
	// Both the runs and the merged output go back to the pool, at length.
	if err := longRun.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := long.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	short, err := NewExternalSort(dir, bufRecords, encodeResume, decodeResume, compareResume)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	const shortCount = 9
	for i := range shortCount {
		if err := short.Add(resumeRecord{Key: i, Payload: "mine"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	shortRun, err := short.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer shortRun.Close()
	defer short.Close()

	got := collectResume(t, shortRun)
	if len(got) != shortCount {
		t.Fatalf("the sort answered %d records, want %d: it read past its own runs into the bytes the pool's previous tenant left", len(got), shortCount)
	}
	for _, r := range got {
		if r.Payload != "mine" {
			t.Fatalf("the sort answered a record of the pool's previous tenant: %+v", r)
		}
	}
	if shortRun.Len() != shortCount {
		t.Fatalf("SortedRun.Len is %d, want %d", shortRun.Len(), shortCount)
	}
}
