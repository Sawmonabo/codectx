package context

import (
	stdcontext "context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// pagedCompiler is intCompiler with continuations composed: the spool store the
// checkpoint is adopted into, the signer that mints the token and the lease
// store that owns the retention. It is a separate constructor rather than a
// flag on intCompiler so every other row keeps proving that a compiler composed
// WITHOUT them behaves exactly as it did before ruling C7.
// release, when non-nil, replaces the graph engine's release. frontHalf defers
// it, so it runs exactly at the P-F/P-G boundary: that is the seam a row uses
// to make the deadline fire THERE rather than in the middle of a pass.
func pagedCompiler(t *testing.T, fx *contextFixture, now func() time.Time, release func() error) (*Compiler, *pagination.Spools) {
	t.Helper()
	dir := t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"), 1<<26, fx.Store)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	// ONE engine for every leg of a continued compile, not one per call: a
	// graph walk's own cursor is signed by the engine that minted it, and
	// scopeEngine opens a fresh signer in a fresh directory each time it is
	// called, so a per-call engine refuses the walk cursor a mid-walk
	// continuation carries. fx.Rels, as intCompiler wires it: an adjacency with
	// no edges walks nothing, and a parity row over it would compare two plans
	// that never ran a walk.
	eng := fx.scopeEngine(fx.Rels, fixtureCapabilities)
	c, err := New(Options{
		Store:  fx.Store,
		Repo:   fx.Repo,
		Search: searchService(t, fx),
		Graph: func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
			if release == nil {
				release = func() error { return nil }
			}
			return eng, release, nil
		},
		Config: fx.Cfg,
		Now:    now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// SortDir is deliberately NOT set: the compiler derives it from the
		// spool store, which is the wiring compose.go uses.
		Spools: spools,
		Signer: signer,
		Leases: pagination.NewLeases(fx.Store, fx.Cfg.Storage.QueryCursorTTL.Std()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, spools
}

// A compile interrupted at the P-F/P-G boundary and continued from its cursor
// must produce the SAME plan as one that was never interrupted -- not a plan
// with the same entries, the same canonical projection, which is what every
// later consumer reads and what a resumed compile could silently diverge on:
// pagination.AdoptRuns re-applies a comparator and a fold and "merges without
// error into an answer that is not sorted" when either is wrong.
//
// Two independent stores, deliberately. Compile consults reuseManifest before
// any pass, so interrupting and resuming inside ONE store would have the
// resumed call return the manifest the reference compile stored and assert
// nothing at all.
func TestAResumedCompileProducesTheUninterruptedPlan(t *testing.T) {
	t.Parallel()
	req := model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}
	tick := 0
	now := func() time.Time {
		tick++
		return fixtureNow().Add(time.Duration(tick) * time.Second)
	}

	// The reference: one uninterrupted compile in its own store.
	ref := newContextFixture(t)
	want, err := intCompiler(t, ref, now).Compile(ref.ctx, req)
	if err != nil {
		t.Fatalf("the uninterrupted compile failed: %v", err)
	}
	if want.EntryCount == 0 {
		t.Fatalf("the reference plan selected nothing; the row would assert on an empty plan")
	}

	// The interrupted one: the front half runs, the boundary checkpoints, and
	// the token is what a deadline at that boundary would have answered.
	fx := newContextFixture(t)
	c, spools := pagedCompiler(t, fx, now, nil)
	token := checkpointAtBoundary(t, c, fx, req, passMeasure)

	got, err := c.CompilePage(fx.ctx, req, token)
	if err != nil {
		t.Fatalf("resuming from the continuation cursor failed: %v", err)
	}
	if got.Truncated || got.NextCursor != "" {
		t.Fatalf("the resumed call reported truncated=%v cursor=%q, want the finished plan", got.Truncated, got.NextCursor)
	}
	if got.Manifest.CanonicalHash != want.CanonicalHash || got.Manifest.ID != want.ID {
		t.Fatalf("the resumed plan is %s/%s, the uninterrupted one %s/%s: a continuation must not change the plan",
			got.Manifest.ID, got.Manifest.CanonicalHash, want.ID, want.CanonicalHash)
	}
	if got.Manifest.EntryCount != want.EntryCount || got.Manifest.SliceCount != want.SliceCount ||
		got.Manifest.ScopeComplete != want.ScopeComplete {
		t.Fatalf("resumed header %+v, want %+v", got.Manifest, want)
	}
	if len(got.Manifest.Notices) != len(want.Notices) {
		t.Fatalf("the resumed plan carries %d notice(s), the uninterrupted one %d", len(got.Manifest.Notices), len(want.Notices))
	}
	for i := range got.Manifest.Notices {
		if got.Manifest.Notices[i] != want.Notices[i] {
			t.Fatalf("notice %d = %q, want %q", i, got.Manifest.Notices[i], want.Notices[i])
		}
	}

	// Leak check: the consumed continuation's state directory is released, so
	// the store holds no live bytes and no spool entry once the plan is done.
	live, err := spools.Sweep(fx.ctx, now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if live != 0 {
		t.Fatalf("the spool store still holds %d byte(s) after the continuation was consumed, want 0", live)
	}
}

// A cursor is bound to the request that minted it. Presenting it to a different
// request must be refused: the checkpoint holds the ranking of ONE request's
// candidates, and resuming another request's budget half over it would produce
// a plan for a task nobody asked for, with nothing reporting the splice.
func TestAContinuationRefusesAnotherRequest(t *testing.T) {
	t.Parallel()
	fx := newContextFixture(t)
	c, _ := pagedCompiler(t, fx, fx.Now, nil)
	token := checkpointAtBoundary(t, c, fx,
		model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}, passMeasure)

	_, err := c.CompilePage(fx.ctx, model.ContextRequest{Task: "something else entirely", Phase: model.PhaseVerify}, token)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCursorInvalid {
		t.Fatalf("resuming another request answered %v, want CTX_CURSOR_INVALID", err)
	}
}

// checkpointAtBoundary advances a fresh compile to the boundary behind pass
// `at` and returns the continuation token that boundary mints.
//
// It drives the stop predicate directly rather than forcing a real deadline:
// the deadline is wall-clock and firing it inside a fixture compile would stop
// a pass rather than land on a NAMED boundary. Both predicates drive the
// identical checkpoint -- runPasses takes the stop as a parameter for exactly
// that reason -- and the row below this one executes the deadline predicate
// itself, so the production path is not assumed anywhere.
func checkpointAtBoundary(t *testing.T, c *Compiler, fx *contextFixture, req model.ContextRequest, at int) string {
	t.Helper()
	token, _ := checkpointState2(t, c, fx, req, at)
	return token
}

// checkpointState2 is checkpointAtBoundary with the halted state handed back,
// for the one row that asserts WHERE inside a pass the halt landed.
func checkpointState2(t *testing.T, c *Compiler, fx *contextFixture, req model.ContextRequest, at int) (string, *compileState) {
	t.Helper()
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, 0, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	binding := reader.Binding()
	id, _ := manifestIdentity(binding, req, c.cfg)
	if !model.ValidHexID(string(id)) {
		t.Fatalf("the manifest identity %q is not a well-formed id; the cursor payload would refuse every token it mints", id)
	}
	st := &compileState{pass: passIngest}
	sorts, err := newCompileSorts(c.cfg, c.sortDir)
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	defer sorts.Close()
	resolved, err := resolveBudget(req.Budget, c.cfg.Context)
	if err != nil {
		t.Fatalf("resolveBudget: %v", err)
	}
	if err := c.runPasses(fx.ctx, reader, binding.GenerationID, req, sorts, resolved, st,
		func(pass int) bool { return pass == at }); err != nil {
		t.Fatalf("the passes before boundary %d failed: %v", at, err)
	}
	if !st.halted || st.pass != at {
		t.Fatalf("the pipeline ran to pass %d (halted=%v), want a halt at boundary %d", st.pass, st.halted, at)
	}
	next, err := c.checkpointAt(fx.ctx, binding, string(id), st)
	if err != nil {
		t.Fatalf("checkpointAt pass %d: %v", at, err)
	}
	if next == "" {
		t.Fatalf("boundary %d minted no continuation token", at)
	}
	return next, st
}

// EVERY pass boundary is a checkpoint, and a compile stopped at any of them
// must finish into the plan an uninterrupted compile produces -- not a plan
// with the same entry count, the same canonical projection.
// model.ContextManifest.CanonicalHash IS the fold of that projection's
// canonical JSON, so equal hashes over an equal header is byte-identity of the
// plan and not a weaker claim about it.
//
// Two independent stores per boundary, deliberately. Compile consults
// reuseManifest before any pass, so interrupting and resuming inside ONE store
// would have the resumed call return the manifest an earlier row stored and
// assert nothing at all.
func TestEveryPassBoundaryResumesIntoTheUninterruptedPlan(t *testing.T) {
	t.Parallel()
	req := model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}
	ref := newContextFixture(t)
	want, err := intCompiler(t, ref, ref.Now).Compile(ref.ctx, req)
	if err != nil {
		t.Fatalf("the uninterrupted compile failed: %v", err)
	}
	if want.EntryCount == 0 {
		t.Fatalf("the reference plan selected nothing; every row below would assert on an empty plan")
	}
	for at := passHydrate; at <= passEmit; at++ {
		fx := newContextFixture(t)
		c, spools := pagedCompiler(t, fx, fx.Now, nil)
		token := checkpointAtBoundary(t, c, fx, req, at)
		got, err := c.CompilePage(fx.ctx, req, token)
		if err != nil {
			t.Fatalf("resuming at pass %d failed: %v", at, err)
		}
		assertSamePlan(t, at, got, want)
		// Leak check: the consumed continuation's state directory is released,
		// so the store holds no live bytes once the plan is done.
		live, err := spools.Sweep(fx.ctx, fx.Now())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if live != 0 {
			t.Fatalf("boundary %d left %d live byte(s) in the spool store, want 0", at, live)
		}
	}
}

// assertSamePlan is the parity assertion every continuation row makes: the
// resumed call finished, and the plan it finished into is the uninterrupted
// one.
func assertSamePlan(t *testing.T, at int, got CompileResult, want model.ContextManifest) {
	t.Helper()
	if got.Truncated || got.NextCursor != "" {
		t.Fatalf("boundary %d: the resumed call reported truncated=%v cursor=%q, want the finished plan",
			at, got.Truncated, got.NextCursor)
	}
	if got.Manifest.CanonicalHash != want.CanonicalHash || got.Manifest.ID != want.ID {
		t.Fatalf("boundary %d: the resumed plan is %s/%s, the uninterrupted one %s/%s: a continuation must not change the plan",
			at, got.Manifest.ID, got.Manifest.CanonicalHash, want.ID, want.CanonicalHash)
	}
	if got.Manifest.EntryCount != want.EntryCount || got.Manifest.SliceCount != want.SliceCount ||
		got.Manifest.ScopeComplete != want.ScopeComplete {
		t.Fatalf("boundary %d: resumed header %+v, want %+v", at, got.Manifest, want)
	}
	if len(got.Manifest.Notices) != len(want.Notices) {
		t.Fatalf("boundary %d: the resumed plan carries %d notice(s), the uninterrupted one %d",
			at, len(got.Manifest.Notices), len(want.Notices))
	}
	for i := range got.Manifest.Notices {
		if got.Manifest.Notices[i] != want.Notices[i] {
			t.Fatalf("boundary %d: notice %d = %q, want %q", at, i, got.Manifest.Notices[i], want.Notices[i])
		}
	}
}

// boundaryClock is the compiler's clock with a halt: it reads as the ordinary
// clock until halt() is called and one query timeout later afterwards.
//
// Its base is the real clock, deliberately. compile bounds itself with
// context.WithTimeout, whose deadline is a real instant, so a clock based
// anywhere else could never be compared with it; jumping by MORE than
// resources.query_timeout (10s) and far LESS than storage.query_cursor_ttl
// (15m) is what makes "the deadline has passed, the cursor has not" the state
// at the boundary, deterministically and without any sleep.
type boundaryClock struct {
	mu     sync.Mutex
	base   time.Time
	tick   int
	halted bool
}

func newBoundaryClock() *boundaryClock { return &boundaryClock{base: time.Now().UTC()} }

func (b *boundaryClock) now() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tick++
	t := b.base.Add(time.Duration(b.tick) * time.Millisecond)
	if b.halted {
		t = t.Add(time.Minute)
	}
	return t
}

