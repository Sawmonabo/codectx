package link

import (
	"cmp"
	"testing"
)

// TestSeekSpanReadsEveryRecordOfItsRange writes a sorted run of many blocks,
// each key repeated, with a padding string that makes a key's records cross
// block boundaries, and reads ranges at the start, inside, across block
// boundaries, past the end and of absent keys: each span yields exactly the
// records a full scan filtered by the range yields, in order.
//
// Mutations that fail it: start a span at the first block whose first
// record is in range instead of the block before it (a range that starts
// inside the previous block loses its first records); stop at the end of
// the first block read (a range across a boundary loses its tail).
func TestSeekSpanReadsEveryRecordOfItsRange(t *testing.T) {
	type rec struct {
		K   int32
		Pad string
	}
	c := codec[rec]{put: func(w *wbuf, r rec) { w.i32(r.K); w.str(r.Pad) },
		get: func(r *rbuf) rec { return rec{K: r.i32(), Pad: r.str()} },
		cmp: func(a, b rec) int { return cmp.Or(cmp.Compare(a.K, b.K), cmp.Compare(a.Pad, b.Pad)) }}
	dir := t.TempDir()
	var all []rec
	pad := string(make([]byte, 700))
	for k := int32(0); k < 400; k += 2 {
		for j := range 3 {
			all = append(all, rec{K: k, Pad: pad + string(rune('a'+j))})
		}
	}
	run, err := collect(dir, c, nil, func(add func(rec) error) error {
		for _, r := range all {
			if err := add(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	s, err := writeSeek(dir, run, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if len(s.first) < 4 {
		t.Fatalf("the run spans %d blocks, want several", len(s.first))
	}
	for _, r := range [][2]int32{{0, 0}, {0, 10}, {1, 1}, {44, 47}, {100, 300}, {398, 398}, {399, 500}, {-5, -1}, {0, 399}} {
		var got, want []rec
		for _, x := range all {
			if x.K >= r[0] && x.K <= r[1] {
				want = append(want, x)
			}
		}
		err := s.span(func(x rec) int {
			switch {
			case x.K < r[0]:
				return -1
			case x.K > r[1]:
				return 1
			}
			return 0
		}, func(x rec) error {
			got = append(got, x)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("the span %v yields %d records, want %d", r, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("the span %v yields %v at %d, want %v", r, got[i].K, i, want[i].K)
			}
		}
	}
}
