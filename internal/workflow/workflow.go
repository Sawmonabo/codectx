// Package workflow is the Section 17 service layer: the transition guards, the
// strict readiness gate, source-backed scope review and deterministic capsule
// identity. It is a service over work that has already landed -- Task 2 shipped
// every §17.2/§17.3 model type including NewObservationID, and Task 5 shipped
// AdvanceSession's transition table and state_version compare-and-swap,
// IncludeManifest, PutObservation, Observations, Waive, PutCapsule and Capsule.
//
// Nothing here re-implements any of that. There is no second transition table,
// no second observation preimage, no Go-side interval merge, no new SQL table
// and no second capsule store; the store owns reachability and the CAS, and
// this package owns only what the store deliberately does not check.
//
// This package imports neither internal/config nor *sqlite.Store's wider
// surface: internal/app supplies the frozen Sessions, Compiler and Validator
// interfaces and a Limits resolved from configuration.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Sessions is the only storage surface this package may use. *sqlite.Store
// satisfies it. Every method here already landed in Task 5, Task 12 or Task 16
// except the three marked NEW, which L6 and L6b append to the store; no method
// is redefined, re-spelled or wrapped by this package.
type Sessions interface {
	// Session checks exact actor identity and rejects a wrong actor, an
	// expired session and a closed session -- but it skips the actor check
	// when actor is empty, so every caller Validate()s its request first. It
	// returns a partially populated record beside CTX_SESSION_EXPIRED rather
	// than swallowing the record, so status stays honest.
	Session(ctx context.Context, id model.SessionID, actor string) (sqlite.SessionRecord, error)
	// AdvanceSession owns the Section 17.1 transition graph (allowedTransitions)
	// and the state_version compare-and-swap inside its own transaction. This
	// package never re-checks reachability in Go and never reads-then-writes
	// around the CAS: it presents the caller's ExpectedVersion and surfaces
	// CTX_VERSION_CONFLICT rather than retrying silently.
	AdvanceSession(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, error)
	// IncludeManifest bumps both the scope and state versions, appends
	// session_manifests and re-runs the session_files derivation as INSERT OR
	// IGNORE, so same-actor same-hash coverage is retained for free. A changed
	// generation or snapshot binding is CTX_SESSION_SUPERSEDED.
	IncludeManifest(ctx context.Context, req model.IncludeRequest, manifest model.ManifestID) (model.WorkflowStatus, error)
	// Waive records one required-file waiver. A waiver never grants strict
	// readiness; SessionStatus.Validate refuses that combination outright.
	Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, error)
	// PutObservation rejects a stale ScopeVersion with CTX_SCOPE_CHANGED and is
	// INSERT OR IGNORE on the content-derived id, so a duplicate submission is
	// already idempotent and an existing observation is never rewritten.
	PutObservation(ctx context.Context, o model.Observation) error
	// Observations pages one actor's observations by keyset on the observation
	// id, filtered by kind and scope version.
	Observations(ctx context.Context, session model.SessionID, actor string, kind model.ObservationKind,
		scopeVersion int, after model.ObservationID, limit int) ([]model.Observation, error)
	// PutCapsule is write-once: context_capsules.session_id is the primary key,
	// so it returns the first stored capsule unchanged when a row exists. It
	// does no canonical-hash comparison -- that determinism check is this
	// package's (see canonicalCapsuleHash).
	PutCapsule(ctx context.Context, c model.Capsule) (model.Capsule, error)
	Capsule(ctx context.Context, session model.SessionID, actor string) (model.Capsule, error)
	// Coverage pages one actor's per-file coverage by keyset on file_id. It is
	// paged only for a user-visible list; counts come from CoverageSummary.
	Coverage(ctx context.Context, session model.SessionID, actor string, after model.FileID, limit int) ([]model.FileCoverage, error)
	// CoverageSummary is Task 16's aggregate and this package defines no second
	// one: the readiness gate makes exactly one call per evaluation.
	CoverageSummary(ctx context.Context, session model.SessionID, actor string) (required, fullyServed, waived int64, err error)
	Manifest(ctx context.Context, id model.ManifestID) (model.ContextManifest, error)
	ManifestEntries(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ContextEntry, error)
	// RangeConfirmed reports whether one interval is already fully confirmed
	// served to this actor for this file at this content hash. It returns a
	// bounded boolean, never an interval list: containment is answered in SQL
	// over served_ranges and no interval list crosses into Go. NEW -- L6.
	RangeConfirmed(ctx context.Context, session model.SessionID, actor string, file model.FileID,
		hash string, r model.ByteRange) (bool, error)
	// SessionFilePaths resolves a bounded batch of pinned file ids to their
	// paths. Coverage and manifest records carry identifiers and no path, and
	// naming a file the operator cannot locate is not an answer. NEW -- L6.
	SessionFilePaths(ctx context.Context, session model.SessionID, actor string,
		ids []model.FileID) (map[model.FileID]string, error)
	// Waivers reads the session's recorded coverage exceptions in file-id
	// order. Waive's returned record echoes the request's reason and
	// FileCoverage carries only the Waived flag, so this is the only source of
	// the stored reasons a sealed capsule must carry. NEW -- L6b.
	Waivers(ctx context.Context, session model.SessionID, actor string) ([]model.WaiverRecord, error)
	// ActiveGeneration is the repository's currently published generation. It
	// is Task 12's existing read (internal/storage/sqlite/units.go), widened
	// onto this interface rather than reinvented: supersession is "a newer
	// generation is active than the one this session pinned", and deriving it
	// from per-file content hashes instead -- as this service did before INT --
	// reported Superseded false for the exact case Section 16.3 names, a newer
	// generation active with the pinned files untouched.
	ActiveGeneration(ctx context.Context, repo model.RepositoryID) (model.GenerationID, error)
}

