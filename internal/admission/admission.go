// Package admission is this process's one reservation ledger: every heavy
// child -- analysis engine runs, external indexers, language servers, parser
// workers -- is admitted against a single machine-derived allocation of MEMORY and one of
// DISK, by the SUM of what they reserve in each (ADR-0010 decisions 5 and 6).
//
// Two dimensions, one ledger, one total each, one queue. Disk is admitted here
// and not by a gate of its own for the reason memory is: a second gate is a
// second running total, and two gates over one queue would also let a child
// holding memory wait for disk behind a child holding disk waiting for memory.
// A waiter is admitted when every dimension it requests fits, and takes both
// at once, so there is no order in which two reservers can hold half of what
// the other needs. Two reservers each holding a running total bounded by the
// same allocation is not one gate: it lets a process reserve a multiple of the
// machine's memory and freeze the host, which is the failure this package
// exists to make impossible. Nothing here observes the machine or derives the
// allocations: the composition root derives both and hands the figures over,
// and re-derives the memory figure from the kernel's before every admission
// (RederiveBeforeEachAdmission, SetAllocation), so there is exactly one place
// a second total could ever be introduced.
//
// A holding's memory can move while it is held (Holding.Adjust): a parser
// worker holds the base it reports for its lifetime, and a reported figure
// above what it holds is taken at once, without waiting and whatever the sum,
// because the memory is already in use and waiting cannot give it back.
//
// Admission is strict first-in-first-out across every reserver. A waiter that
// does not fit blocks the waiters behind it rather than letting a small
// reservation overtake it: the alternative starves a large unit for as long as
// small ones keep arriving, and a unit that never runs is a capability that
// never answers. A language server therefore queues behind an engine unit that
// arrived first instead of opportunistically taking room from under it.
//
// The one thing a reserver may do before it waits is free room of its own: a
// reserver that brought a makeRoom step is asked to run it when it reaches the
// head and does not fit. That is how the language-server manager keeps its
// rule that a server is never refused because another project's server is
// running -- it stops an idle one -- without being able to overtake anything
// in the queue. A reserver with nothing idle to free brings no makeRoom step;
// what it holds comes back through release, which pumps.
//
// A holder that keeps room warm for reuse -- the parser pool's idle workers --
// is told when that room is wanted. It registers an idle-release step with
// Holder, and whenever the head of the queue does not fit, the head's own
// waiting goroutine runs its makeRoom step and then every registered step, with
// no ledger lock held. Such a holder also asks Waiting before it reuses room,
// and never reuses it while a reserver waits: the parser pool hands a worker
// back to its own acquirer at the head with the reservation it holds, and
// otherwise stops it, and its registered step stops its idle workers. Either
// way the room reaches the head in order and is returned by its holder, never
// taken from it: only idle room is given back, and the order among the queued
// reservers is kept.
package admission

