package dependence

import (
	"github.com/Sawmonabo/codectx/internal/ledger"
)

// The stages one unit of this provider is made of. They are opened under
// whatever span the caller already has in its context -- the coordinator's
// unit span, in an index run -- so a reader sees where a unit's seconds went
// without this package knowing what opened that span.
const (
	stageParse  = "parse"
	stageExport = "export"
	stageImport = "import"
	// stagePart is one part of a subdivided unit: its own parse, export and
	// import nest under it, so a subdivided unit's cost is the sum of parts.
	stagePart = "part"
)

// measured turns a step's outcome into what its span records. Every figure the
// child's platform did not take is left absent: a nil pointer is a
// measurement nobody made, and a zero would be a measurement that says the
// child was free (Section 22).
func measured(o Outcome) ledger.Measured {
	m := ledger.Measured{CPUUnattributed: ledger.CPUUnsampled}
	if !o.CPUUnsampled {
		user, sys := o.CPUUserMS, o.CPUSysMS
		m.CPUUserMS, m.CPUSysMS, m.CPUUnattributed = &user, &sys, ledger.CPUAttributed
	}
	if !o.PeakUnsampled && o.PeakBytes >= 0 {
		peak := uint64(o.PeakBytes)
		m.PeakRSSBytes = &peak
	}
	if !o.IOUnsampled && o.ReadBytes >= 0 && o.WriteBytes >= 0 {
		read, write := uint64(o.ReadBytes), uint64(o.WriteBytes)
		m.ReadBytes, m.WriteBytes = &read, &write
	}
	return m
}

// unmeasured is what a span records when nothing of the step it brackets was
// ever measured: the child never started, or the step ended before it ran. It
// names why the CPU columns are null rather than leaving them unexplained --
// and it is not measured(Outcome{}), which would publish three zeros as
// figures somebody took.
func unmeasured() ledger.Measured {
	return ledger.Measured{CPUUnattributed: ledger.CPUUnsampled}
}

// bracketed is what a span that only brackets other spans records. Its children
// carry the child processes' own figures, so attributing them here as well
// would count the same processor time twice; the honest answer for the bracket
// itself is a null with its reason.
func bracketed() ledger.Measured {
	return ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}
}

// spanFailure is the typed error a classified step hands its span so the row
// carries the failure class the caller already holds, instead of a failed span
// with nothing said about it. It is handed to End and nowhere else: the
// provider's own recovery decides separately whether that class fails the
// unit, retries it or subdivides it.
func spanFailure(o Outcome, scopeKey string, r Reservation) error {
	if o.Class == FailureNone {
		return nil
	}
	return failure(o.Class, scopeKey, o, r)
}

// spanOutcome is what a step's class says about the span that ran it. Only the
// class decides: a step whose child exited non-zero and was classified is a
// failed span even though the provider may still recover the unit from it.
func spanOutcome(o Outcome) ledger.Outcome {
	if o.Class == FailureNone {
		return ledger.OutcomeOK
	}
	return ledger.OutcomeFailed
}
