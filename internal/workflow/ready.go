package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Status is the honest readiness answer for one session (Section 16.3). It is
// revalidated per request, immediately before answering, and never cached.
// Owned by L4.
func (s *Service) Status(ctx context.Context, req model.SessionRequest) (model.SessionStatus, error) {
	return model.SessionStatus{}, errUnimplemented("Status")
}

// readiness evaluates the full Section 16.3 precondition set exactly once per
// request: one CoverageSummary call, the manifest's ScopeComplete, the verify
// phase, a current-scope review, a blocking-unresolved scan, waived == 0 and
// per-request Validator revalidation of every cited source. When the gate is
// open the result carries the point-in-time guarantee limit (ruling Q10): a
// silent true is a defect. Owned by L4.
func (s *Service) readiness(ctx context.Context, rec sqlite.SessionRecord, m model.ContextManifest) (gate, error) {
	return gate{}, errUnimplemented("readiness")
}

// status projects one readiness evaluation onto all nineteen SessionStatus
// fields, Superseded included. SessionStatus.Validate refuses the dishonest
// combinations, so a wrong evaluator fails loudly rather than silently.
// Owned by L4.
func (s *Service) status(ctx context.Context, rec sqlite.SessionRecord) (model.SessionStatus, error) {
	return model.SessionStatus{}, errUnimplemented("status")
}