import (
	"context"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Ledger admits heavy children against one allocation per dimension. It is safe for
// concurrent use and holds no resource of its own: what it hands back is
// permission to run, and the release function returns that permission exactly
// once.
type Ledger struct {
	mu sync.Mutex
	// allocation is the machine-derived memory allocation, as last set by
	// NewLedger or SetAllocation, that every admitted reservation must sum
	// within, so admission is bounded by a sum of bytes on every platform and
	// never by a count of children. It is never
	// negative; zero is a real reading -- a host with nothing left over this
	// process's footprint -- and serializes every child that wants memory.
	allocation int64
	// diskAllocation is the same for the temporary disk a child stages its
	// inputs and writes its outputs into: the free space observed under the
	// data directory less the host-safety floor the operator set, or the
	// conservative figure the composition root stands in where the platform
	// reports no free space. It is never negative and never "unlimited"; zero
	// is a real reading -- a host already at or below its floor -- and
	// serializes every child that wants disk at all.
	diskAllocation int64
	admitted       int
	// parses counts the admitted Parse reservations: the forward-progress
	// rule grants a Parse head whenever it is zero. alone counts the admitted
	// ones that run alone: while it is above zero no Parse head is granted.
	parses   int
	alone    int
	used     int64
	diskUsed int64
	queue    []*waiter
	// holders are the registered idle-release steps (Holder), keyed by the
	// registration so each one is removed exactly once. The map is bounded by
	// the holders this process composes, one per room-keeping pool.
	holders map[*func()]struct{}
	// rederive is the composition root's re-derivation of the memory
	// allocation (RederiveBeforeEachAdmission), or nil.
	rederive func()
}

// waiter is one reservation, queued and then held. bytes, granted, released
// and the queue position are guarded by the ledger's mutex; ready is closed
// exactly once, by the grant. bytes is what the holding holds now, so a
// release returns the adjusted figure.
type waiter struct {
	bytes     int64
	diskBytes int64
	parse     bool
	alone     bool
	granted   bool
	released  bool
	ready     chan struct{}
	// stuck carries one wake-up to a waiter that has reached the head of the
	// queue and does not fit: it then runs its own makeRoom step, if it
	// brought one, and every registered holder's step. The send is
	// non-blocking, so a waiter that is already awake is never held up by the
	// grant path.
	stuck chan struct{}
}

// NewLedger builds the ledger over the one memory allocation and the one disk
// allocation. Either may be zero and neither may be negative. Zero is a real
// reading: a host whose available memory is already at or below this
// process's footprint, or whose free space is at or below the floor it must
// keep, has nothing to give a child, and every child that wants that
// dimension then runs alone rather than being refused. An unobservable figure
// is NOT zero and must not be passed as one; the composition root stands a
// conservative figure in for it (dependence.Machine.SchedulingAllocation for
// memory), because an admission gate with no bound is not a gate. Whether a
// figure was observed is not the ledger's concern: it gates against the
// figure either way, and the composition root tells the surfaces that
// disclose it.
func NewLedger(allocationBytes, diskAllocationBytes int64) (*Ledger, error) {
	if allocationBytes < 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the reservation ledger needs a memory allocation that is not negative"}
	}
	if diskAllocationBytes < 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the reservation ledger needs a disk allocation that is not negative"}
	}
	return &Ledger{allocation: allocationBytes, diskAllocation: diskAllocationBytes}, nil
}

// Reserve blocks until these bytes of MEMORY may be held: the summed
// reservations of everything admitted plus this one within the allocation. It
// returns a release function that is idempotent and must be called on every
// path. A child that also stages bytes on disk states both through
// ReserveWith; this is that call with no disk.
//
// A reservation larger than the whole allocation is admitted when the ledger
// holds nothing. Refusing it would refuse work the user never asked to have
// refused; serializing it is the whole point of the reservation. This is the
// runs-alone rule, stated once here for every reserver.
//
// makeRoom may be nil. When it is not, the ledger calls it when this
// reservation is at the head of the queue and does not fit, before this call
// waits: it is the reserver's chance to free room it is holding itself.
// Whatever it frees goes back through the ordinary release path, which pumps
// the queue. It is called with no ledger lock held and may call back into the
// ledger; it may be called again each time the head is pumped and still does
// not fit, so a reserver whose room frees up later is still asked. Every
// registered holder's step (Holder) runs right after it, the same way, so idle
// room another holder keeps reaches this reservation in order.
//
// A canceled wait returns CTX_CANCELED and no release function. A wait that is
// granted at the same moment its context ends gives the permission straight
// back, so a canceled caller never leaks a reservation.
func (l *Ledger) Reserve(ctx context.Context, bytes int64, makeRoom func()) (func(), error) {
	return l.ReserveWith(ctx, Reservation{MemoryBytes: bytes}, makeRoom)
}

