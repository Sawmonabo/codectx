package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestPlanResultIsExactlyOneShape pins ruling C9's exactly-one-of rule: a
// PlanResult is EITHER a compiled plan (manifest + session) OR a continuation
// (cursor + truncated), never both and never neither.
//
// The invariant is worth a test of its own because both broken shapes are
// silent failures rather than crashes. A result carrying both would let a
// reader take a continuation for an answer and bind a session to a manifest
// that was never persisted; a result carrying neither is an empty success a
// caller cannot act on or distinguish from a finished plan that selected
// nothing.
func TestPlanResultIsExactlyOneShape(t *testing.T) {
	const cursor = "ctx-plan-token"

	both := model.PlanResult{
		Manifest:         validManifest(),
		SessionID:        model.SessionID(model.H("session-v1", "s")),
		ActorID:          "lead",
		Truncated:        true,
		TruncationReason: "deadline",
		NextCursor:       cursor,
	}
	neither := model.PlanResult{ActorID: "lead"}
	cursorWithoutFlag := model.PlanResult{ActorID: "lead", NextCursor: cursor}
	flagWithoutCursor := model.PlanResult{ActorID: "lead", Truncated: true, TruncationReason: "deadline"}

	tests := []struct {
		protects string
		value    model.PlanResult
		wants    string
	}{
		{"a result that is both an answer and a continuation", both, "both"},
		{"a result that is neither", neither, "neither"},
		{"a cursor offered without the truncated flag a client branches on", cursorWithoutFlag, "without truncated"},
		{"a truncated report with no cursor to continue from", flagWithoutCursor, "next_cursor"},
	}
	for _, tc := range tests {
		t.Run(tc.protects, func(t *testing.T) {
			err := tc.value.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %#v, want a typed rejection", tc.value)
			}
			var typed *model.Error
			if !errors.As(err, &typed) {
				t.Fatalf("Validate returned %T, want *model.Error", err)
			}
			if !strings.Contains(typed.Message, tc.wants) {
				t.Errorf("rejection %q does not name %q; the message is what tells a producer which half is wrong",
					typed.Message, tc.wants)
			}
		})
	}

	// Both legitimate shapes are accepted, or the rule above would be a refusal
	// of every plan rather than a discriminator between two.
	finished := model.PlanResult{
		Manifest:  validManifest(),
		SessionID: model.SessionID(model.H("session-v1", "s")),
		ActorID:   "lead",
	}
	if err := finished.Validate(); err != nil {
		t.Fatalf("a compiled plan was rejected: %v", err)
	}
	continuation := model.PlanResult{
		ActorID:          "lead",
		Truncated:        true,
		TruncationReason: "deadline",
		NextCursor:       cursor,
	}
	if err := continuation.Validate(); err != nil {
		t.Fatalf("a continuation was rejected: %v", err)
	}
}

// validManifest is the smallest ContextManifest that passes Validate, so the
// rejections above are about the exactly-one-of rule and never about a manifest
// field the fixture forgot.
func validManifest() model.ContextManifest {
	id := model.H("manifest-v1", "plan")
	return model.ContextManifest{
		ID: model.ManifestID(id),
		Binding: model.Binding{
			RepositoryID: model.RepositoryID(model.H("repository-v1", "repo")),
			SnapshotID:   model.SnapshotID(model.H("snapshot-v1", "snap")),
			GenerationID: 1,
		},
		Phase:          model.PhaseSweep,
		RequestHash:    model.H("request-v1", "req"),
		PolicyVersion:  "v1",
		CanonicalHash:  model.H("canonical-v1", "canon"),
		EstimateMethod: "measured",
	}
}
