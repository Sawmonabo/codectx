// This is the whole test budget for Task 17: one file, in-package so a lane can
// prove the unexported readiness, citationsServed and canonicalCapsuleHash
// directly, one fake and one scenario table.
//
// The table is empty here by design. L0 owns the fixture and the runner; each
// fill-in lane appends its own self-contained scenario under its own marker and
// nothing else in this file moves. A row exists only to protect an invariant
// whose silent breakage grants false write readiness, breaks capsule
// determinism, bypasses an actor or version guard or leaks unsealed facts --
// re-asserting Task 5's transition table, PutObservation idempotency or
// PutCapsule's write-once is a redundant test and a defect.
package workflow

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// --- the scenario table -----------------------------------------------------

// scenario is one named invariant. run gets a freshly built harness, so rows
// never share mutable fixture state and can be reordered freely.
type scenario struct {
	name string
	run  func(t *testing.T, h *harness)
}

func TestWorkflowScenarios(t *testing.T) {
	t.Parallel()
	cases := []scenario{
		// L1 rows
		// Failure mode: a service that picked its own version from a
		// pre-read record, retried the store's compare-and-swap or
		// swallowed its conflict would let two clients both believe they
		// closed the same session -- and the loser would carry a stale
		// ExpectedVersion it thinks is current. Closing is the transition
		// with no service guard, so this exercises the version path alone.
		{name: "two clients closing at the same expected version: exactly one wins", run: func(t *testing.T, h *harness) {
			expected := h.session(fixtureSession).StateVersion
			req := model.SessionRequest{SessionID: fixtureSession, ActorID: fixtureActor}

			type attempt struct {
				status model.WorkflowStatus
				err    error
			}
			attempts := make([]attempt, 2)
			var wg sync.WaitGroup
			wg.Add(len(attempts))
			for i := range attempts {
				go func(i int) {
					defer wg.Done()
					status, err := h.svc.Close(context.Background(), req, expected)
					attempts[i] = attempt{status: status, err: err}
				}(i)
			}
			wg.Wait()

			var won, lost int
			for _, a := range attempts {
				switch {
				case a.err == nil:
					won++
					if a.status.State != model.StateClosed {
						t.Errorf("the winning close reports state %q, want %q", a.status.State, model.StateClosed)
					}
					if a.status.StateVersion != expected+1 {
						t.Errorf("the winning close reports state version %d, want %d", a.status.StateVersion, expected+1)
					}
				case code(a.err) == model.CodeVersionConflict:
					lost++
				default:
					t.Errorf("the losing close failed with %q: %v", code(a.err), a.err)
				}
			}
			if won != 1 || lost != 1 {
				t.Fatalf("%d closes won and %d hit a version conflict; want exactly one of each", won, lost)
			}
			if got := h.session(fixtureSession).State; got != model.StateClosed {
				t.Errorf("the session is in state %q after both closes, want %q", got, model.StateClosed)
			}
		}},
		// L2 rows
		// An include invalidates the prior scope review by moving the scope
		// version, never by deleting it, and the coverage the actor already
		// earned at the same content hash survives the INSERT OR IGNORE
		// derivation. A service that extended scope without the version bump
		// would leave a stale review satisfying the gate; one that deleted the
		// observation or the coverage would destroy the audit record and send
		// the actor back to re-read bytes it has already confirmed.
		{"include invalidates the prior scope review while same-hash coverage survives", func(t *testing.T, h *harness) {
			ctx := context.Background()
			before := h.session(fixtureSession)
			servedHash := h.file(fixtureSession, fileFull).hash

			// The review is inserted through the store rather than through
			// Record: the eight-category and citation rules are L3's guard, and
			// this row needs only a review the gate would count as current.
			oreq := model.ObservationRequest{
				SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: before.ScopeVersion,
				Kind: model.ObservationScopeReview, Note: "scope reviewed at the planned scope",
				Review: &model.ScopeReview{
					ManifestHash: fixtureID("manifest", "canonical"), ScopeVersion: before.ScopeVersion,
				},
			}
			review := model.Observation{
				ID: model.NewObservationID(oreq), SessionID: oreq.SessionID, ActorID: oreq.ActorID,
				ScopeVersion: oreq.ExpectedScope, Kind: oreq.Kind, Review: oreq.Review,
				Note: oreq.Note, CreatedAt: fixtureNow,
			}
			if err := h.store.PutObservation(ctx, review); err != nil {
				t.Fatalf("record the scope review the include must invalidate: %v", err)
			}

			// What the recompile returns: a second immutable manifest on the
			// session's own binding that pulls one further file into scope.
			included := model.ManifestID(fixtureID("manifest", "included"))
			extra := model.FileID(fixtureID("file", "included"))
			h.store.mu.Lock()
			m := h.store.manifests[fixtureManifest]
			m.ID, m.EntryCount, m.CanonicalHash = included, 1, fixtureID("manifest", "included", "canonical")
			h.store.manifests[included] = m
			h.store.entries[included] = []model.ContextEntry{{
				Ordinal: 0, FileID: extra, Requirement: model.RequirementFull,
				ScoreMicros: 900, EstimatedBytes: 10, EstimatedTokens: 2, Reasons: []string{"seed"},
			}}
			h.store.current[extra] = fixtureID("hash", "included")
			h.store.compiled = m
			h.store.mu.Unlock()

			_, err := h.svc.Include(ctx, model.IncludeRequest{
				SessionID: fixtureSession, ActorID: fixtureActor,
				Seeds: []string{"internal/c/included.go"}, ExpectedVersion: before.StateVersion,
			})
			// Include's tail projects the new record through L4's status, which
			// is still a stub in this lane; its store-visible work is committed
			// by then, so this row asserts on the store. When L4 lands the error
			// is nil and every assertion below is unchanged.
			if err != nil && err.Error() != errUnimplemented("status").Error() {
				t.Fatalf("include: %v", err)
			}

			after := h.session(fixtureSession)
			// The review is untouched at the scope version it was recorded
			// under -- nothing was deleted or rewritten.
			prior, err := h.store.Observations(ctx, fixtureSession, fixtureActor,
				model.ObservationScopeReview, before.ScopeVersion, "", 0)
			if err != nil {
				t.Fatalf("read the prior scope review: %v", err)
			}
			if len(prior) != 1 || prior[0].ID != review.ID {
				t.Fatalf("the include did not leave the prior scope review intact: %+v", prior)
			}
			// ...and it no longer counts, because no review is current at the
			// session's new scope version.
			current, err := h.store.Observations(ctx, fixtureSession, fixtureActor,
				model.ObservationScopeReview, after.ScopeVersion, "", 0)
			if err != nil {
				t.Fatalf("read the current scope review: %v", err)
			}
			if len(current) != 0 {
				t.Fatalf("a scope review still satisfies scope version %d after the include: %+v", after.ScopeVersion, current)
			}
			if after.ScopeVersion != before.ScopeVersion+1 {
				t.Fatalf("scope version is %d after the include; want %d", after.ScopeVersion, before.ScopeVersion+1)
			}
			if after.ManifestID != included {
				t.Fatalf("the session's current manifest is %q; want the recompiled %q", after.ManifestID, included)
			}
			// Same-actor same-hash coverage survives, so the include costs the
			// actor a review and not its confirmed reads.
			cov, err := h.store.Coverage(ctx, fixtureSession, fixtureActor, "", 0)
			if err != nil {
				t.Fatalf("read coverage after the include: %v", err)
			}
			var full *model.FileCoverage
			for i := range cov {
				if cov[i].FileID == fileFull {
					full = &cov[i]
				}
			}
			if full == nil {
				t.Fatalf("the include dropped %q from the session's scope", fileFull)
			}
			if full.ContentHash != servedHash || full.State != model.CoverageFullServed {
				t.Fatalf("coverage of the fully served file is %q/%s after the include; want %q/%s",
					full.ContentHash, full.State, servedHash, model.CoverageFullServed)
			}
			inScope := false
			for _, c := range cov {
				if c.FileID == extra {
					inScope = true
				}
			}
			if !inScope {
				t.Fatalf("the recompiled manifest's file %q never entered the session's scope", extra)
			}
		}},
		// L3 rows
		//
		// A scope review is the one observation that can claim a file was read,
		// so both rows below are about the same silent failure: a review that is
		// believed rather than checked against served_ranges would let an actor
		// attest its way to a satisfied strict gate without reading anything.
		{"Record/a citation over an unconfirmed interval is refused", func(t *testing.T, h *harness) {
			partial := h.file(fixtureSession, filePartial)
			// [30,70) spans the deliberate gap at [40,60): it overlaps two
			// confirmed intervals and is contained by neither.
			review := l3ScopeReview(fixtureID("manifest", "canonical"), 1, model.ReviewCallersConsumers,
				[]model.ClaimReference{{Source: &model.SourceCitation{
					FileID: filePartial, ContentHash: partial.hash,
					Bytes: model.ByteRange{Start: 30, End: 70},
				}}})
			_, _, err := h.svc.Record(context.Background(), model.ObservationRequest{
				SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: 1,
				Kind: model.ObservationScopeReview, Review: review,
				Note: "reviewed the callers of the partly served file",
			})
			if got := code(err); got != model.CodeCoverageIncomplete {
				t.Fatalf("Record over an unconfirmed citation: code %q (err %v), want %q",
					got, err, model.CodeCoverageIncomplete)
			}
		}},
		{"Record/a review claiming a file read without full coverage is refused", func(t *testing.T, h *harness) {
			partial := h.file(fixtureSession, filePartial)
			// [0,40) IS confirmed, so the citation guard passes and the refusal
			// can only come from the file's coverage state: a served interval is
			// not a read file.
			review := l3ScopeReview(fixtureID("manifest", "canonical"), 1, model.ReviewCompleteFilesRead,
				[]model.ClaimReference{{Source: &model.SourceCitation{
					FileID: filePartial, ContentHash: partial.hash,
					Bytes: model.ByteRange{Start: 0, End: 40},
				}}})
			_, _, err := h.svc.Record(context.Background(), model.ObservationRequest{
				SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: 1,
				Kind: model.ObservationScopeReview, Review: review,
				Note: "attested every file read",
			})
			if got := code(err); got != model.CodeCoverageIncomplete {
				t.Fatalf("Record over a partly served attested file: code %q (err %v), want %q",
					got, err, model.CodeCoverageIncomplete)
			}
		}},
		// L4 rows
		// Failure mode: a gate that folds a waiver into readiness grants false
		// write readiness -- an orchestrator would start writing files nobody
		// read. Everything §16.3 asks for is arranged here except the waiver,
		// so the waiver is the only precondition left unsatisfied and the two
		// readiness booleans must still be false. SessionStatus.Validate:514
		// refuses the same combination, so this row also proves the evaluator
		// never has to be caught by the model.
		{name: "readiness/a required-file waiver alone shuts the strict gate", run: func(t *testing.T, h *harness) {
			// Finish the partly served file and read the waived one too: a
			// file may be both waived and later read, and only then is
			// "everything else satisfied" literally true.
			h.file(fixtureSession, filePartial).served = []model.ByteRange{{Start: 0, End: 100}}
			h.file(fixtureSession, fileWaived).served = []model.ByteRange{{Start: 0, End: 50}}

			// A current, non-blocking scope review answering all eight
			// categories, written straight into the fake because Record is
			// another lane's surface and this row is about the gate, not about
			// how an observation is persisted.
			review := model.ScopeReview{
				ManifestHash: fixtureID("manifest", "canonical"),
				ScopeVersion: 1,
			}
			for _, c := range []model.ScopeReviewCategory{
				model.ReviewCompleteFilesRead, model.ReviewCallersConsumers,
				model.ReviewContractsTypes, model.ReviewStateLifecycle,
				model.ReviewDependencies, model.ReviewIntegrationPoints,
				model.ReviewSharedUtilities, model.ReviewRemainingUncertainty,
			} {
				review.Entries = append(review.Entries, model.ScopeReviewEntry{
					Category: c, Note: "read and accounted for in the fixture scope",
				})
			}
			if err := review.Validate(); err != nil {
				t.Fatalf("the fixture scope review is malformed: %v", err)
			}
			obsReq := model.ObservationRequest{
				SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: 1,
				Kind: model.ObservationScopeReview, Review: &review, Note: "scope reviewed",
			}
			fs := h.store.sessions[fixtureSession]
			fs.obs = append(fs.obs, model.Observation{
				ID: model.NewObservationID(obsReq), SessionID: fixtureSession, ActorID: fixtureActor,
				ScopeVersion: 1, Kind: model.ObservationScopeReview, Review: &review,
				Note: "scope reviewed", CreatedAt: fixtureNow,
			})

			st, err := h.svc.Status(context.Background(), model.SessionRequest{
				SessionID: fixtureSession, ActorID: fixtureActor,
			})
			if err != nil {
				t.Fatalf("Status: %v (code %q)", err, code(err))
			}
			if !st.ScopeComplete || !st.ReadCompleteForSnapshot || st.Superseded {
				t.Fatalf("the non-waiver preconditions are not all satisfied: scope_complete=%v read_complete=%v superseded=%v",
					st.ScopeComplete, st.ReadCompleteForSnapshot, st.Superseded)
			}
			if st.RequiredFiles != 4 || st.FullyServedFiles != 4 || st.WaivedFiles != 1 {
				t.Fatalf("counts are required=%d served=%d waived=%d, want 4/4/1",
					st.RequiredFiles, st.FullyServedFiles, st.WaivedFiles)
			}
			if st.StrictGateSatisfied || st.ReadyForImplementation {
				t.Fatalf("a waived required file granted strict readiness: strict=%v ready=%v",
					st.StrictGateSatisfied, st.ReadyForImplementation)
			}
		}},
		// L5 rows
		{"capsule identity excludes timestamps and a second completion returns the sealed capsule", capsuleIsDeterministic},
		// L6 rows
		// L7 rows
		// L8 rows
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t, newHarness(t))
		})
	}
}