// Reservation is what one child holds while it runs, in both dimensions.
// DiskBytes is the temporary space it stages its inputs and writes its outputs
// into -- not the source it reads and not the store it publishes to, which are
// not this child's to hold -- and is zero for a child that writes none.
type Reservation struct {
	MemoryBytes int64
	DiskBytes   int64
	// Parse marks one parser file's per-file increment. At the head of the
	// queue it is granted whenever no other Parse reservation is held,
	// whatever the sum: the forward-progress rule, under which a file larger
	// than the whole allocation still runs, alone among the parses. The rule
	// waives the memory sum only, never the disk one.
	Parse bool
	// Alone marks a Parse reservation that runs alone among the parses: at
	// the head it is granted only once no other Parse reservation is held, and
	// while it is held no other Parse reservation is granted, whatever the
	// sum. It is the reservation of a file whose increment rests on a
	// prediction that has already fallen short of what a file needed, so no
	// other parse's room is staked on it. It requires Parse.
	Alone bool
}

// ReserveWith is Reserve in both dimensions: it blocks until this child's
// memory AND its temporary disk may be held, and takes both in the same grant.
// Everything Reserve documents holds here, per dimension: the runs-alone rule,
// the makeRoom step, the idempotent release and the cancellation that leaks
// nothing. It is Hold for a reserver that never adjusts what it holds.
func (l *Ledger) ReserveWith(ctx context.Context, r Reservation, makeRoom func()) (func(), error) {
	h, err := l.Hold(ctx, r, makeRoom)
	if err != nil {
		return nil, err
	}
	return h.Release, nil
}

// Snapshot is the memory allocation as last derived and what is reserved
// against it right now, adjustments included, read together under one lock so
// the two figures an operator reads are a consistent pair rather than two
// moments. reserved can exceed allocation: a holding adjusted upward, a
// reservation admitted alone and a lowered allocation all leave it above.
func (l *Ledger) Snapshot() (allocation, reserved int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allocation, l.used
}

// DiskSnapshot is Snapshot for the second dimension: the temporary space the
// ledger admits against -- free space under the data directory less the
// host-safety floor, or the stand-in composition warned about where the
// platform published no figure -- and the sum the children holding room have
// reserved of it. A waiter blocked on disk is invisible in Snapshot alone.
func (l *Ledger) DiskSnapshot() (allocation, reserved int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.diskAllocation, l.diskUsed
}

// RederiveBeforeEachAdmission installs the step that re-derives the memory
// allocation from the kernel's figure. Hold runs it, with no ledger lock held,
// before every reservation queues, so each child -- a unit, a language server,
// a parser worker or file -- is admitted against a reading of the machine
// taken as it asks, and not against the one the last parser file took. The
// step sets the figure through SetAllocation and may leave it as it is when
// the kernel withholds a reading.
func (l *Ledger) RederiveBeforeEachAdmission(step func()) {
	l.mu.Lock()
	l.rederive = step
	l.mu.Unlock()
}

// Holder registers an idle-release step: a holder of room it keeps warm for
// reuse, and would give back when someone needs it, is asked to run it every
// time the head of the queue does not fit, after the head's own makeRoom step.
// It runs on the head's waiting goroutine with no ledger lock held, so it may
// call back into the ledger -- Waiting, and the release of whatever it frees,
// which pumps. It may be called again on every such pump, so it frees only
// room it holds idle and does nothing when it holds none. The returned
// function removes the registration, once, however often it is called.
func (l *Ledger) Holder(release func()) (unregister func()) {
	key := &release
	l.mu.Lock()
	if l.holders == nil {
		l.holders = map[*func()]struct{}{}
	}
	l.holders[key] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.holders, key)
			l.mu.Unlock()
		})
	}
}

// askHolders runs every registered idle-release step, read under the lock and
// run without it.
func (l *Ledger) askHolders() {
	l.mu.Lock()
	steps := make([]func(), 0, len(l.holders))
	for step := range l.holders {
		steps = append(steps, *step)
	}
	l.mu.Unlock()
	for _, step := range steps {
		step()
	}
}

