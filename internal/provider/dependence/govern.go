package dependence

// Memory governance for one unit (Section 11.6, ruling of
// docs/research/00-synthesis.md Section 8).
//
// There is no default memory ceiling. A reservation is a scheduling input,
// not a refusal: the coordinator uses it to order units and to decide whether
// a second heavy analyzer fits, and only an explicit non-zero user
// `unit_memory_ceiling_bytes` ever rejects a unit before it runs. Splitting a
// unit for memory is never done — it costs more than half of the resolved
// calls on a project that is split (research Section 8) — and no analysis
// limit is ever lowered to make a unit fit.
//
// The cap is placed on the frontend heap, which research Section 4 measured
// as lossless everywhere it succeeds: on five large repositories a capped run
// produced the same facts as the default run, or no graph at all. A heap cap
// is not a memory cap, so the reservation adds the resident memory the
// frontend keeps outside the heap, measured per family in research Section 10.

import (
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	kiB = 1 << 10
	miB = 1 << 20
	giB = 1 << 30
)

// DefaultUnitMemoryFloorBytes is the smallest heap cap a unit is given. A cap
// below it makes even a trivial project thrash; it mirrors
// `providers.dependence.unit_memory_floor_bytes`.
const DefaultUnitMemoryFloorBytes int64 = 768 * miB

// DefaultBaseFootprintBytes is what the machine-derived allocation subtracts
// for this process and its base index before handing the rest to an analyzer,
// and DefaultSafetyMarginBytes is the headroom left for transient allocation
// and RSS variation (Section 23.3: degrade concurrency before coverage, and
// leave headroom rather than allocate to the last byte).
const (
	DefaultBaseFootprintBytes int64 = 1 * giB
	DefaultSafetyMarginBytes  int64 = 1 * giB
)

// heapPerSourceByte is how much frontend heap one byte of unit source is
// estimated to need, per family. Each value is taken from the most demanding
// measured point of docs/research/10-round3-empirical.md Sections 4, 4b, 5 and
// 7 with headroom, because research Section 4 also measured that a cap close
// to the live set costs time rather than memory (the Java repository ran 3.6x
// slower at a cap it could just fit in). Over-reserving only serializes work;
// under-reserving fails a unit.
var heapPerSourceByte = map[Family]int64{
	FamilyC:          128, // 1.8M-line C repository: ~54 MB of source needed a 4 GiB cap to pass, 8 GiB to run at full speed
	FamilyGo:         384, // 438k-line Go module passed at a 4 GiB cap
	FamilyJava:       256, // 1.5M-line Java repository wanted ~8 GiB to avoid collector thrashing
	FamilyJavaScript: 512, // 81k-line TypeScript project passed at a 1 GiB cap
	FamilyPython:     640, // 1.05M-line Python tree needed ~18 GiB uncapped and failed closed at 4 GiB
	FamilyRust:       640, // 56k-line Cargo workspace peaked at 1.77 GB of tree RSS, most of it outside the heap
}

// residentAboveHeap is the resident memory the frontend keeps outside the
// heap, measured per family in research Section 10. It is why a heap cap
// cannot be used as the reservation.
var residentAboveHeap = map[Family]int64{
	FamilyC:          2662 * miB, // native parser memory
	FamilyGo:         512 * miB,
	FamilyJava:       448 * miB,
	FamilyJavaScript: 320 * miB,
	FamilyPython:     1945 * miB,
	FamilyRust:       256 * miB,
}

// helperAllowance is the fixed cost of a helper process that lives outside
// the engine's managed heap altogether. Only the Rust frontend has one large
// enough to matter
// (research Section 10: about 0.8 GB, constant).
var helperAllowance = map[Family]int64{FamilyRust: 832 * miB}

// exportHeapDivisor sizes the export cap as a fraction of the parse cap, and
// exportResident is the export step's own resident allowance. Export scales
// with the graph rather than the source and is consistently cheaper than
// parse (research Section 4), but it is not free: it needs its own
// reservation, which is why it is reported separately.
const (
	exportHeapDivisor = 3
	exportResident    = 819 * miB
	exportHeapFloor   = 512 * miB
)

// Reservation is what one unit must be admitted against. Parse and export
// never run at the same time, so Bytes is the peak of the two rather than
// their sum; the coordinator serializes heavy analyzers on it
// (`max_concurrent_heavy_analyzers`, 1 by default) and co-schedules a second
// only when the summed reservations fit the allocation.
type Reservation struct {
	Family Family
	// HeapCapBytes is the cap placed on the frontend heap for the parse step,
	// and ExportHeapCapBytes the export step's own, smaller cap.
	HeapCapBytes       int64
	ExportHeapCapBytes int64
	// ResidentBytes and HelperBytes are the memory the family keeps outside
	// the heap and the fixed cost of a helper outside it.
	ResidentBytes int64
	HelperBytes   int64
	// AllocationBytes is the machine-derived allocation the cap was bounded
	// by, or zero when the machine's available memory could not be observed.
	// Section 22 requires an unavailable metric to be reported as unavailable
	// rather than as zero, which is what a zero here means: unknown, not none.
	AllocationBytes int64
	// EstimatedBytes is the unbounded estimate before the allocation bound
	// was applied. A unit whose estimate exceeds its cap is the one that
	// earns an out-of-memory retry.
	EstimatedBytes int64
	// CeilingBytes is the explicit user limit, or zero for machine-derived.
	CeilingBytes int64
}

