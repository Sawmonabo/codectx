// Package admission is this process's one reservation ledger: every heavy
// child -- analysis engine runs, external indexers, language servers -- is
// admitted against a single machine-derived allocation of MEMORY and one of
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
// in the queue.
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
	// reservation must sum within. It is always positive, so admission is
	// bounded by a sum of bytes on every platform and never by a count of
	// children.
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
}

// waiter is one blocked Reserve call. granted, stuck and the queue position are
// guarded by the ledger's mutex; ready is closed exactly once, by the grant.
type waiter struct {
	bytes     int64
	diskBytes int64
	granted   bool
	ready     chan struct{}
	// stuck carries one wake-up to a waiter that has reached the head of the
	// queue, does not fit, and brought a way to free room. It is nil for a
	// reserver that brought none, and the send is non-blocking, so a waiter
	// that is already awake is never held up by the grant path.
	stuck   chan struct{}
	release sync.Once
}

// NewLedger builds the ledger over the one memory allocation and the one disk
// allocation. A non-positive memory allocation is refused rather than treated
// as unlimited: an admission gate with no bound is not a gate, and every
// caller has a positive figure to hand over
// (dependence.Machine.SchedulingAllocation stands in for an unobservable host).
//
// The disk allocation may be zero and may not be negative. Zero is a real
// reading: a host whose free space is already at or below the floor it must
// keep has nothing to give a child, and every child that wants disk then runs
// alone rather than being refused. An unobservable free-space figure is NOT
// zero and must not be passed as one; the composition root stands a
// conservative figure in for it, exactly as it does for memory. Whether a
// figure was observed is not the ledger's concern: it gates against the
// figure either way, and the composition root tells the surfaces that
// disclose it.
func NewLedger(allocationBytes, diskAllocationBytes int64) (*Ledger, error) {
	if allocationBytes <= 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the reservation ledger needs a positive memory allocation"}
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
// not fit, so a reserver whose room frees up later is still asked.
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
	w := &waiter{bytes: r.MemoryBytes, diskBytes: r.DiskBytes, ready: make(chan struct{})}
	if makeRoom != nil {
		w.stuck = make(chan struct{}, 1)
	}
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
			makeRoom()
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
			if head.stuck != nil {
				select {
				case head.stuck <- struct{}{}:
				default:
				}
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
