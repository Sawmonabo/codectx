package context

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// max_manifest_bytes is a CALLER budget, so a caller that knows it can hold any
// manifest must be able to raise a finite deployment default all the way. Zero
// already means "take the configured value", so without a separate spelling the
// caller under a finite default could raise the budget to any number except the
// one it needs; that residual is a scale refusal (plan row 19).
//
// Mutation: resolve model.BudgetUnlimited through pick64 again and the request
// inherits the deployment ceiling, failing the first case.
func TestAManifestBudgetIsRaisableToUnlimitedByTheRequest(t *testing.T) {
	cfg := config.Context{
		DefaultMaxBytes: 1 << 20, DefaultEstimatedTokens: 1000, DefaultMaxFiles: 10, MaxSlices: 4,
		MaxManifestBytes: config.Limit(2048),
	}
	for _, tc := range []struct {
		name      string
		requested int64
		want      config.Limit
	}{
		{"the request removes the ceiling", model.BudgetUnlimited, config.Unlimited},
		{"zero inherits the deployment value", 0, config.Limit(2048)},
		{"a larger finite value wins", 8192, config.Limit(8192)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBudget(model.Budget{MaxManifestBytes: tc.requested}, cfg)
			if err != nil {
				t.Fatalf("resolveBudget(%d): %v", tc.requested, err)
			}
			if got.MaxManifestBytes != tc.want {
				t.Fatalf("max_manifest_bytes resolved to %s, want %s", got.MaxManifestBytes, tc.want)
			}
		})
	}
}
