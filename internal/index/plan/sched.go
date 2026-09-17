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
// The allocation, the running total and the queue are not this file's: they
// belong to the process's one admission ledger, which every other heavy child
// -- language servers above all -- is admitted against as well. What is here
// is the part that is the analysis engine's own: knowing that a unit's
// reservation is a dependence.Reservation and what it weighs in bytes.
//
// Admission is strict first-in-first-out across every reserver, and priority
// ordering (Section 11.6: a query promotes the units it needs to the head of
// the background queue) is the coordinator's queue above this gate -- Admit
// has no priority parameter and never reorders what reaches it.

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// Scheduler admits heavy analyzers against the process's admission ledger. It
// is safe for concurrent use and holds no resource of its own: what it hands
// back is permission to run, and the release function returns that permission
// exactly once.
type Scheduler struct {
	ledger *admission.Ledger
}

// NewScheduler binds the heavy-analyzer front to the process's one admission
// ledger. The ledger is the composition root's, never one built here: a
// scheduler that observed the machine itself would be a second running total
// bounded by the same allocation, which is how a process comes to reserve a
// multiple of the machine's memory.
func NewScheduler(l *admission.Ledger) *Scheduler { return &Scheduler{ledger: l} }

// Admit blocks until this reservation may run: the summed reservations of
// everything admitted -- by this scheduler or by any other reserver sharing
// the ledger -- plus this one within the machine-derived allocation. It
// returns a release function that is idempotent and must be called on every
// path.
//
// A unit larger than the whole allocation is admitted when the ledger holds
// nothing. Refusing it would refuse work the user never asked to have refused;
// serializing it is the whole point of the reservation.
//
// A canceled wait returns CTX_CANCELED and no release function.
func (s *Scheduler) Admit(ctx context.Context, r dependence.Reservation) (func(), error) {
	// No makeRoom step: a heavy unit holds nothing it could give back to let
	// itself in. It waits its turn, which the ledger keeps for it.
	return s.ledger.Reserve(ctx, r.Bytes(), nil)
}
