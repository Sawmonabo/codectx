package workflow

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestCapsuleBytesIsACallerBudgetUnlimitedByDefault protects the row-19 rule
// that context.max_capsule_bytes is a caller budget, not a scale refusal.
//
// Two failure modes it guards. First, the default: config defaults the key to
// config.Unlimited, and a service that compared bytes against the raw 0 refused
// EVERY seal and EVERY export on stock configuration. Second, the report: a
// caller that did set a ceiling must be told both numbers -- the ceiling it set
// and the size the capsule reached -- or raising it is a guess.
func TestCapsuleBytesIsACallerBudgetUnlimitedByDefault(t *testing.T) {
	c := model.Capsule{Counts: model.CapsuleCounts{Coverage: 4096}}

	unlimited := &Service{limits: Limits{MaxCapsuleBytes: config.Unlimited}}
	if err := unlimited.checkCapsuleBytes(c); err != nil {
		t.Fatalf("the default (unlimited) capsule budget refused a capsule: %v", err)
	}

	bounded := &Service{limits: Limits{MaxCapsuleBytes: config.Limit(8)}}
	err := bounded.checkCapsuleBytes(c)
	if err == nil {
		t.Fatal("a caller-set 8-byte capsule budget admitted a capsule far over it")
	}
	typed, ok := err.(*model.Error)
	if !ok || typed.Code != model.CodeResourceLimit {
		t.Fatalf("over-budget capsule reported %v, want a %s error", err, model.CodeResourceLimit)
	}
	if typed.Details["limit_value"] != "8" {
		t.Fatalf("the error names limit_value %q, want the caller's own 8", typed.Details["limit_value"])
	}
	if typed.Details["capsule_bytes"] == "" {
		t.Fatal("the error does not name the size the capsule reached, so raising the budget is a guess")
	}
	if !strings.Contains(typed.Remediation, "unlimited") {
		t.Fatalf("remediation %q does not say the ceiling can be removed", typed.Remediation)
	}
}

// TestCapsuleRecordCeilingsAreUnlimitedByDefault protects the two new keys.
//
// Stock configuration must seal a session of any size: both
// context.max_capsule_records_per_list and context.max_capsule_coverage_files
// default to unlimited, and neither may refuse. A ceiling an operator DID set
// must refuse the seal outright -- Section 17.3 forbids a truncated list -- and
// name the key it checked and the count the list reached.
func TestCapsuleRecordCeilingsAreUnlimitedByDefault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rec, g := h.consolidating(t)
	// The fixture session's only populated list is coverage, so the
	// records-per-list ceiling needs a list of its own to exceed.
	h.seedUnresolved(t, 2)

	if _, err := h.svc.buildCapsule(ctx, rec, g); err != nil {
		t.Fatalf("stock (unlimited) capsule record ceilings refused a seal: %v", err)
	}

	for _, tc := range []struct {
		name   string
		limits Limits
		key    string
		list   model.CapsuleList
	}{
		{"records per list", Limits{MaxCapsuleRecordsPerList: 1},
			"context.max_capsule_records_per_list", model.CapsuleListUnresolved},
		{"coverage files", Limits{MaxCapsuleCoverageFiles: 1},
			"context.max_capsule_coverage_files", model.CapsuleListCoverage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := *h.svc
			svc.limits.MaxCapsuleRecordsPerList = tc.limits.MaxCapsuleRecordsPerList
			svc.limits.MaxCapsuleCoverageFiles = tc.limits.MaxCapsuleCoverageFiles
			_, err := svc.buildCapsule(ctx, rec, g)
			if err == nil {
				t.Fatalf("a %s of 1 sealed a capsule whose %s list holds more", tc.key, tc.list)
			}
			typed, ok := err.(*model.Error)
			if !ok || typed.Code != model.CodeResourceLimit {
				t.Fatalf("the refusal reported %v, want a %s error", err, model.CodeResourceLimit)
			}
			if typed.Details["limit"] != tc.key {
				t.Fatalf("the refusal names limit %q, want %q", typed.Details["limit"], tc.key)
			}
			if typed.Details["limit_value"] != "1" {
				t.Fatalf("the refusal names limit_value %q, want the operator's own 1", typed.Details["limit_value"])
			}
			if typed.Details["records_found"] == "" {
				t.Fatal("the refusal does not name the count the list reached")
			}
			if !strings.Contains(typed.Remediation, "unlimited") {
				t.Fatalf("remediation %q does not say the ceiling can be removed", typed.Remediation)
			}
		})
	}
}

// TestCapsuleSealStreamsTheSessionThreePerList protects ruling D3: the seal
// makes three bounded passes over the SESSION STORE -- count, hash, write --
// and retains no list between them.
//
// This is the invariant no functional assertion can reach: a source that
// buffered pass 1 and replayed it would produce an identical digest and
// identical rows while holding a whole list in heap, which is the memory the
// row redesign exists to remove. Only the walk count sees it. Mutating
// capsuleSource into a memoising source drops every list from 3 to 1 and fails
// here.
func TestCapsuleSealStreamsTheSessionThreePerList(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	rec, g := h.consolidating(t)

	h.store.mu.Lock()
	h.store.walks = map[model.CapsuleList]int{}
	h.store.mu.Unlock()

	if _, err := h.svc.buildCapsule(ctx, rec, g); err != nil {
		t.Fatalf("seal the capsule: %v", err)
	}

	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	for _, list := range model.CapsuleListOrder {
		if got := h.store.walks[list]; got != 3 {
			t.Fatalf("the seal walked the session's %s list %d times, want 3 "+
				"(count, hash, write); fewer means a pass was served from a buffered list", list, got)
		}
	}
}

// consolidating opens the fixture session's consolidate phase and returns the
// record and the readiness gate a seal is built from. The fixture's waiver is
// withdrawn whole -- the flag and the stored record -- because these rows seal
// under a satisfied strict gate and Capsule.Validate refuses that beside a
// recorded waiver.
func (h *harness) consolidating(t *testing.T) (sqlite.SessionRecord, gate) {
	t.Helper()
	if _, err := h.store.AdvanceSession(context.Background(), model.AdvanceRequest{
		SessionID: fixtureSession, ActorID: fixtureActor,
		Target: model.StateConsolidateOpen, ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("open consolidate on the fixture session: %v", err)
	}
	h.file(fixtureSession, fileWaived).waived = false
	h.store.sessions[fixtureSession].waivers = nil
	return h.session(fixtureSession), gate{ReadComplete: true, Ready: true, Strict: true, ScopeComplete: true}
}

// seedUnresolved writes n distinct unresolved observations at the session's
// current scope version straight into the fake, which is how every row here
// arranges observations it is not testing the recording of.
func (h *harness) seedUnresolved(t *testing.T, n int) {
	t.Helper()
	fs := h.store.sessions[fixtureSession]
	for i := 0; i < n; i++ {
		req := model.ObservationRequest{
			SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: fs.rec.ScopeVersion,
			Kind: model.ObservationUnresolved, Note: "open question " + strconv.Itoa(i),
		}
		fs.obs = append(fs.obs, model.Observation{
			ID: model.NewObservationID(req), SessionID: fixtureSession, ActorID: fixtureActor,
			ScopeVersion: fs.rec.ScopeVersion, Kind: model.ObservationUnresolved,
			Note: req.Note, CreatedAt: fixtureNow,
		})
	}
}