// ParseBytes is the reservation of the parse step and ExportBytes that of the
// export step.
func (r Reservation) ParseBytes() int64  { return r.HeapCapBytes + r.ResidentBytes + r.HelperBytes }
func (r Reservation) ExportBytes() int64 { return r.ExportHeapCapBytes + exportResident }

// Bytes is the reservation the coordinator schedules against.
func (r Reservation) Bytes() int64 { return max(r.ParseBytes(), r.ExportBytes()) }

// Machine is the observed memory of the host the allocation is derived from.
// Available is zero and Observed false when the platform does not expose it.
type Machine struct {
	AvailableBytes int64
	Observed       bool
}

// Allocation is the machine-derived allocation: available memory minus this
// process's base footprint minus the safety margin. It is zero when the host
// does not expose available memory, which means unknown: with no observation
// the estimate is used as it stands, because inventing a bound would be a
// default memory ceiling by another name and the ruling forbids one.
func (m Machine) Allocation(baseFootprint, safetyMargin int64) int64 {
	if !m.Observed {
		return 0
	}
	alloc := m.AvailableBytes - baseFootprint - safetyMargin
	if alloc < 0 {
		return 0
	}
	return alloc
}

// Governor sizes reservations. It holds no state and reads nothing: the
// machine observation is a parameter, so the sizing is a pure function a test
// can drive across hosts it does not have.
type Governor struct {
	FloorBytes    int64
	CeilingBytes  int64
	BaseFootprint int64
	SafetyMargin  int64
}

// NewGovernor applies the defaults for any bound the caller left at zero.
// A zero ceiling is not a default: it is the documented value that means
// machine-derived.
func NewGovernor(floorBytes, ceilingBytes int64) Governor {
	if floorBytes <= 0 {
		floorBytes = DefaultUnitMemoryFloorBytes
	}
	return Governor{FloorBytes: floorBytes, CeilingBytes: ceilingBytes,
		BaseFootprint: DefaultBaseFootprintBytes, SafetyMargin: DefaultSafetyMarginBytes}
}

// Reserve sizes the reservation of a unit of sourceBytes bytes on machine m.
// The heap cap is the family's estimate from the unit's byte count, never
// below the floor, and bounded above by the machine-derived allocation when
// one could be observed. An explicit non-zero ceiling bounds it too, and is
// the only value that can reject the unit outright (Reject).
func (g Governor) Reserve(f Family, sourceBytes int64, m Machine) Reservation {
	r := Reservation{Family: f, ResidentBytes: residentAboveHeap[f], HelperBytes: helperAllowance[f],
		CeilingBytes: g.CeilingBytes, AllocationBytes: m.Allocation(g.BaseFootprint, g.SafetyMargin)}
	estimate := sourceBytes * heapPerSourceByte[f]
	if estimate < g.FloorBytes {
		estimate = g.FloorBytes
	}
	r.EstimatedBytes = estimate
	cap := estimate
	if r.AllocationBytes > 0 && cap > r.AllocationBytes {
		cap = r.AllocationBytes
	}
	if g.CeilingBytes > 0 && cap > g.CeilingBytes {
		cap = g.CeilingBytes
	}
	if cap < g.FloorBytes {
		cap = g.FloorBytes
	}
	r.HeapCapBytes = cap
	if r.ExportHeapCapBytes = cap / exportHeapDivisor; r.ExportHeapCapBytes < exportHeapFloor {
		r.ExportHeapCapBytes = exportHeapFloor
	}
	if r.ExportHeapCapBytes > cap {
		r.ExportHeapCapBytes = cap
	}
	return r
}

// Reject reports the typed refusal for a unit whose reservation does not fit
// an explicit user ceiling, or nil when the unit may run. A machine-derived
// allocation never rejects: it bounds the cap and the unit runs, because the
// alternative is refusing work the user never asked to have refused.
func (g Governor) Reject(r Reservation, scopeKey string) error {
	if g.CeilingBytes <= 0 || r.ParseBytes() <= g.CeilingBytes {
		return nil
	}
	return resourceLimit("the dependence unit reserves more memory than providers.dependence.unit_memory_ceiling_bytes allows").
		WithDetail("scope_key", truncate(scopeKey, model.MaxIdentifierBytes)).
		WithDetail("reservation_bytes", itoa(r.ParseBytes())).
		WithDetail("ceiling_bytes", itoa(g.CeilingBytes)).
		WithDetail("limit", "unit_memory_ceiling_bytes").
		WithRemediation("raise or clear providers.dependence.unit_memory_ceiling_bytes; 0 means the allocation is derived from the machine")
}

// RetryCap is the cap an out-of-memory unit is retried at, or zero when no
// retry is honest. Section 11.6 allows exactly one retry, at the
// machine-derived allocation, and only when that allocation exceeds the cap
// that failed: a retry at the same or a smaller cap cost 100 seconds on a
// 1.05M-line Python tree and could not have succeeded.
func (g Governor) RetryCap(r Reservation) int64 {
	if r.AllocationBytes <= r.HeapCapBytes {
		return 0
	}
	cap := r.AllocationBytes
	if g.CeilingBytes > 0 && cap > g.CeilingBytes {
		cap = g.CeilingBytes
	}
	if cap <= r.HeapCapBytes {
		return 0
	}
	return cap
}

// itoa renders a byte figure for a bounded error detail.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