// --- the fixture ------------------------------------------------------------

// fixtureNow is the one clock every scenario sees. Nothing in this package reads
// the wall clock, so a capsule or a status built twice is byte-identical.
var fixtureNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func fixtureID(parts ...string) string { return model.H("codectx.workflow.fixture.v1", parts...) }

// The fixture session: two required_full files (one fully served, one partly),
// one waived required file and one empty file, all pinned to one snapshot, plus
// a second actor with a session of its own so a cross-actor read is a real
// scenario rather than a hypothetical.
var (
	fixtureSession  = model.SessionID(fixtureID("session"))
	fixtureOther    = model.SessionID(fixtureID("session", "other"))
	fixtureManifest = model.ManifestID(fixtureID("manifest"))
	fixtureActor    = "actor-a"
	fixtureActorB   = "actor-b"

	fileFull    = model.FileID(fixtureID("file", "full"))    // required_full, fully served
	filePartial = model.FileID(fixtureID("file", "partial")) // required_full, partly served
	fileWaived  = model.FileID(fixtureID("file", "waived"))  // required_full, waived
	fileEmpty   = model.FileID(fixtureID("file", "empty"))   // required_full, zero length
)

// fakeFile is one pinned file with the confirmed intervals the store would hold
// in served_ranges. The slice is kept merged and ascending, exactly as the
// store's own merge leaves it, so containment here answers what SQL answers.
type fakeFile struct {
	id     model.FileID
	hash   string
	path   string
	size   int64
	served []model.ByteRange
	waived bool
}

