package residency

import "testing"

// TestParseReadsKibibytesAndMissingIsUnavailable protects the reading every
// per-file need, worker base and parent resident figure is taken from. A field
// read in the wrong unit, or a missing field read as zero, reaches the need
// model, the ledger and the allocation as a figure the process never had.
// Mutations that fail it: dropping the ×1024, or returning a pointer to 0 for
// an absent field.
func TestParseReadsKibibytesAndMissingIsUnavailable(t *testing.T) {
	const text = "Name:\tcodectx\nVmPeak:\t  900000 kB\nVmHWM:\t   81920 kB\nVmRSS:\t   40960 kB\nRssFile:\t   30000 kB\nThreads:\t8\n"
	r := Parse(text)
	if r.Peak == nil || *r.Peak != 81920*1024 {
		t.Fatalf("VmHWM = %v, want %d", r.Peak, 81920*1024)
	}
	if r.Resident == nil || *r.Resident != 40960*1024 {
		t.Fatalf("VmRSS = %v, want %d", r.Resident, 40960*1024)
	}
	if r.Anon != nil {
		t.Fatalf("RssAnon is absent from the text but read as %d, not unavailable", *r.Anon)
	}
}
