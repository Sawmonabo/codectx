package pagination

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestPageLimitReportsWhatItClamped protects the one rule that keeps a clamped
// page distinguishable from the end of an answer.
//
// Failure mode: a caller asks for more rows than the wire ceiling, is served the
// ceiling, and nothing on the answer says so -- it reads as a complete result.
// Since an edge batch limit of 0 became "no caller bound" rather than a refusal,
// the silent path is now reachable from ordinary configuration.
//
// Mutation: drop the record call in pageLimit and the clamped case reports no
// notice while still returning 200.
func TestPageLimitReportsWhatItClamped(t *testing.T) {
	ctx, clamps := WithPageClamps(context.Background())

	if got := PageLimit(ctx, model.MaxPageItems+50); got != model.MaxPageItems {
		t.Fatalf("pageLimit served %d for a request above the ceiling; want %d", got, model.MaxPageItems)
	}
	// The same clamp seen twice is one fact, not two: a keyset walk calls the
	// reader once per page.
	PageLimit(ctx, model.MaxPageItems+50)
	notes := clamps.Notices()
	if len(notes) != 1 {
		t.Fatalf("a clamped page produced %d notices %q; want exactly one", len(notes), notes)
	}

	// A request of 0 is "no caller-side bound", not a number the caller chose,
	// and an in-range request is served as asked; neither is a clamp to report.
	if got := PageLimit(ctx, 0); got != model.MaxPageItems {
		t.Fatalf("pageLimit resolved an unbounded request to %d; want %d", got, model.MaxPageItems)
	}
	if got := PageLimit(ctx, 7); got != 7 {
		t.Fatalf("pageLimit served %d for an in-range request of 7", got)
	}
	if len(clamps.Notices()) != 1 {
		t.Fatalf("an unbounded or in-range request was reported as a clamp: %q", clamps.Notices())
	}

	// An edge batch limit of 0 is a USER setting meaning unlimited, so its
	// resolution to the ceiling is reported where pageLimit's bare zero is not.
	RecordUnbounded(ctx, model.MaxPageItems)
	if len(clamps.Notices()) != 2 {
		t.Fatalf("an unlimited edge batch request was not reported: %q", clamps.Notices())
	}

	// Without a collector installed the clamp still applies and nothing panics.
	if got := PageLimit(context.Background(), model.MaxPageItems+1); got != model.MaxPageItems {
		t.Fatalf("pageLimit without a collector served %d; want %d", got, model.MaxPageItems)
	}
	RecordUnbounded(context.Background(), model.MaxPageItems)
}
