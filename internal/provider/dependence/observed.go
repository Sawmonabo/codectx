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
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// MaxObservedUnits bounds the record. Section 6 requires a finite bound on
// every retained collection, and the block that reports it is a bounded
// response; a repository with more heavy units than this reports the first
// ones it ran, which are the ones whose sizing decided how the rest were
// scheduled.
const MaxObservedUnits = 64

// UnitMemory is one unit's memory accounting.
type UnitMemory struct {
	ScopeKey string
	Family   Family
	// ReservationBytes is what the unit was admitted against, HeapCapBytes
	// the cap its parse ran under and ExportHeapCapBytes its export's.
	ReservationBytes   int64
	HeapCapBytes       int64
	ExportHeapCapBytes int64
	// AllocationBytes is the machine-derived allocation the cap was bounded
	// by, or zero when the host does not expose available memory.
	AllocationBytes int64
	// PeakBytes is the highest peak either step's process tree reached, and
	// PeakUnsampled reports that this platform cannot observe it at all --
	// which is why it is a flag and not a zero.
	PeakBytes     int64
	PeakUnsampled bool
}

var observed struct {
	mu    sync.Mutex
	units []UnitMemory
	at    map[string]int
}

// Observe records what one unit was given and the peak its tree reached. It
// is called once per completed step, and the peak kept for a scope is the
// highest of them: parse and export never run at the same time, so the unit's
// footprint is the greater of the two, which is exactly what the reservation
// claimed.
func Observe(scopeKey string, f Family, r Reservation, peakBytes int64, unsampled bool) {
	observed.mu.Lock()
	defer observed.mu.Unlock()
	if observed.at == nil {
		observed.at = map[string]int{}
	}
	if i, ok := observed.at[scopeKey]; ok {
		u := &observed.units[i]
		u.HeapCapBytes, u.ExportHeapCapBytes = r.HeapCapBytes, r.ExportHeapCapBytes
		u.ReservationBytes, u.AllocationBytes = r.Bytes(), r.AllocationBytes
		u.PeakUnsampled = u.PeakUnsampled || unsampled
		if peakBytes > u.PeakBytes {
			u.PeakBytes = peakBytes
		}
		return
	}
	if len(observed.units) >= MaxObservedUnits {
		return
	}
	observed.at[scopeKey] = len(observed.units)
	observed.units = append(observed.units, UnitMemory{ScopeKey: scopeKey, Family: f,
		ReservationBytes: r.Bytes(), HeapCapBytes: r.HeapCapBytes, ExportHeapCapBytes: r.ExportHeapCapBytes,
		AllocationBytes: r.AllocationBytes, PeakBytes: peakBytes, PeakUnsampled: unsampled})
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
		if u.AllocationBytes > 0 {
			alloc := uint64(u.AllocationBytes)
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
