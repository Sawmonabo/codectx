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
// allocations -- the composition root does that once and hands the figures
// over -- so there is exactly one place a second total could ever be
// introduced.
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
	// allocation is the machine-derived memory allocation every admitted
	// reservation must sum within, so admission is bounded by a sum of bytes
	// on every platform and never by a count of children. It is never
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
	used           int64
	diskUsed       int64
	queue          []*waiter
	// holders are the registered idle-release steps (Holder), keyed by the
	// registration so each one is removed exactly once. The map is bounded by
	// the holders this process composes, one per room-keeping pool.
	holders map[*func()]struct{}
}

// waiter is one blocked Reserve call. granted and the queue position are
// guarded by the ledger's mutex; ready is closed exactly once, by the grant.
type waiter struct {
	bytes     int64
	diskBytes int64
	granted   bool
	ready     chan struct{}
	// stuck carries one wake-up to a waiter that has reached the head of the
	// queue and does not fit: it then runs its own makeRoom step, if it
	// brought one, and every registered holder's step. The send is
	// non-blocking, so a waiter that is already awake is never held up by the
	// grant path.
	stuck   chan struct{}
	release sync.Once
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
	// than the whole allocation still runs, alone among the parses.
	Parse bool
}

// ReserveWith is Reserve in both dimensions: it blocks until this child's
// memory AND its temporary disk may be held, and takes both in the same grant.
// Everything Reserve documents holds here, per dimension: the runs-alone rule,
// the makeRoom step, the idempotent release and the cancellation that leaks
// nothing.
func (l *Ledger) ReserveWith(ctx context.Context, r Reservation, makeRoom func()) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	w := &waiter{bytes: r.MemoryBytes, diskBytes: r.DiskBytes, ready: make(chan struct{}),
		stuck: make(chan struct{}, 1)}
	l.mu.Lock()
	l.queue = append(l.queue, w)
	l.pump()
	l.mu.Unlock()

	for {
		select {
		case <-w.ready:
			return func() { l.release(w) }, nil
		case <-w.stuck:
			// A wake sent while this waiter was stuck can still be buffered
			// when a later pump grants it; the grant wins, and room that is no
			// longer needed -- a warm idle server -- is not freed for nothing.
			l.mu.Lock()
			granted := w.granted
			l.mu.Unlock()
			if granted {
				return func() { l.release(w) }, nil
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

// Snapshot is the memory allocation and what is reserved against it right now,
// read together under one lock so the two figures an operator reads are a
// consistent pair rather than two moments.
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

// pump grants the head of the queue for as long as the head fits. The mutex
// must be held. It stops at the first waiter that does not fit rather than
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
		if l.admitted > 0 && (head.bytes > 0 && l.used+head.bytes > l.allocation ||
			head.diskBytes > 0 && l.diskUsed+head.diskBytes > l.diskAllocation) {
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

// release returns one reservation exactly once and lets the queue move.
func (l *Ledger) release(w *waiter) {
	w.release.Do(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.admitted--
		l.used -= w.bytes
		l.diskUsed -= w.diskBytes
		l.pump()
	})
}

// Holding is one admitted reservation whose memory can be adjusted while it is
// held. Release returns it exactly once.
type Holding struct {
	l *Ledger
	w *waiter
}

// Hold is ReserveWith returning an adjustable holding.
func (l *Ledger) Hold(ctx context.Context, r Reservation, makeRoom func()) (*Holding, error) {
	panic("admission: Hold is not built yet")
}

// Adjust sets the holding's memory to memoryBytes: upward at once, without
// waiting and whatever the sum, downward by returning the difference, which
// pumps the queue.
func (h *Holding) Adjust(memoryBytes int64) {
	panic("admission: Adjust is not built yet")
}

// Release returns the holding, once, however often it is called.
func (h *Holding) Release() {
	panic("admission: Release is not built yet")
}

// SetAllocation replaces the memory allocation with a re-derived figure and
// pumps the queue. Nothing already admitted is taken back.
func (l *Ledger) SetAllocation(memoryBytes int64) {
	panic("admission: SetAllocation is not built yet")
}
