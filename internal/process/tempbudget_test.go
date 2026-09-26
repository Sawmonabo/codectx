package process

import (
	"context"
	"errors"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// resources.max_temp_bytes is a BOUND, and its default is unlimited. A runner
// built from the default must construct and must admit a run of any disk
// reservation: a runner that refused a non-positive disk budget would fail
// application construction rather than any one run. Only a value the operator
// set may refuse a run, and it must say which key to raise.
//
// Mutation proof: add `|| limits.DiskBudgetBytes <= 0` to NewRunner's
// positive-bound check and this fails with
//
//	the default temporary budget refused the runner: ... both reservations must be positive
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