// Waiting reports whether any reserver is queued: one that has not been
// granted and, since pump grants the head the moment it fits, one whose head
// does not fit right now. It is a moment's reading for a holder of room that
// could either reuse that room itself or give it back. A holder that reuses
// room while this is true takes it around a waiter that arrived first, which
// is the overtaking strict first-in-first-out forbids; giving it back instead
// pumps, and the head is admitted in order.
func (l *Ledger) Waiting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue) > 0
}

// pump grants the head of the queue for as long as the head is admissible. The
// mutex must be held. It stops at the first waiter that is not rather than
// looking past it, which is what makes admission first-in-first-out across
// every reserver.
func (l *Ledger) pump() {
	for len(l.queue) > 0 {
		head := l.queue[0]
		// The sums are checked only against something already admitted: an
		// idle ledger admits any single reservation, whatever it is, so a
		// child larger than the whole allocation -- of either dimension --
		// runs alone rather than never. A dimension is checked only when the
		// head requests some of it: a child that stages nothing is never held
		// behind a disk dimension another child filled, and a child that
		// holds no memory never behind the memory one. Every dimension the
		// head requests must fit, and it takes both in one grant, so the two
		// dimensions cannot be held against each other.
		//
		// A Parse head's memory is also granted whatever the sum when no other
		// Parse reservation is held: the forward-progress rule. Parser workers
		// hold their bases for their lifetime, so "nothing admitted" would
		// never hold while a pool is up, and a file larger than the allocation
		// would never run. The rule waives memory only: a Parse head that
		// requested disk still waits for the disk to fit.
		//
		// A Parse head is never granted beside a Parse reservation that runs
		// alone, and one that runs alone is never granted beside another
		// Parse reservation, whatever the sum.
		memoryFits := head.bytes == 0 || l.used+head.bytes <= l.allocation || head.parse && l.parses == 0
		if head.parse && (l.alone > 0 || head.alone && l.parses > 0) {
			memoryFits = false
		}
		diskFits := head.diskBytes == 0 || l.diskUsed+head.diskBytes <= l.diskAllocation
		if l.admitted > 0 && !(memoryFits && diskFits) {
			select {
			case head.stuck <- struct{}{}:
			default:
			}
			return
		}
		// Cleared before the reslice: a popped waiter left in the backing array
		// stays reachable until append next reallocates, and the product bounds
		// every queue explicitly rather than by luck.
		l.queue[0] = nil
		l.queue = l.queue[1:]
		l.admitted++
		if head.parse {
			l.parses++
		}
		if head.alone {
			l.alone++
		}
		l.used += head.bytes
		l.diskUsed += head.diskBytes
		head.granted = true
		close(head.ready)
	}
}

// remove drops a canceled waiter from the queue and lets the queue move.
// The mutex must be held.
func (l *Ledger) remove(w *waiter) {
	for i, q := range l.queue {
		if q == w {
			// The shift leaves the last slot pointing at the waiter that moved
			// down; clearing it is what keeps the canceled waiter unreachable.
			copy(l.queue[i:], l.queue[i+1:])
			l.queue[len(l.queue)-1] = nil
			l.queue = l.queue[:len(l.queue)-1]
			// pump stops at the first waiter that does not fit, so the head
			// this one was blocking behind is granted only when something
			// pumps. Without this the new head waits for an unrelated release
			// -- a full engine parse away -- although it fits right now.
			l.pump()
			return
		}
	}
}

// release returns one reservation exactly once, at the memory it holds now,
// and lets the queue move.
func (l *Ledger) release(w *waiter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if w.released {
		return
	}
	w.released = true
	l.admitted--
	if w.parse {
		l.parses--
	}
	if w.alone {
		l.alone--
	}
	l.used -= w.bytes
	l.diskUsed -= w.diskBytes
	l.pump()
}

