package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Advance applies one guarded transition. The store owns reachability and the
// state_version compare-and-swap; this owns the Section 17.1 service guards the
// store deliberately does not check -- a resolved seed for a direct verify,
// required-full coverage plus a current-scope review for verify -> consolidate
// (or recorded waivers with allow_exploratory_waiver_consolidation), and
// capsule-before-complete ordering. Owned by L1.
func (s *Service) Advance(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error) {
	return model.WorkflowStatus{}, model.SessionStatus{}, errUnimplemented("Advance")
}

// Close closes a session under the caller's expected version. Closing is
// expressed as AdvanceRequest{Target: StateClosed}: there is no CloseSession on
// the store and no edge out of complete, so closing a completed session is
// CTX_VERSION_CONFLICT. Owned by L1.
func (s *Service) Close(ctx context.Context, req model.SessionRequest, expectedVersion int) (model.WorkflowStatus, error) {
	return model.WorkflowStatus{}, errUnimplemented("Close")
}
