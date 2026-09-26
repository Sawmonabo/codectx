package dependence

// What each unit of this process was given and what it actually used.
//
// The governor's arithmetic is only checkable from outside if the three
// figures travel together: the reservation the unit was admitted against, the
// heap cap it ran under, and the peak resident memory its process tree
// reached. They are not capability details and must not be -- a capability
// row's details fold into the analysis key, and an observed peak is different
// on every run, so publishing it there would key two identical runs
// differently. They are process accounting, which is what the Section 23
// resources block is, and that is where they are disclosed.
//
// The record is per process and not per generation on purpose: what an
// operator diagnosing a slow or serialized run needs is what THIS process has
// been handing its analyzers, which outlives no restart and belongs to no
// stored generation.

import (
	"context"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// MaxObservedUnits bounds the record's rows at the one figure a bounded
// response already carries: a record wider than the response could not be
// delivered whole anyway. ADR-0010 decision 4 promises that EVERY unit
// discloses its reservation, its ceiling and its observed peak, so a unit the
// rows cannot hold is not dropped in silence: it is counted as omitted, and if
// it overran its reservation it is counted in the overrun total as well
// (ObservedTotals), which therefore never under-reports.
const MaxObservedUnits = model.MaxRecordsPerResult

// UnitMemory is one unit's memory accounting: the latest run of its scope in
// this process.
type UnitMemory struct {
	ScopeKey string
	Family   Family
	// ReservationBytes is what the unit held when it ended -- the reservation
	// it was admitted at, or the larger one it was re-admitted at for its
	// out-of-memory retry -- HeapCapBytes the cap its parse last ran under and
	// ExportHeapCapBytes its export's.
	ReservationBytes   int64
	HeapCapBytes       int64
	ExportHeapCapBytes int64
	// AllocationBytes is the machine-derived allocation the cap was bounded
	// by, and AllocationObserved whether the host exposed available memory.
	AllocationBytes    int64
	AllocationObserved bool
	// PeakBytes is the highest peak any SAMPLED step's process tree reached,
	// and PeakUnsampled reports that no step of the unit was sampled at all --
	// which is why it is a flag and not a zero. One unsampled step does not
	// hide what a sampled one measured.
	PeakBytes     int64
	PeakUnsampled bool
	// Overran is PeakBytes above ReservationBytes on a sampled unit, the same
	// comparison model.AnalyzerUnit.OverranReservation makes of the row.
	Overran bool
}

var observed struct {
	mu    sync.Mutex
	units []UnitMemory
	at    map[string]int
	// omitted counts the unit runs whose scope the rows had no room for, and
	// omittedOverruns those of them that overran. A scope past the bound has
	// no row to deduplicate against, so each of its runs is counted.
	omitted, omittedOverruns int64
}

// admitted is one unit's single admission, as the provider holds it while the
// unit runs, and what the unit's steps used of it.
type admitted struct {
	// res is the reservation the unit holds now: the one it was admitted at,
	// or the one grow exchanged it for.
	res     Reservation
	readmit func(context.Context, Reservation) error
	// peak is the highest sampled tree peak of any step, and sampled whether
	// any step was sampled at all.
	peak    int64
	sampled bool
}

// grow exchanges the unit's admission for one at r, and makes r the
// reservation every later step runs, is compared and is recorded against.
func (a *admitted) grow(ctx context.Context, r Reservation) error {
	if err := a.readmit(ctx, r); err != nil {
		return err
	}
	a.res = r
	return nil
}

// observe folds one completed step's tree peak into the unit's. Parse and
// export never run at the same time, so the unit's footprint is the greatest
// sampled step, which is exactly what the reservation claimed. A step the
// platform did not sample contributes nothing: it is not a peak of zero.
func (a *admitted) observe(o Outcome) {
	if o.PeakUnsampled {
		return
	}
	a.sampled = true
	a.peak = max(a.peak, o.PeakBytes)
}

// record stores what the unit held and used as its scope's row. It runs once
// per unit, on every path out of the import, so the row describes the unit's
// latest run whole.
func (a *admitted) record(u Unit) {
	row := UnitMemory{ScopeKey: u.ScopeKey, Family: u.Family, ReservationBytes: a.res.Bytes(),
		HeapCapBytes: a.res.HeapCapBytes, ExportHeapCapBytes: a.res.ExportHeapCapBytes,
		AllocationBytes: a.res.AllocationBytes, AllocationObserved: a.res.AllocationObserved, PeakBytes: a.peak, PeakUnsampled: !a.sampled}
	row.Overran = a.sampled && a.peak > row.ReservationBytes
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if observed.at == nil {
		observed.at = map[string]int{}
	}
	if i, ok := observed.at[u.ScopeKey]; ok {
		observed.units[i] = row
		return
	}
	if len(observed.units) >= MaxObservedUnits {
		// Section 6 requires a finite bound on every retained collection, and
		// this is the response bound being reached. The unit is counted
		// rather than dropped.
		observed.omitted++
		if row.Overran {
			observed.omittedOverruns++
		}
		return
	}
	observed.at[u.ScopeKey] = len(observed.units)
	observed.units = append(observed.units, row)
}

// ObservedTotals is what the rows alone cannot say once the record is full:
// overrun is every unit this process recorded as over its reservation, the
// rows' and the omitted runs' together, and omitted is how many unit runs the
// rows had no room for. Units whose tree this platform cannot sample are in
// neither overrun count: an unsampled peak is not a peak below the
// reservation, so it is neither an overrun nor evidence against one.
func ObservedTotals() (overrun, omitted int64) {
	observed.mu.Lock()
	defer observed.mu.Unlock()
	overrun = observed.omittedOverruns
	for _, u := range observed.units {
		if u.Overran {
			overrun++
		}
	}
	return overrun, observed.omitted
}

// ObservedUnits is the resources block's view of the record, in the order the
// units ran. A unit whose tree this platform cannot sample reports no peak
// rather than a peak of zero, which would claim its analyzers used nothing.
func ObservedUnits() []model.AnalyzerUnit {
	observed.mu.Lock()
	defer observed.mu.Unlock()
	out := make([]model.AnalyzerUnit, 0, len(observed.units))
	for _, u := range observed.units {
		row := model.AnalyzerUnit{ScopeKey: model.TruncateDetail(u.ScopeKey), Family: string(u.Family),
			ReservationBytes: uint64(max(u.ReservationBytes, 0)), HeapCapBytes: uint64(max(u.HeapCapBytes, 0)),
			ExportHeapCapBytes: uint64(max(u.ExportHeapCapBytes, 0))}
		if u.AllocationObserved {
			alloc := uint64(max(u.AllocationBytes, 0))
			row.AllocationBytes = &alloc
		}
		if !u.PeakUnsampled && u.PeakBytes > 0 {
			peak := uint64(u.PeakBytes)
			row.ObservedPeakBytes = &peak
		}
		out = append(out, row)
	}
	return out
}
