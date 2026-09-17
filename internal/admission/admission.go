// Package admission is this process's one memory admission ledger: every heavy
// child -- analysis engine runs, external indexers, language servers -- is
// admitted against a single machine-derived allocation by the SUM of what they
// reserve (ADR-0010 decision 5).
//
// One ledger, one total, one queue. Two reservers each holding a running total
// bounded by the same allocation is not one gate: it lets a process reserve a
// multiple of the machine's memory and freeze the host, which is the failure
// this package exists to make impossible. Nothing here observes the machine or
// derives the allocation -- the composition root does that once and hands the
// figure over -- so there is exactly one place a second total could ever be
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

// Ledger admits heavy children against one allocation. It is safe for
// concurrent use and holds no resource of its own: what it hands back is
// permission to run, and the release function returns that permission exactly
// once.
type Ledger struct {
	mu sync.Mutex
	// allocation is the machine-derived allocation every admitted reservation
	// must sum within. It is always positive, so admission is bounded by a sum
	// of bytes on every platform and never by a count of children.
	allocation int64
	admitted   int
	used       int64
	queue      []*waiter
}

// waiter is one blocked Reserve call. granted, stuck and the queue position are
// guarded by the ledger's mutex; ready is closed exactly once, by the grant.
type waiter struct {
	bytes   int64
	granted bool
	ready   chan struct{}
	// stuck carries one wake-up to a waiter that has reached the head of the
	// queue, does not fit, and brought a way to free room. It is nil for a
	// reserver that brought none, and the send is non-blocking, so a waiter
	// that is already awake is never held up by the grant path.
	stuck   chan struct{}
	release sync.Once
}

// NewLedger builds the ledger over the one machine-derived allocation. A
// non-positive allocation is refused rather than treated as unlimited: an
// admission gate with no bound is not a gate, and every caller has a positive
// figure to hand over (dependence.Machine.SchedulingAllocation stands in for an
// unobservable host).
func NewLedger(allocationBytes int64) (*Ledger, error) {
	if allocationBytes <= 0 {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the memory admission ledger needs a positive allocation"}
	}
	return &Ledger{allocation: allocationBytes}, nil
}

// Reserve blocks until these bytes may be held: the summed reservations of
// everything admitted plus this one within the allocation. It returns a
// release function that is idempotent and must be called on every path.
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
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	w := &waiter{bytes: bytes, ready: make(chan struct{})}
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

// Snapshot is the allocation and what is reserved against it right now, read
// together under one lock so the two figures an operator reads are a
// consistent pair rather than two moments.
func (l *Ledger) Snapshot() (allocation, reserved int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.allocation, l.used
}

// pump grants the head of the queue for as long as the head fits. The mutex
// must be held. It stops at the first waiter that does not fit rather than
// looking past it, which is what makes admission first-in-first-out across
// every reserver.
func (l *Ledger) pump() {
	for len(l.queue) > 0 {
		head := l.queue[0]
		// The sum is checked only against something already admitted: an idle
		// ledger admits any single reservation, whatever it is, so a child
		// larger than the whole allocation runs alone rather than never.
		if l.admitted > 0 && l.used+head.bytes > l.allocation {
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
		l.pump()
	})
}
