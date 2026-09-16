package scip

import (
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/process"
)

// The stages one unit of this provider is made of: the indexer's own run, and
// the import of what it produced. They are opened under whatever span the
// caller's context already carries -- the coordinator's unit span, in an index
// run -- so this package never names what opened it.
const (
	stageRun    = "run"
	stageImport = "import"
)

// measured turns one child's result into what the span that ran it records.
// Every figure the platform did not take is left absent: a nil pointer is a
// measurement nobody made, and a zero would say the child was free
// (Section 22).
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