// The store is the production Sessions; the assertion keeps the interface
// honest against it rather than discovering a drift at composition.
var _ Sessions = (*sqlite.Store)(nil)

// Compiler recompiles a manifest when Include extends a session's scope. The
// pinned generation travels on the request, so this package never picks one.
type Compiler interface {
	Compile(ctx context.Context, req model.ContextRequest) (model.ContextManifest, error)
}

// Validator is current-source revalidation: has this file's content hash
// changed under the session since it was read? internal/app supplies it over
// the memoised *snapshot.View, which is what keeps *snapshot.CAS and the store's
// wider surface out of this package.
//
// It is consulted per request, immediately before answering -- never cached.
// A readiness answer that was true a minute ago is not an answer.
type Validator interface {
	Current(ctx context.Context, file model.FileID, hash string) (bool, error)
}

// Options composes the service. Sessions, Compile, Validate and every Limits
// bound are required: the composition root builds this eagerly when a workspace
// opens, so a missing one is a wiring defect that must fail there rather than
// per request. Logger is the one exception New tolerates.
type Options struct {
	Sessions Sessions
	Compile  Compiler
	Validate Validator
	Limits   Limits
	Now      func() time.Time
	Logger   *slog.Logger
}

// Limits are the Section 20.1 bounds resolved from configuration by the caller.
// This package never reads config.Config, so every bound it enforces is here.
// Zero on a request field means the configured default, never unlimited.
type Limits struct {
	// MaxPageItems bounds every page this service returns
	// (resources.max_page_items).
	MaxPageItems int
	// MaxObservationReferences bounds the references on one observation
	// (model.MaxObservationReferences).
	MaxObservationReferences int
	// MaxCapsuleBytes bounds the serialized capsule (context.max_capsule_bytes).
	// The store enforces the same bound; this is the value the service builds
	// against so an over-budget capsule fails before the write.
	MaxCapsuleBytes int64
	// QueryTimeout is the per-request deadline (resources.query_timeout).
	QueryTimeout time.Duration
	// AllowExploratoryWaiverConsolidation permits verify_open ->
	// consolidate_open with recorded waivers
	// (context.allow_exploratory_waiver_consolidation, user-level only,
	// default false). It never changes StrictGateSatisfied, which stays false
	// whenever any waiver exists.
	AllowExploratoryWaiverConsolidation bool
}

// Service is the Section 17 workflow service. Every field is read-only after
// New, so one service answers concurrent requests for the workspace that built
// it and holds no per-request state.
type Service struct {
	sessions Sessions
	compile  Compiler
	validate Validator
	limits   Limits
	now      func() time.Time
	log      *slog.Logger
}

