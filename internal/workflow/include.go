package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// includeTask is the task text every include recompiles under. A session
// records its manifest's request hash and never the original task text, so the
// task cannot be recovered; a constant one keeps the recompile deterministic
// (the same seeds under the same binding reuse the same immutable manifest) and
// carries no session or run id into manifest identity, which Section 15.1
// excludes from the canonical projection. It names no path and no qualified
// identifier, so it contributes no task-derived seed of its own and cannot turn
// the recompiled manifest's ScopeComplete false by itself.
const includeTask = "context include"

// Include extends a pinned session's scope: it recompiles through the Compiler
// at the session's pinned generation, persists the manifest and hands it to
// IncludeManifest, which bumps the scope and state versions, appends
// session_manifests and retains same-hash coverage. It never deletes an
// observation -- the prior scope review stops satisfying readiness because the
// scope version moved, not because anything was removed. Owned by L2.
func (s *Service) Include(ctx context.Context, req model.IncludeRequest) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	ctx, cancel := model.QueryDeadline(ctx, s.limits.QueryTimeout)
	defer cancel()

	// The request is validated, so the actor is non-empty and Session applies
	// its actor, expiry and lifecycle checks. An include is a mutation: an
	// expired or wrong-actor session is refused here rather than read past.
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	// The session's current manifest supplies the budget policy the recompile
	// runs under, so extending scope cannot quietly raise the ceiling the
	// session was planned against.
	current, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	// The pinned generation travels on the request: this service never picks a
	// generation, and naming the session's own is what keeps the new manifest
	// inside the snapshot the session reads. The phase comes from the session
	// record because IncludeManifest never rewrites read_sessions.phase.
	compiled, err := s.compile.Compile(ctx, model.ContextRequest{
		Task:         includeTask,
		Seeds:        req.Seeds,
		Phase:        rec.Phase,
		Budget:       current.Budget,
		GenerationID: rec.Binding.GenerationID,
	})
	if err != nil {
		return model.SessionStatus{}, err
	}
	if compiled.Binding != rec.Binding {
		// The store compares the generation and the snapshot; this also
		// refuses a changed analysis key, and refusing before the write keeps
		// a recompile that landed on another binding out of the session's
		// history rather than reporting it after the fact.
		return model.SessionStatus{}, typedErrf(model.CodeSessionSuperseded,
			"the recompiled manifest is bound to another generation or snapshot than this session's")
	}

	status, err := s.sessions.IncludeManifest(ctx, req, compiled.ID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	// Invalidation is reported, never performed: nothing is deleted and no
	// observation is rewritten. A scope review recorded at the prior scope
	// version is simply no longer current, so it stops counting toward
	// readiness, while same-actor same-hash coverage survives the INSERT OR
	// IGNORE derivation untouched.
	s.log.Info("session scope extended; a scope review recorded at the prior scope version no longer satisfies readiness",
		"session", rec.ID, "scope_version_from", rec.ScopeVersion, "scope_version_to", status.ScopeVersion,
		"manifest", compiled.ID)

	// Readiness is evaluated against the session as it now stands, never
	// against the record loaded before the include.
	rec, err = s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	return s.status(ctx, rec)
}