// Holding is one admitted reservation whose memory can be adjusted while it is
// held. Release returns it exactly once.
type Holding struct {
	l *Ledger
	w *waiter
}

// Hold blocks until r may be held and returns the admitted holding. It is the
// one waiting path: Reserve and ReserveWith are Hold for a reserver that never
// adjusts, and everything they document -- the runs-alone rule, the makeRoom
// step, the idle-release steps, the cancellation that leaks nothing -- is this
// call's. A Parse reservation is also admitted under the forward-progress rule
// (Reservation.Parse), and one that runs alone is held apart from every other
// (Reservation.Alone). A reservation of negative bytes in either dimension is
// refused with CTX_ARGUMENT_INVALID: a holding's figures are never negative.
// So is one that runs alone without being a Parse reservation, since only
// parses are kept apart from each other.
func (l *Ledger) Hold(ctx context.Context, r Reservation, makeRoom func()) (*Holding, error) {
	if r.MemoryBytes < 0 || r.DiskBytes < 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "a reservation on the ledger needs memory and disk figures that are not negative"}
	}
	if r.Alone && !r.Parse {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "only a parser file's reservation can run alone among the parses"}
	}
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	w := &waiter{bytes: r.MemoryBytes, diskBytes: r.DiskBytes, parse: r.Parse, alone: r.Alone, ready: make(chan struct{}),
		stuck: make(chan struct{}, 1)}
	h := &Holding{l: l, w: w}
	l.mu.Lock()
	rederive := l.rederive
	l.mu.Unlock()
	if rederive != nil {
		rederive()
	}
	l.mu.Lock()
	l.queue = append(l.queue, w)
	l.pump()
	l.mu.Unlock()

	for {
		select {
		case <-w.ready:
			return h, nil
		case <-w.stuck:
			// A wake sent while this waiter was stuck can still be buffered
			// when a later pump grants it; the grant wins, and room that is no
			// longer needed -- a warm idle server -- is not freed for nothing.
			l.mu.Lock()
			granted := w.granted
			l.mu.Unlock()
			if granted {
				return h, nil
			}
			if makeRoom != nil {
				makeRoom()
			}
			l.askHolders()
		case <-ctx.Done():
			l.mu.Lock()
			granted := w.granted
			if !granted {
				l.remove(w)
			}
			l.mu.Unlock()
			if granted {
				l.release(w)
			}
			return nil, model.Canceled(ctx.Err())
		}
	}
}

// Adjust sets the holding's memory to memoryBytes. Upward it takes the
// difference at once, without waiting and whatever the sum: the memory is
// already in use, and waiting would give none of it back. Downward it returns
// the difference, which pumps the queue. A negative figure is refused with no
// change, since a holding's memory is never negative, and a released holding
// holds nothing to adjust, so Adjust after Release does nothing.
func (h *Holding) Adjust(memoryBytes int64) {
	if memoryBytes < 0 {
		return
	}
	l := h.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if h.w.released {
		return
	}
	delta := memoryBytes - h.w.bytes
	h.w.bytes = memoryBytes
	l.used += delta
	if delta < 0 {
		l.pump()
	}
}

// Release returns the holding, once, however often it is called.
func (h *Holding) Release() { h.l.release(h.w) }

// SetAllocation replaces the memory allocation with a re-derived figure and
// pumps the queue: a raised figure admits the waiters that now fit, and a
// lowered one holds the next reservation back. Nothing already admitted is
// taken back, so what is reserved may stand above a lowered allocation until
// its holders release it. A negative figure is refused with no change, for the
// reason NewLedger refuses one: the allocation is never negative, and zero is
// the real reading of a host with nothing left over.
func (l *Ledger) SetAllocation(memoryBytes int64) {
	if memoryBytes < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allocation = memoryBytes
	l.pump()
}