func (b *boundaryClock) halt() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halted = true
	return nil
}

// The deadline branch itself, executed at EVERY boundary in sequence by the
// production predicate. The clock halts when the graph engine is released,
// which is the end of P-A, and stays halted: the deadline has therefore passed
// at every boundary behind it, so each call advances exactly ONE pass and
// answers a continuation, and the last one returns the plan.
//
// Every one of those calls must answer truncated=deadline with a resumable
// cursor, an EMPTY manifest, and -- the invariant Section 14.4 puts above every
// other -- nothing written to the store. A partial plan that reached
// persistence would be indistinguishable to every later reader from a complete
// one, because a manifest header carries no "this is a fragment" bit.
//
// This is the row C-D3 could not write: it drove the checkpoint by calling it,
// so compile's own deadline branch never ran and "the deadline path persists no
// manifest" was an argument about the code rather than a fact about it.
func TestTheDeadlineBranchWalksEveryBoundaryAndStoresNoManifest(t *testing.T) {
	t.Parallel()
	req := model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}
	ref := newContextFixture(t)
	want, err := intCompiler(t, ref, ref.Now).Compile(ref.ctx, req)
	if err != nil {
		t.Fatalf("the uninterrupted compile failed: %v", err)
	}

	fx := newContextFixture(t)
	clock := newBoundaryClock()
	c, spools := pagedCompiler(t, fx, clock.now, clock.halt)
	// The identity is computable without compiling, which is what lets each
	// truncated call assert on the STORE rather than on the returned value.
	id := requestManifestID(t, c, fx, req)

	token := ""
	// One call per boundary plus the call that finishes P-I. A compile that
	// stopped making progress would spin here rather than fail, so the bound
	// is what turns that into a failure.
	stops := 0
	for range passEmit + 1 {
		res, err := c.CompilePage(fx.ctx, req, token)
		if err != nil {
			t.Fatalf("a compile that ran out of deadline at a boundary failed instead of continuing: %v", err)
		}
		if !res.Truncated {
			assertSamePlan(t, passEmit, res, want)
			if res.Manifest.EntryCount == 0 {
				t.Fatalf("the continuation selected nothing; the assertions above would have passed over an empty plan")
			}
			token = ""
			break
		}
		stops++
		if res.TruncationReason != truncationDeadline || res.NextCursor == "" {
			t.Fatalf("a deadline boundary answered reason=%q cursor=%q, want a deadline continuation",
				res.TruncationReason, res.NextCursor)
		}
		if res.Manifest.ID != "" || res.Manifest.EntryCount != 0 {
			t.Fatalf("a truncated compile answered manifest %+v, want the zero header", res.Manifest)
		}
		if _, stored, err := c.reuseManifest(fx.ctx, id); err != nil {
			t.Fatalf("reuseManifest: %v", err)
		} else if stored {
			t.Fatalf("the deadline path persisted manifest %s; a partial plan must never reach the store", id)
		}
		token = res.NextCursor
	}
	if token != "" {
		t.Fatalf("the compile never finished: it is still answering a continuation after %d stops", stops)
	}
	// P-A..P-H each end a call, so the deadline stopped this compile at every
	// boundary there is. A smaller count would mean a boundary fell through.
	if want := passEmit - passIngest; stops != want {
		t.Fatalf("the deadline stopped the compile %d time(s), want one per boundary (%d)", stops, want)
	}
	live, err := spools.Sweep(fx.ctx, clock.now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if live != 0 {
		t.Fatalf("the spool store still holds %d byte(s) after the continuation was consumed, want 0", live)
	}
}