// confirmed is the merged confirmed byte count, the number the store's coverage
// state switch derives full_served from.
func (f *fakeFile) confirmed() int64 {
	var n int64
	for _, r := range f.served {
		n += int64(r.End - r.Start)
	}
	return n
}

func (f *fakeFile) state() model.CoverageState {
	switch {
	case f.size == 0 && len(f.served) > 0:
		// A zero-length file is served by its confirmed EOF chunk; the store
		// keeps that as a separate branch because served_ranges cannot store a
		// zero-length interval.
		return model.CoverageFullServed
	case f.confirmed() == 0:
		return model.CoverageUnserved
	case f.confirmed() >= f.size:
		return model.CoverageFullServed
	}
	return model.CoveragePartialServed
}

// contains reports whether r lies wholly inside one confirmed interval. A range
// spanning the gap between two confirmed intervals is not contained, which is
// the distinction between containment and overlap that RangeConfirmed exists to
// make.
func (f *fakeFile) contains(r model.ByteRange) bool {
	if r.End <= r.Start {
		return f.size == 0 && len(f.served) > 0
	}
	for _, s := range f.served {
		if r.Start >= s.Start && r.End <= s.End {
			return true
		}
	}
	return false
}

type fakeSession struct {
	rec     sqlite.SessionRecord
	files   map[model.FileID]*fakeFile
	order   []model.FileID
	obs     []model.Observation
	waivers []model.WaiverRecord
	capsule *model.Capsule
}

