package plan

// Heavy-analyzer admission (Sections 11.6, 13.1).
//
// How much heavy work runs at once is decided by one observation of the
// machine and nothing else: a reservation is admitted when the summed
// reservations of everything already running plus this one fit the
// machine-derived allocation. There is no count of analyzers, no setting and
// no ceiling that can refuse a unit -- a waiter always runs eventually, and a
// scheduler that refused on the allocation would be a default memory ceiling
// by another name, which Section 11.6 forbids.
//
// Admission is strict first-in-first-out. A waiter that does not fit blocks
// every waiter behind it rather than letting a small unit overtake it: the
// alternative starves a large unit for as long as small ones keep arriving,
// and a dependence unit that never runs is a capability that never answers.
// Priority ordering (Section 11.6: a query promotes the units it needs to the
// head of the background queue) is the coordinator's queue above this gate --
// Admit has no priority parameter and never reorders what reaches it.

import (
	"context"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// Scheduler admits heavy analyzers. It is safe for concurrent use and holds no
// resource of its own: what it hands back is permission to run, and the
// release function returns that permission exactly once.
type Scheduler struct {
	mu sync.Mutex
	// allocation is the machine-derived allocation every admitted reservation
	// must sum within. It is always positive: Machine.SchedulingAllocation
	// stands in for an unobservable host, so admission is bounded by a sum of
	// bytes on every platform and never by a count of children.
	allocation int64
	admitted   int
	used       int64
	queue      []*waiter
}

// waiter is one blocked Admit call. granted and the queue position are guarded
// by the scheduler's mutex; ready is closed exactly once, by the grant.
type waiter struct {
	bytes   int64
	granted bool
	ready   chan struct{}
	release sync.Once
}

// NewScheduler builds the admission gate over the machine's one allocation.
func NewScheduler(m dependence.Machine) *Scheduler {
	return &Scheduler{allocation: m.SchedulingAllocation()}
}

// Admit blocks until this reservation may run: the summed reservations of
// everything admitted plus this one within the machine-derived allocation. It
// returns a release function that is idempotent and must be called on every
// path.
//
// A unit larger than the whole allocation is admitted when nothing else is
// running. Refusing it would refuse work the user never asked to have
// refused; serializing it is the whole point of the reservation.
//
// A canceled wait returns CTX_CANCELED and no release function. A wait that is
// granted at the same moment its context ends gives the permission straight
// back, so a canceled caller never leaks a slot.
func (s *Scheduler) Admit(ctx context.Context, r dependence.Reservation) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	w := &waiter{bytes: r.Bytes(), ready: make(chan struct{})}
	s.mu.Lock()
	s.queue = append(s.queue, w)
	s.pump()
	s.mu.Unlock()

	select {
	case <-w.ready:
		return func() { s.release(w) }, nil
	case <-ctx.Done():
		s.mu.Lock()
		granted := w.granted
		if !granted {
			s.remove(w)
		}
		s.mu.Unlock()
		if granted {
			s.release(w)
		}
		return nil, model.Canceled(ctx.Err())
	}
}

// pump grants the head of the queue for as long as the head fits. The mutex
// must be held. It stops at the first waiter that does not fit rather than
// looking past it, which is what makes admission first-in-first-out.
func (s *Scheduler) pump() {
	for len(s.queue) > 0 {
		head := s.queue[0]
		// The sum is checked only against something already admitted: an idle
		// scheduler admits any single unit, whatever it reserves, so a unit
		// larger than the whole allocation runs alone rather than never.
		if s.admitted > 0 && s.used+head.bytes > s.allocation {
			return
		}
		// Cleared before the reslice: a popped waiter left in the backing array
		// stays reachable until append next reallocates, and the product bounds
		// every queue explicitly rather than by luck.
		s.queue[0] = nil
		s.queue = s.queue[1:]
		s.admitted++
		s.used += head.bytes
		head.granted = true
		close(head.ready)
	}
}

// remove drops a canceled waiter from the queue and lets the queue move.
// The mutex must be held.
func (s *Scheduler) remove(w *waiter) {
	for i, q := range s.queue {
		if q == w {
			// The shift leaves the last slot pointing at the waiter that moved
			// down; clearing it is what keeps the canceled waiter unreachable.
			copy(s.queue[i:], s.queue[i+1:])
			s.queue[len(s.queue)-1] = nil
			s.queue = s.queue[:len(s.queue)-1]
			// pump stops at the first waiter that does not fit, so the head
			// this one was blocking behind is granted only when something
			// pumps. Without this the new head waits for an unrelated release
			// -- a full engine parse away -- although it fits right now.
			s.pump()
			return
		}
	}
}

// release returns one admission exactly once and lets the queue move.
func (s *Scheduler) release(w *waiter) {
	w.release.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.admitted--
		s.used -= w.bytes
		s.pump()
	})
}