// New validates the options and builds the service.
func New(o Options) (*Service, error) {
	if o.Sessions == nil {
		return nil, typedErrf(model.CodeInternal, "workflow service was built without a session store")
	}
	if o.Compile == nil {
		return nil, typedErrf(model.CodeInternal, "workflow service was built without a context compiler")
	}
	if o.Validate == nil {
		return nil, typedErrf(model.CodeInternal, "workflow service was built without a source validator")
	}
	if err := checkLimits(o.Limits); err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{
		sessions: o.Sessions,
		compile:  o.Compile,
		validate: o.Validate,
		limits:   o.Limits,
		now:      now,
		log:      log,
	}, nil
}

// checkLimits rejects a Limits that cannot bound a request. Zero on a request
// field means the configured default, so a zero default means "unlimited",
// which Section 20.2 forbids.
func checkLimits(l Limits) error {
	for _, b := range []struct {
		name  string
		value int64
	}{
		{"max_page_items", int64(l.MaxPageItems)},
		{"max_observation_references", int64(l.MaxObservationReferences)},
		{"max_capsule_bytes", l.MaxCapsuleBytes},
		{"query_timeout", int64(l.QueryTimeout)},
	} {
		if b.value <= 0 {
			return typedErrf(model.CodeInternal,
				"workflow service was built with a non-positive %s bound", b.name)
		}
	}
	return nil
}

// typedErrf is this package's only error constructor: every failure that leaves
// it carries a Section 20.3 code, and no detail ever carries a source body, a
// receipt token, raw SQL or a private absolute root.
func typedErrf(code, format string, args ...any) *model.Error {
	return &model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// errUnimplemented is the placeholder body of every declaration L0 froze and a
// fill-in lane owns. It is a typed CTX_INTERNAL rather than a panic so a lane
// that lands before its siblings cannot crash the process it is wired into.
func errUnimplemented(op string) error {
	return typedErrf(model.CodeInternal, "workflow: %s is not implemented", op)
}

// --- shared unexported surface: names frozen by L0, bodies owned by a lane ---

// gate is one readiness evaluation. It is the single value Advance, Status and
// buildCapsule all read, so the Section 16.3 preconditions are evaluated in one
// place and never re-derived per caller. Owned by L4.
type gate struct {
	// ReadComplete is read completeness for the pinned snapshot; Ready is
	// strict implementation readiness; Strict is the strict gate itself, which
	// is false whenever any waiver exists.
	ReadComplete, Ready, Strict, ScopeComplete bool
	// Superseded reports that a newer generation is active than the one this
	// session pinned. It rides on the gate rather than beside it because Ready
	// is defined in terms of it and every gate reader must see the same answer.
	Superseded bool
	// Required, Served and Waived come from one CoverageSummary call.
	Required, Served, Waived int64
	// Blocking names the unresolved observations that hold the gate shut.
	Blocking []model.ObservationID
	// Reason states, for an operator, why the gate is shut -- or, when it is
	// open, carries the point-in-time guarantee limit (ruling Q10).
	Reason string
}

// canonicalCapsuleDomain is the hash domain of the Section 17.3 capsule
// identity preimage. The version suffix is part of the domain, so a change to
// the preimage's composition yields a disjoint identity space rather than
// silently reinterpreting stored hashes. Owned by L5.
const canonicalCapsuleDomain = "codectx.capsule.canonical.v1"

// errNotConsolidating is the sentinel for a capsule asked for outside the one
// state that produces it. Owned by L5.
var errNotConsolidating = errors.New("capsule is produced only while consolidate is open")

// --- Also frozen by L0; implemented in files this package does not own -------
//
// These signatures are recorded here as the map of what wires Task 17 together.
// Changing one of them is a controller question, not a lane decision.
//
//	// internal/storage/sqlite/state.go -- L6 owns; APPEND ONLY, no existing method is edited
//	func (s *Store) RangeConfirmed(ctx context.Context, session model.SessionID, actor string,
//	        file model.FileID, hash string, r model.ByteRange) (bool, error)
//	func (s *Store) SessionFilePaths(ctx context.Context, session model.SessionID, actor string,
//	        ids []model.FileID) (map[model.FileID]string, error)
//
//	// internal/app/compose.go + workspace.go -- INT owns
//	func (s *stack) openWorkflow() error            // openQueries-style; workflow.New(...) onto the stack
//
//	// internal/app/overlay.go -- L8 owns (new file)
//	func (s *stack) overlaySymbols(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error)
//	func (s *stack) overlayReferences(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error)
//
//	// internal/cli/context.go -- L9 owns; newContextCommands already exists and INT adds no root.go line
