package dependence

import (
	"log/slog"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
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

// overran records on a step's span that its process tree peaked ABOVE the
// reservation the step was admitted against. It is the same comparison the
// resources block's count is, made here because this is where both figures are
// already known: the peak the child's sampler observed, and the bytes this
// step reserved. No second sampler and no second observation.
//
// The span's outcome is untouched. A step that overran its reservation and
// produced its graph SUCCEEDED; what the row states is the accounting fact,
// which is what makes a run that froze its host readable afterwards instead of
// leaving nothing behind at all. The code and the reason are carried on the
// span the way every other unavailable or failed row carries them, so the
// existing stage surfaces render it without knowing about this at all.
//
// A step whose tree was never sampled records nothing: nothing was measured,
// which is not a peak below the reservation and must never be read as one.
// reservedBytes is the reservation the UNIT holds -- Bytes of the admitted
// reservation, not the individual step's figure -- which is the same figure
// the unit's row on the resources block is compared against.
func overran(m *ledger.Measured, scopeKey string, reservedBytes int64) {
	if m.PeakRSSBytes == nil || reservedBytes <= 0 || int64(*m.PeakRSSBytes) <= reservedBytes {
		return
	}
	if m.DiagnosticCode == "" {
		m.DiagnosticCode = model.CodeResourceLimit
	}
	if m.Failure == "" {
		m.Failure = "the process tree of " + scopeKey + " peaked at " + itoa(int64(*m.PeakRSSBytes)) +
			" bytes, above the " + itoa(reservedBytes) + " bytes it was admitted against"
	}
	slog.Warn("a dependence step peaked above its reservation", "component", component,
		"scope", scopeKey, "reservation_bytes", reservedBytes, "tree_peak_bytes", int64(*m.PeakRSSBytes))
}

// measured is what the span of one step run under this admission records: the
// child's own figures, and the over-reservation marker when the step succeeded
// and its tree peaked above the reservation the unit holds. A step that failed
// keeps its own class: a crash or a timeout above the reservation is that
// crash or that timeout, and the overrun must not take its code and reason.
// scope names the step's work in the marker's reason.
func (a *admitted) measured(o Outcome, scope string) ledger.Measured {
	m := measured(o)
	if o.Class == FailureNone {
		overran(&m, scope, a.res.Bytes())
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
