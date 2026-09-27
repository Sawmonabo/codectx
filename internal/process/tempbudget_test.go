package process

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// resources.max_temp_bytes is a BOUND, and its default is unlimited. A runner
// built from the default must construct and must admit a run of any disk
// reservation: a runner that refused a non-positive disk budget would fail
// application construction rather than any one run. Only a value the operator
// set may refuse a run, and it must say which key to raise.
//
// Mutation proof: change NewRunner's `limits.DiskBudgetBytes < 0` to `<= 0`
// and this fails with
//
//	the default temporary budget refused the runner: ... none may be negative, and 0 is no bound
func TestDefaultTemporaryBudgetIsUnlimited(t *testing.T) {
	cfg := config.Defaults()
	if cfg.Resources.MaxTempBytes != 0 {
		t.Fatalf("resources.max_temp_bytes defaults to %d, not unlimited: a bound "+
			"nobody set must not refuse or truncate work", cfg.Resources.MaxTempBytes)
	}
	// The shape a runner takes from the default ceiling.
	r, err := NewRunner(Limits{MaxConcurrent: 2, MemoryBudgetBytes: 1 << 30,
		DiskBudgetBytes: cfg.Resources.MaxTempBytes})
	if err != nil {
		t.Fatalf("the default temporary budget refused the runner: %v", err)
	}
	// A 4 TiB reservation: unlimited means admitted, not merely "large".
	release, err := r.reserve(context.Background(), Spec{DiskReservationBytes: 1 << 42})
	if err != nil {
		t.Fatalf("an unlimited disk budget refused a %d-byte reservation: %v", int64(1)<<42, err)
	}
	release()

	// A value the operator DID set still refuses, and names the key.
	set, err := NewRunner(Limits{MaxConcurrent: 2, MemoryBudgetBytes: 1 << 30, DiskBudgetBytes: 1 << 20})
	if err != nil {
		t.Fatalf("a user-set temporary budget refused the runner: %v", err)
	}
	if _, err = set.reserve(context.Background(), Spec{DiskReservationBytes: 1 << 30}); err == nil {
		t.Fatalf("a user-set temporary budget admitted a reservation over it: only an " +
			"explicit limit may reject work, and it must actually reject it")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
		t.Fatalf("the refusal is %v, not a typed CTX_RESOURCE_LIMIT", err)
	}
	if typed.Details["limit"] != "resources.max_temp_bytes" {
		t.Fatalf("the refusal names %q as the limit, so the operator is not told which key to raise",
			typed.Details["limit"])
	}
}

// A runner beneath the reservation ledger states no memory figure and no count
// (Limits{}), and must then admit every run at once: the ledger above already
// admitted each child, so a runner that read an unset bound as a bound of zero
// would hold every child of the process in its queue forever, and one that read
// it as a figure would refuse the child larger than the machine that the
// ledger runs alone. Mutations that fail it: dropping the `> 0` guard on
// MaxConcurrent in promote (the first reservation waits until the deadline
// below) or on MemoryBudgetBytes in reserve or promote (a
// refusal, or the second reservation waits).
func TestUnsetBoundsAdmitEveryRunAtOnce(t *testing.T) {
	r, err := NewRunner(Limits{})
	if err != nil {
		t.Fatalf("a runner with no bounds of its own was refused: %v", err)
	}
	// A wait that never ends must fail as an assertion, not as the binary's
	// timeout: the reservations get half of whatever time the test has left.
	ctx := t.Context()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Until(deadline)/2)
		defer cancel()
	}
	for i := range 2 {
		release, err := r.reserve(ctx, Spec{MemoryReservationBytes: 1 << 62})
		if err != nil {
			t.Fatalf("reservation %d of 1<<62 bytes on a runner with no bounds: %v", i, err)
		}
		defer release()
	}
}
