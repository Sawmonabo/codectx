package worker

import (
	"fmt"
	"testing"
)

// TestParseStatusReadsKibibytesAndMissingIsUnavailable protects the reading
// every per-file need and worker base is taken from. A field read in the
// wrong unit, or a missing field read as zero, reaches the parent's need model
// and ledger as a figure the worker never had. Mutations that fail it:
// dropping the ×1024, or returning a pointer to 0 for an absent field.
func TestParseStatusReadsKibibytesAndMissingIsUnavailable(t *testing.T) {
	const text = "Name:\tcodectx\nVmPeak:\t  900000 kB\nVmHWM:\t   81920 kB\nVmRSS:\t   40960 kB\nRssFile:\t   30000 kB\nThreads:\t8\n"
	s := parseStatus(text)
	if s.peak == nil || *s.peak != 81920*1024 {
		t.Fatalf("VmHWM = %v, want %d", s.peak, 81920*1024)
	}
	if s.resident == nil || *s.resident != 40960*1024 {
		t.Fatalf("VmRSS = %v, want %d", s.resident, 40960*1024)
	}
	if s.anon != nil {
		t.Fatalf("RssAnon is absent from the text but read as %d, not unavailable", *s.anon)
	}
}

// fakeHost is a platform whose peak, resident set and heap return the test
// sets between boundaries.
type fakeHost struct {
	peakKiB, rssKiB uint64
	returns, resets bool
}

func (f *fakeHost) host() host {
	return host{
		status: func() (string, bool) {
			return fmt.Sprintf("VmHWM:\t%d kB\nVmRSS:\t%d kB\nRssAnon:\t%d kB\n", f.peakKiB, f.rssKiB, f.rssKiB/2), true
		},
		returnHeap: func() bool { return f.returns },
		resetPeak:  func() bool { return f.resets },
	}
}

// TestBoundaryNeedIsUnavailableUnlessMeasurable protects the rule that an
// unmeasurable need is absent, never zero or a wrapped negative. A need the
// worker could not scope to one file (the peak was not reset, or the heap kept
// the previous file's pages) would fold a censored figure into the p99 the
// ledger reserves, so the next such file is admitted below what it uses.
// Mutations that fail it: returning 0 when the reset failed, ignoring the
// heap-return result, or subtracting a base above the peak.
func TestBoundaryNeedIsUnavailableUnlessMeasurable(t *testing.T) {
	cases := []struct {
		name            string
		returns, resets bool
		peakKiB         uint64
		want            *uint64
	}{
		{name: "measured", returns: true, resets: true, peakKiB: 300, want: ptr(uint64(200 * 1024))},
		{name: "peak not reset", returns: true, resets: false, peakKiB: 300},
		{name: "heap not returned", returns: false, resets: true, peakKiB: 300},
		{name: "peak below base", returns: true, resets: true, peakKiB: 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeHost{peakKiB: 100, rssKiB: 100, returns: c.returns, resets: c.resets}
			m := meter{host: f.host()}
			start := m.boundary()
			if start.NeedBytes != nil {
				t.Fatalf("the first boundary has no file before it but reports need %d", *start.NeedBytes)
			}
			if start.BaseBytes == nil || *start.BaseBytes != 100*1024 {
				t.Fatalf("base = %v, want %d", start.BaseBytes, 100*1024)
			}
			f.peakKiB = c.peakKiB
			got := m.boundary().NeedBytes
			switch {
			case c.want == nil && got != nil:
				t.Fatalf("need = %d, want unavailable", *got)
			case c.want != nil && (got == nil || *got != *c.want):
				t.Fatalf("need = %v, want %d", got, *c.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
