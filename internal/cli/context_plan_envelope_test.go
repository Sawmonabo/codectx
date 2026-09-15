package cli

import (
	"encoding/json"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestATruncatedPlanEnvelopeCarriesNoSession pins the wire shape ruling C9
// promises and docs/context-sessions.md documents: a compile that stopped at a
// pass boundary opened no session, so the envelope carries the continuation and
// NO `session` object.
//
// It is a marshal test rather than an assertion about the struct because what
// decides the shape is `omitzero` on a STRUCT field -- reflection semantics, not
// code a reader can check -- and a zero SessionStatus encoded as a real object
// would show a machine consumer a blank session id and a failing gate as though
// a session had been opened and found wanting.
func TestATruncatedPlanEnvelopeCarriesNoSession(t *testing.T) {
	truncated := contextPlan{Plan: model.PlanResult{
		ActorID:          "lead",
		Truncated:        true,
		TruncationReason: "deadline",
		NextCursor:       "ctx-plan-token",
	}}
	raw, err := json.Marshal(truncated)
	if err != nil {
		t.Fatalf("marshal the truncated envelope: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the truncated envelope: %v", err)
	}
	if _, present := got["session"]; present {
		t.Errorf("the truncated envelope carries a session block: %s", raw)
	}
	var plan map[string]json.RawMessage
	if err := json.Unmarshal(got["plan"], &plan); err != nil {
		t.Fatalf("decode the plan: %v", err)
	}
	for _, field := range []string{"truncated", "truncation_reason", "next_cursor"} {
		if _, present := plan[field]; !present {
			t.Errorf("the truncated plan omits %q, so a client cannot continue: %s", field, raw)
		}
	}
	for _, field := range []string{"manifest", "session_id"} {
		if _, present := plan[field]; present {
			t.Errorf("the truncated plan carries %q; nothing was compiled and no session was opened: %s", field, raw)
		}
	}

	// The finished shape still carries its session, or the rule above would be
	// a suppression of every plan rather than of the truncated one.
	finished := contextPlan{Session: model.SessionStatus{SessionID: model.SessionID("s")}}
	raw, err = json.Marshal(finished)
	if err != nil {
		t.Fatalf("marshal the finished envelope: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode the finished envelope: %v", err)
	}
	if _, present := got["session"]; !present {
		t.Fatalf("a finished plan lost its session block: %s", raw)
	}
}
