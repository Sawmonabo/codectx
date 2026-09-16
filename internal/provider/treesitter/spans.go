package treesitter

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/process"
)

// stageStructuralParse is the stage the structural parse of a run is recorded
// under: one span per parser worker, under one total for the stage. A file is
// never a span -- a run parses tens of thousands of them and a span is a thing
// that costs seconds -- so a worker's files are counted on its span's own
// counters instead.
const stageStructuralParse = "structural_parse"

// stageTotal is the parent span of one run's structural parse and the context
// its workers' spans are opened under. That context carries the run and the
// total, and nothing else: a worker is started by one unit, is reused by the
// units that follow it and outlives them all, so its span cannot hang off the
// unit span that happened to start it, and a cancelable context would end the
// span's parentage with that unit's work.
//
// refs is how many units are parsing under this run. The total is opened by
// the first and ended by the last, which is the stage: exactly as long as
// there is parse work in flight.
type stageTotal struct {
	ctx  context.Context
	span *ledger.Span
	refs int
}

// measured turns one worker child's result into what the span that ran it
// records. Every figure the platform did not take is left absent: a nil
// pointer is a measurement nobody made, and a zero would say the child was
// free (Section 22).
func measured(res process.Result) ledger.Measured {
	m := ledger.Measured{CPUUnattributed: ledger.CPUUnsampled}
	if !res.CPUUnsampled {
		user, sys := res.CPUUserMillis, res.CPUSysMillis
		m.CPUUserMS, m.CPUSysMS, m.CPUUnattributed = &user, &sys, ledger.CPUAttributed
	}
	if !res.TreeUnsampled && res.PeakTreeBytes >= 0 {
		peak := uint64(res.PeakTreeBytes)
		m.PeakRSSBytes = &peak
	}
	if !res.IOUnsampled && res.ReadBytes >= 0 && res.WriteBytes >= 0 {
		read, write := uint64(res.ReadBytes), uint64(res.WriteBytes)
		m.ReadBytes, m.WriteBytes = &read, &write
	}
	return m
}