// fakeStore is the in-package Sessions, Compiler and Validator. It reproduces
// the guarantees the real store gives and this package is built on top of --
// the actor check, the transition table, the state_version compare-and-swap,
// CTX_SCOPE_CHANGED on a stale observation, PutCapsule's write-once and
// CoverageSummary's counts -- and nothing more.
type fakeStore struct {
	mu        sync.Mutex
	sessions  map[model.SessionID]*fakeSession
	manifests map[model.ManifestID]model.ContextManifest
	entries   map[model.ManifestID][]model.ContextEntry
	// compiled is what Compile returns; a lane that exercises Include points it
	// at a manifest it also registered in manifests/entries.
	compiled model.ContextManifest
	// current is the Validator's answer: the content hash each file carries
	// right now. Changing one under a session is how a lane proves the gate is
	// revalidated per request rather than cached.
	current map[model.FileID]string
	// compileErr and validateErr let a lane drive the failure paths without a
	// second fake.
	compileErr, validateErr error
}

var (
	_ Sessions  = (*fakeStore)(nil)
	_ Compiler  = (*fakeStore)(nil)
	_ Validator = (*fakeStore)(nil)
)

// harness is what a scenario gets: the service under test and the fake behind
// it, so a row can arrange store state and then drive the service.
//
// This block is the lane-facing surface, and every helper and knob on it --
// session, file, code, fakeStore.compileErr, fakeStore.validateErr,
// fakeStore.current, fixtureOther -- is deliberately caller-less until rows land
// under the markers above. It is not dead code awaiting deletion.
//
// Two things a lane will look for and will not find here. Superseded has no
// source in the frozen Sessions -- no method yields the active generation -- so
// L4 derives it from Validator.Current: a required file whose pinned hash is no
// longer current means the session reads a superseded snapshot. The knob is
// h.store.current[fileFull] = "<another hash>". And the capsule size bound is
// the service's, not the fake's: L5 checks Limits.MaxCapsuleBytes before the
// write, so the "capsule over max_capsule_bytes fails explicitly" row drives the
// service and never reaches PutCapsule.
type harness struct {
	t     *testing.T
	store *fakeStore
	svc   *Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := newFakeStore()
	svc, err := New(Options{
		Sessions: store,
		Compile:  store,
		Validate: store,
		Limits: Limits{
			MaxPageItems:             model.MaxPageItems,
			MaxObservationReferences: model.MaxObservationReferences,
			MaxCapsuleBytes:          8 << 20,
			QueryTimeout:             10 * time.Second,
		},
		Now:    func() time.Time { return fixtureNow },
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("build the workflow service: %v", err)
	}
	return &harness{t: t, store: store, svc: svc}
}

// session is the fixture session record, for a row that needs to call an
// unexported helper directly rather than through an exported operation.
func (h *harness) session(id model.SessionID) sqlite.SessionRecord {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	fs, ok := h.store.sessions[id]
	if !ok {
		h.t.Fatalf("no fixture session %q", id)
	}
	return fs.rec
}

// file reaches one fixture file so a row can change its served ranges, waive it
// or move its current hash out from under the session.
func (h *harness) file(id model.SessionID, file model.FileID) *fakeFile {
	h.t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	fs, ok := h.store.sessions[id]
	if !ok {
		h.t.Fatalf("no fixture session %q", id)
	}
	f, ok := fs.files[file]
	if !ok {
		h.t.Fatalf("session %q pins no file %q", id, file)
	}
	return f
}

// code is the Section 20.3 code on err, or "" when err carries none. Every row
// asserts on the code rather than on message text, which is presentation.
func code(err error) string {
	var e *model.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func newFakeStore() *fakeStore {
	binding := model.Binding{
		RepositoryID: model.RepositoryID(fixtureID("repo")),
		SnapshotID:   model.SnapshotID(fixtureID("snapshot")),
		GenerationID: 1,
		AnalysisKey:  model.AnalysisKey(fixtureID("analysis")),
	}
	manifest := model.ContextManifest{
		ID:             fixtureManifest,
		Binding:        binding,
		Phase:          model.PhaseVerify,
		RequestHash:    fixtureID("request"),
		PolicyVersion:  "1",
		CanonicalHash:  fixtureID("manifest", "canonical"),
		Budget:         model.Budget{MaxEstimatedTokens: 100000, MaxBytes: 1 << 20, MaxFiles: 64, MaxSlices: 64},
		EntryCount:     4,
		ScopeComplete:  true,
		EstimateMethod: "bytes",
		CreatedAt:      fixtureNow,
	}
	files := []*fakeFile{
		{id: fileFull, hash: fixtureID("hash", "full"), path: "internal/a/full.go", size: 100,
			served: []model.ByteRange{{Start: 0, End: 100}}},
		// Two disjoint intervals with a deliberate gap at [40,60): a citation
		// spanning the gap is confirmed by neither and must be refused.
		{id: filePartial, hash: fixtureID("hash", "partial"), path: "internal/a/partial.go", size: 100,
			served: []model.ByteRange{{Start: 0, End: 40}, {Start: 60, End: 100}}},
		{id: fileWaived, hash: fixtureID("hash", "waived"), path: "internal/b/waived.go", size: 50},
		{id: fileEmpty, hash: fixtureID("hash", "empty"), path: "internal/b/empty.go", size: 0,
			served: []model.ByteRange{{Start: 0, End: 0}}},
	}
	s := &fakeStore{
		sessions:  map[model.SessionID]*fakeSession{},
		manifests: map[model.ManifestID]model.ContextManifest{fixtureManifest: manifest},
		entries:   map[model.ManifestID][]model.ContextEntry{},
		compiled:  manifest,
		current:   map[model.FileID]string{},
	}
	for i, f := range files {
		s.entries[fixtureManifest] = append(s.entries[fixtureManifest], model.ContextEntry{
			Ordinal: i, FileID: f.id, Requirement: model.RequirementFull,
			ScoreMicros: int64(1000 - i), EstimatedBytes: f.size, EstimatedTokens: f.size / 4,
			Reasons: []string{"seed"},
		})
		s.current[f.id] = f.hash
	}
	for _, spec := range []struct {
		id    model.SessionID
		actor string
	}{{fixtureSession, fixtureActor}, {fixtureOther, fixtureActorB}} {
		fs := &fakeSession{
			rec: sqlite.SessionRecord{
				ID: spec.id, ActorID: spec.actor, Binding: binding, ManifestID: fixtureManifest,
				Phase: model.PhaseVerify, State: model.StateVerifyOpen,
				StateVersion: 1, ScopeVersion: 1,
				CreatedAt: fixtureNow, ExpiresAt: fixtureNow.Add(24 * time.Hour),
			},
			files: map[model.FileID]*fakeFile{},
		}
		for _, f := range files {
			c := *f
			c.served = append([]model.ByteRange(nil), f.served...)
			fs.files[f.id] = &c
			fs.order = append(fs.order, f.id)
		}
		s.sessions[spec.id] = fs
	}
	// The waived file carries its waiver from the start, so the readiness rows
	// exercise "everything else satisfied, one waiver" without arranging it.
	owner := s.sessions[fixtureSession]
	owner.files[fileWaived].waived = true
	owner.waivers = append(owner.waivers, model.WaiverRecord{
		SessionID: fixtureSession, ActorID: fixtureActor, FileID: fileWaived,
		ContentHash: owner.files[fileWaived].hash, Reason: "vendored generated code",
		CreatedAt: fixtureNow,
	})
	return s
}

// --- Sessions ---------------------------------------------------------------

// lookup applies the store's own actor and lifecycle rules. Like the real
// Session it skips the actor check when actor is empty, so a caller that did not
// Validate() its request first gets the same unguarded read here as in
// production rather than a fake that is stricter than the thing it stands for.
func (s *fakeStore) lookup(id model.SessionID, actor string) (*fakeSession, error) {
	fs, ok := s.sessions[id]
	if !ok {
		return nil, &model.Error{Code: model.CodeArgumentInvalid, Message: "no such session"}
	}
	if actor != "" && actor != fs.rec.ActorID {
		return nil, &model.Error{Code: model.CodeActorMismatch, Message: "this session belongs to another actor"}
	}
	if fs.rec.ExpiresAt.Before(fixtureNow) {
		// Expired reads report honestly beside the record rather than
		// swallowing it; every caller must use errors.As and not treat this as
		// "no record".
		return fs, &model.Error{Code: model.CodeSessionExpired, Message: "this session has expired"}
	}
	return fs, nil
}

func (s *fakeStore) Session(_ context.Context, id model.SessionID, actor string) (sqlite.SessionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(id, actor)
	if fs == nil {
		return sqlite.SessionRecord{}, err
	}
	return fs.rec, err
}

// allowed is the store's transition graph, reproduced here only so the fake can
// refuse what the store refuses. A scenario that asserts on this table rather
// than on a service guard is re-asserting Task 5 and is a defect.
var allowed = map[model.WorkflowState][]model.WorkflowState{
	model.StateSweepOpen:       {model.StateVerifyOpen, model.StateClosed},
	model.StateVerifyOpen:      {model.StateConsolidateOpen, model.StateClosed},
	model.StateConsolidateOpen: {model.StateComplete, model.StateClosed},
}

func (s *fakeStore) AdvanceSession(_ context.Context, req model.AdvanceRequest) (model.WorkflowStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(req.SessionID, req.ActorID)
	if err != nil {
		return model.WorkflowStatus{}, err
	}
	// The compare-and-swap is on state_version alone and happens inside the
	// store's transaction, so a service that pre-reads and then writes loses a
	// race the store itself would have caught.
	if fs.rec.StateVersion != req.ExpectedVersion {
		return model.WorkflowStatus{}, &model.Error{Code: model.CodeVersionConflict,
			Message: "the session advanced under this request"}
	}
	ok := false
	for _, next := range allowed[fs.rec.State] {
		if next == req.Target {
			ok = true
			break
		}
	}
	if !ok {
		return model.WorkflowStatus{}, &model.Error{Code: model.CodeVersionConflict,
			Message: "that transition is not reachable from this state"}
	}
	fs.rec.State = req.Target
	fs.rec.StateVersion++
	if req.Target == model.StateClosed {
		closed := fixtureNow
		fs.rec.ClosedAt = &closed
	}
	return s.statusOf(fs), nil
}

func (s *fakeStore) statusOf(fs *fakeSession) model.WorkflowStatus {
	return model.WorkflowStatus{
		SessionID: fs.rec.ID, State: fs.rec.State, StateVersion: fs.rec.StateVersion,
		ScopeVersion: fs.rec.ScopeVersion, ManifestID: fs.rec.ManifestID, Phase: fs.rec.Phase,
	}
}

func (s *fakeStore) IncludeManifest(_ context.Context, req model.IncludeRequest, manifest model.ManifestID) (model.WorkflowStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(req.SessionID, req.ActorID)
	if err != nil {
		return model.WorkflowStatus{}, err
	}
	if fs.rec.StateVersion != req.ExpectedVersion {
		return model.WorkflowStatus{}, &model.Error{Code: model.CodeVersionConflict,
			Message: "the session advanced under this request"}
	}
	m, ok := s.manifests[manifest]
	if !ok {
		return model.WorkflowStatus{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "no such manifest"}
	}
	if m.Binding != fs.rec.Binding {
		return model.WorkflowStatus{}, &model.Error{Code: model.CodeSessionSuperseded,
			Message: "the manifest is bound to another generation"}
	}
	// Both versions move and the file derivation is INSERT OR IGNORE, so
	// same-actor same-hash coverage survives an include untouched. Nothing is
	// deleted here, and nothing about the observations is rewritten.
	fs.rec.ManifestID = manifest
	fs.rec.ScopeVersion++
	fs.rec.StateVersion++
	for _, e := range s.entries[manifest] {
		if e.FileID == "" {
			continue
		}
		if _, exists := fs.files[e.FileID]; exists {
			continue
		}
		fs.files[e.FileID] = &fakeFile{id: e.FileID, hash: s.current[e.FileID],
			path: "included/" + string(e.FileID)[:8] + ".go", size: e.EstimatedBytes}
		fs.order = append(fs.order, e.FileID)
	}
	return s.statusOf(fs), nil
}

func (s *fakeStore) Waive(_ context.Context, req model.WaiverRequest) (model.WaiverRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(req.SessionID, req.ActorID)
	if err != nil {
		return model.WaiverRecord{}, err
	}
	f, ok := fs.files[req.FileID]
	if !ok {
		return model.WaiverRecord{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "that file is not in this session's scope"}
	}
	for _, w := range fs.waivers {
		if w.FileID == req.FileID {
			return w, nil
		}
	}
	// A waiver records the exception; it never fabricates coverage, so the
	// file's served ranges are untouched.
	w := model.WaiverRecord{SessionID: fs.rec.ID, ActorID: fs.rec.ActorID, FileID: req.FileID,
		ContentHash: f.hash, Reason: req.Reason, CreatedAt: fixtureNow}
	f.waived = true
	fs.waivers = append(fs.waivers, w)
	return w, nil
}

func (s *fakeStore) PutObservation(_ context.Context, o model.Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(o.SessionID, o.ActorID)
	if err != nil {
		return err
	}
	if o.ScopeVersion != fs.rec.ScopeVersion {
		return &model.Error{Code: model.CodeScopeChanged,
			Message: "the session's scope moved under this observation"}
	}
	for _, existing := range fs.obs {
		if existing.ID == o.ID {
			// INSERT OR IGNORE on the content-derived id: an existing
			// observation is never rewritten.
			return nil
		}
	}
	fs.obs = append(fs.obs, o)
	return nil
}

func (s *fakeStore) Observations(_ context.Context, session model.SessionID, actor string,
	kind model.ObservationKind, scopeVersion int, after model.ObservationID, limit int) ([]model.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if fs == nil {
		return nil, err
	}
	if limit <= 0 || limit > model.MaxPageItems {
		limit = model.MaxPageItems
	}
	out := make([]model.Observation, 0, limit)
	sorted := append([]model.Observation(nil), fs.obs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	for _, o := range sorted {
		if kind != "" && o.Kind != kind {
			continue
		}
		if scopeVersion > 0 && o.ScopeVersion != scopeVersion {
			continue
		}
		if after != "" && o.ID <= after {
			continue
		}
		out = append(out, o)
		if len(out) == limit {
			break
		}
	}
	return out, err
}

func (s *fakeStore) PutCapsule(_ context.Context, c model.Capsule) (model.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(c.SessionID, c.ActorID)
	if err != nil {
		return model.Capsule{}, err
	}
	if fs.capsule != nil {
		// session_id is the primary key: the first capsule is the capsule, and
		// it comes back unchanged with its original timestamp.
		return *fs.capsule, nil
	}
	if fs.rec.State != model.StateConsolidateOpen {
		return model.Capsule{}, &model.Error{Code: model.CodeVersionConflict,
			Message: "a capsule is sealed only while consolidate is open"}
	}
	if c.Binding != fs.rec.Binding {
		return model.Capsule{}, &model.Error{Code: model.CodeSessionSuperseded,
			Message: "the capsule is bound to another generation"}
	}
	if err := c.Validate(); err != nil {
		return model.Capsule{}, err
	}
	stored := c
	fs.capsule = &stored
	return stored, nil
}

func (s *fakeStore) Capsule(_ context.Context, session model.SessionID, actor string) (model.Capsule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if fs == nil {
		return model.Capsule{}, err
	}
	if fs.capsule == nil {
		return model.Capsule{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "this session has sealed no capsule"}
	}
	return *fs.capsule, err
}

func (s *fakeStore) Coverage(_ context.Context, session model.SessionID, actor string,
	after model.FileID, limit int) ([]model.FileCoverage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if fs == nil {
		return nil, err
	}
	if limit <= 0 || limit > model.MaxPageItems {
		limit = model.MaxPageItems
	}
	ids := append([]model.FileID(nil), fs.order...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]model.FileCoverage, 0, limit)
	for _, id := range ids {
		if after != "" && id <= after {
			continue
		}
		f := fs.files[id]
		out = append(out, model.FileCoverage{
			FileID: f.id, ContentHash: f.hash, Size: f.size, ConfirmedBytes: f.confirmed(),
			Requirement: model.RequirementFull, State: f.state(), Waived: f.waived,
		})
		if len(out) == limit {
			break
		}
	}
	return out, err
}

func (s *fakeStore) CoverageSummary(_ context.Context, session model.SessionID, actor string) (int64, int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if fs == nil {
		return 0, 0, 0, err
	}
	var required, served, waived int64
	for _, f := range fs.files {
		required++
		if f.state() == model.CoverageFullServed {
			served++
		}
		if f.waived {
			waived++
		}
	}
	return required, served, waived, err
}

func (s *fakeStore) Manifest(_ context.Context, id model.ManifestID) (model.ContextManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.manifests[id]
	if !ok {
		return model.ContextManifest{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "no such manifest"}
	}
	return m, nil
}

func (s *fakeStore) ManifestEntries(_ context.Context, id model.ManifestID, afterOrdinal, limit int) ([]model.ContextEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > model.MaxPageItems {
		limit = model.MaxPageItems
	}
	out := make([]model.ContextEntry, 0, limit)
	for _, e := range s.entries[id] {
		if e.Ordinal <= afterOrdinal {
			continue
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeStore) RangeConfirmed(_ context.Context, session model.SessionID, actor string,
	file model.FileID, hash string, r model.ByteRange) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if err != nil {
		return false, err
	}
	f, ok := fs.files[file]
	if !ok || f.hash != hash {
		// A file outside the pinned scope, or one at a hash this session never
		// pinned, is not confirmed -- it is not an error, because a citation
		// over it is a claim to refuse, not a storage failure.
		return false, nil
	}
	return f.contains(r), nil
}

func (s *fakeStore) SessionFilePaths(_ context.Context, session model.SessionID, actor string,
	ids []model.FileID) (map[model.FileID]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fs, err := s.lookup(session, actor)
	if err != nil {
		return nil, err
	}
	if len(ids) > model.MaxPageItems {
		return nil, &model.Error{Code: model.CodeResourceLimit,
			Message: "a path batch of " + strconv.Itoa(len(ids)) + " exceeds the page bound"}
	}
	out := make(map[model.FileID]string, len(ids))
	for _, id := range ids {
		if f, ok := fs.files[id]; ok {
			out[id] = f.path
		}
	}
	return out, nil
}

// --- Compiler and Validator -------------------------------------------------

func (s *fakeStore) Compile(_ context.Context, _ model.ContextRequest) (model.ContextManifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.compileErr != nil {
		return model.ContextManifest{}, s.compileErr
	}
	return s.compiled, nil
}

func (s *fakeStore) Current(_ context.Context, file model.FileID, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.validateErr != nil {
		return false, s.validateErr
	}
	return s.current[file] == hash, nil
}

// --- L3 helpers -------------------------------------------------------------

// l3ScopeReview builds an attestation that answers all eight required
// categories, so a row exercises the guard it names rather than
// ScopeReview.Validate's completeness check. Only the target category carries
// references; every other category is answered with an explicit note, which is
// what the model requires of a category with no references.
func l3ScopeReview(manifestHash string, scopeVersion int, target model.ScopeReviewCategory,
	refs []model.ClaimReference) *model.ScopeReview {
	categories := []model.ScopeReviewCategory{
		model.ReviewCompleteFilesRead, model.ReviewCallersConsumers, model.ReviewContractsTypes,
		model.ReviewStateLifecycle, model.ReviewDependencies, model.ReviewIntegrationPoints,
		model.ReviewSharedUtilities, model.ReviewRemainingUncertainty,
	}
	review := &model.ScopeReview{ManifestHash: manifestHash, ScopeVersion: scopeVersion}
	for _, c := range categories {
		entry := model.ScopeReviewEntry{Category: c, Note: "answered for " + string(c)}
		if c == target {
			entry.References = refs
		}
		review.Entries = append(review.Entries, entry)
	}
	return review
}

// --- L5 scenario ------------------------------------------------------------

// capsuleIsDeterministic protects the Section 17.3 identity rule and ruling Q11
// together, because the same seal answers both: the canonical hash is derived
// from the capsule's semantics and never from when it was built, and a repeated
// completion returns the FIRST stored capsule rather than recomputing one and
// writing over the sealed identity.
//
// A silent breakage here is severe in a way a wrong error code is not: two
// honest completions of one session would disagree about what was sealed, and
// the durable artifact a later session replays would carry whichever timestamp
// happened to run last.
func capsuleIsDeterministic(t *testing.T, h *harness) {
	ctx := context.Background()
	if _, err := h.store.AdvanceSession(ctx, model.AdvanceRequest{
		SessionID: fixtureSession, ActorID: fixtureActor,
		Target: model.StateConsolidateOpen, ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("open consolidate on the fixture session: %v", err)
	}
	// The fixture waives one required file, and the frozen Sessions interface
	// exposes no reader for the stored waiver reasons (see capsuleWaivers), so
	// this row seals a session with no waiver outstanding. The waiver's own
	// invariant is L4's row, not this one.
	h.file(fixtureSession, fileWaived).waived = false

	rec := h.session(fixtureSession)
	g := gate{ReadComplete: true, Ready: true, Strict: true, ScopeComplete: true}

	first, err := h.svc.buildCapsule(ctx, rec, g)
	if err != nil {
		t.Fatalf("seal the first capsule: %v", err)
	}
	if !first.CreatedAt.Equal(fixtureNow) {
		t.Fatalf("the sealed capsule carries %s, not the clock it was sealed under (%s)", first.CreatedAt, fixtureNow)
	}

	// The preimage itself: the same capsule stamped three days later must hash
	// to the same identity, or the digest is a function of the wall clock.
	later := fixtureNow.Add(72 * time.Hour)
	restamped := first
	restamped.CreatedAt = later
	if got := canonicalCapsuleHash(restamped); got != first.CanonicalHash {
		t.Fatalf("the canonical hash moved with CreatedAt: %s before, %s after", first.CanonicalHash, got)
	}

	// A second completion under a later clock: the seal must answer with the
	// stored capsule and its original timestamp, never a freshly built one.
	svc, err := New(Options{
		Sessions: h.store, Compile: h.store, Validate: h.store, Limits: h.svc.limits,
		Now: func() time.Time { return later }, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("build a second service on a later clock: %v", err)
	}
	second, err := svc.buildCapsule(ctx, rec, g)
	if err != nil {
		t.Fatalf("seal the capsule a second time: %v", err)
	}
	if second.CanonicalHash != first.CanonicalHash {
		t.Fatalf("a second completion sealed a different identity: %s then %s", first.CanonicalHash, second.CanonicalHash)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("a second completion returned %s, not the first capsule's %s", second.CreatedAt, first.CreatedAt)
	}
}