// requestManifestID is the identity a request compiles to under this compiler's
// pinned generation and configuration -- the same value compile computes before
// its first pass.
func requestManifestID(t *testing.T, c *Compiler, fx *contextFixture, req model.ContextRequest) model.ManifestID {
	t.Helper()
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, 0, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	id, _ := manifestIdentity(reader.Binding(), req, c.cfg)
	return id
}

// The P-D boundary carries `edges` PRE-fold -- P-D has finished writing it and
// P-E has not merged it -- so the restore must re-attach foldPkgEdgeDistinct.
// A spill does NOT fold (the fold runs exactly once, over the fully ordered
// stream), so the detached runs still hold every duplicate arrival and a
// restore without the fold hands P-E one record per route that reached a
// package instead of one per DISTINCT edge: every centrality boost inflates,
// with nothing raising an error.
//
// This is asserted directly rather than through a compiled plan because it is a
// property of the checkpoint, not of a fixture: a repository whose routes
// happen to reach each package over distinct edges would pass a parity
// assertion with the fold dropped.
func TestTheRestoredEdgeSortStillFoldsDistinctPairs(t *testing.T) {
	t.Parallel()
	fx := newContextFixture(t)
	c, _ := pagedCompiler(t, fx, fx.Now, nil)
	sorts, err := newCompileSorts(c.cfg, c.sortDir)
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	defer sorts.Close()

	edges, err := newSort(sorts, "pkg-edge", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	edges = edges.WithFold(foldPkgEdgeDistinct)
	// One package reached three times over one edge, and once over another:
	// two DISTINCT pairs, which is what len(centrality[pkg]) counts.
	for range 3 {
		if err := edges.Add(pkgEdgeRec{Pkg: "a", RelationID: "r1"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := edges.Add(pkgEdgeRec{Pkg: "a", RelationID: "r2"}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	dir := t.TempDir()
	files, err := (&compileState{pass: passCentrality, edges: edges}).
		checkpointStream(dir, streamEdges, 1<<20)
	if err != nil {
		t.Fatalf("checkpointStream: %v", err)
	}
	restored := &compileState{pass: passCentrality}
	if err := restored.restoreStream(sorts, dir, streamEdges, files); err != nil {
		t.Fatalf("restoreStream: %v", err)
	}
	run, err := sortedRun(sorts, restored.edges)
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	if got := run.Len(); got != 2 {
		t.Fatalf("the restored edge stream holds %d record(s), want the 2 distinct pairs: "+
			"the checkpoint lost foldPkgEdgeDistinct and P-E would over-count every package", got)
	}
}

// A plan deadline that fires while P-A is RUNNING -- between two pages of the
// graph walk, not behind the pass -- ends the pass with a continuation and
// never a lost compile (ruling C7, finding B1). P-A is one pass over a walk
// that spans many pages, so before this the one boundary it could stop at was
// the one behind it, and a deadline inside it threw the whole walk away.
//
// The generated fixture is the one that can show it: it admits over three
// hundred entities and model.MaxPageItems is 200, so the walk is genuinely
// multi-page and the in-walk halt is reachable. st.pass staying AT passIngest
// is what says the halt landed inside the pass -- a halt at the boundary behind
// P-A would have advanced it to passHydrate -- and the live walk cursor says
// the walk was still running when it did.
//
// The resumed plan is then asserted byte-identical to the uninterrupted one:
// assertSamePlan compares model.ContextManifest.CanonicalHash, which IS the
// fold of the plan's canonical projection (entry ordinals, slices, exclusions),
// over an equal header and an equal notice list.
func TestADeadlineInsideTheGraphWalkResumesIntoTheUninterruptedPlan(t *testing.T) {
	t.Parallel()
	req := generatedRequest(generatedFullBudget)
	ref := newGeneratedFixture(t)
	want, err := intCompiler(t, ref, ref.Now).Compile(ref.ctx, req)
	if err != nil {
		t.Fatalf("the uninterrupted compile failed: %v", err)
	}
	if want.EntryCount == 0 {
		t.Fatalf("the reference plan selected nothing; every assertion below would pass over an empty plan")
	}

	fx := newGeneratedFixture(t)
	c, spools := pagedCompiler(t, fx, fx.Now, nil)
	token, st := checkpointState2(t, c, fx, req, passIngest)
	if st.ingest == nil {
		t.Fatalf("the halt at passIngest carried no seed sink; the walk's four pre-fold sorts are not in the checkpoint")
	}
	if st.ingest.cursor == "" {
		t.Fatalf("the halt at passIngest carried no walk cursor; it ended the walk rather than suspending it")
	}
	if st.ingest.seq == 0 || len(st.ingest.start) == 0 {
		t.Fatalf("the halt carried seq=%d over %d root(s); the continuation cannot reproduce the walk from that",
			st.ingest.seq, len(st.ingest.start))
	}

	got, err := c.CompilePage(fx.ctx, req, token)
	if err != nil {
		t.Fatalf("resuming a walk suspended mid-page failed: %v", err)
	}
	assertSamePlan(t, passIngest, got, want)

	live, err := spools.Sweep(fx.ctx, fx.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if live != 0 {
		t.Fatalf("the consumed mid-walk continuation left %d live byte(s) in the spool store, want 0", live)
	}
}
