package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Include extends a pinned session's scope: it recompiles through the Compiler
// at the session's pinned generation, persists the manifest and hands it to
// IncludeManifest, which bumps the scope and state versions, appends
// session_manifests and retains same-hash coverage. It never deletes an
// observation -- the prior scope review stops satisfying readiness because the
// scope version moved, not because anything was removed. Owned by L2.
func (s *Service) Include(ctx context.Context, req model.IncludeRequest) (model.SessionStatus, error) {
	return model.SessionStatus{}, errUnimplemented("Include")
}
