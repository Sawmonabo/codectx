package context

import (
	stdcontext "context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
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
func pagedCompiler(t *testing.T, fx *contextFixture, now func() time.Time) (*Compiler, *pagination.Spools) {
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
	c, err := New(Options{
		Store:  fx.Store,
		Repo:   fx.Repo,
		Search: searchService(t, fx),
		Graph: func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
			return fx.scopeEngine(nil, fixtureCapabilities), func() error { return nil }, nil
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
	c, spools := pagedCompiler(t, fx, now)
	token := checkpointAtBoundary(t, c, fx, req)

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
	c, _ := pagedCompiler(t, fx, fx.Now)
	token := checkpointAtBoundary(t, c, fx,
		model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify})

	_, err := c.CompilePage(fx.ctx, model.ContextRequest{Task: "something else entirely", Phase: model.PhaseVerify}, token)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCursorInvalid {
		t.Fatalf("resuming another request answered %v, want CTX_CURSOR_INVALID", err)
	}
}

// checkpointAtBoundary drives req through the front half and takes the
// checkpoint the P-F/P-G deadline path takes, returning the token it minted.
//
// It calls the boundary directly rather than forcing a real deadline: the
// deadline is wall-clock and firing it inside a fixture compile would stop a
// pass rather than land on the boundary. What is under test is what the
// checkpoint persists and what the resume restores -- deadlineReached is one
// comparison over context.Context and has no state of its own.
func checkpointAtBoundary(t *testing.T, c *Compiler, fx *contextFixture, req model.ContextRequest) string {
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
	sorts, err := newCompileSorts(c.cfg, c.sortDir)
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	defer sorts.Close()
	ranked, scoped, complete, err := c.frontHalf(fx.ctx, reader, binding.GenerationID, req, sorts)
	if err != nil {
		t.Fatalf("the front half failed: %v", err)
	}
	token, err := c.checkpointHalf(fx.ctx, binding, string(id), ranked, scoped, complete)
	if err != nil {
		t.Fatalf("checkpointHalf: %v", err)
	}
	if token == "" {
		t.Fatalf("the boundary minted no continuation token")
	}
	return token
}
