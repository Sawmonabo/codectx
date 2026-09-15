package workflow

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCapsuleBytesIsACallerBudgetUnlimitedByDefault protects the row-19 rule
// that context.max_capsule_bytes is a caller budget, not a scale refusal.
//
// Two failure modes it guards. First, the default: config now defaults the key
// to config.Unlimited, and a service that compared bytes against the raw 0
// refused EVERY seal and EVERY export on stock configuration. Second, the
// report: a caller that did set a ceiling must be told both numbers -- the
// ceiling it set and the size the capsule reached -- or raising it is a guess.
func TestCapsuleBytesIsACallerBudgetUnlimitedByDefault(t *testing.T) {
	c := model.Capsule{Coverage: make([]model.FileCoverage, 4096)}

	unlimited := &Service{limits: Limits{MaxCapsuleBytes: config.Unlimited}}
	if err := unlimited.checkCapsuleBytes(c); err != nil {
		t.Fatalf("the default (unlimited) capsule budget refused a capsule: %v", err)
	}

	bounded := &Service{limits: Limits{MaxCapsuleBytes: config.Limit(64)}}
	err := bounded.checkCapsuleBytes(c)
	if err == nil {
		t.Fatal("a caller-set 64-byte capsule budget admitted a capsule far over it")
	}
	typed, ok := err.(*model.Error)
	if !ok || typed.Code != model.CodeResourceLimit {
		t.Fatalf("over-budget capsule reported %v, want a %s error", err, model.CodeResourceLimit)
	}
	if typed.Details["limit_value"] != "64" {
		t.Fatalf("the error names limit_value %q, want the caller's own 64", typed.Details["limit_value"])
	}
	if typed.Details["capsule_bytes"] == "" {
		t.Fatal("the error does not name the size the capsule reached, so raising the budget is a guess")
	}
	if !strings.Contains(typed.Remediation, "unlimited") {
		t.Fatalf("remediation %q does not say the ceiling can be removed", typed.Remediation)
	}
}
