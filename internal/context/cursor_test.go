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
	c, err := New(Options{
		Store:  fx.Store,
		Repo:   fx.Repo,
		Search: searchService(t, fx),
		Graph: func(ctx stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
			if release == nil {
				release = func() error { return nil }
			}
			return fx.scopeEngine(nil, fixtureCapabilities), release, nil
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
	c, _ := pagedCompiler(t, fx, fx.Now, nil)
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

// The deadline branch itself, executed: a compile whose query deadline has
// passed by the time the front half returns must answer truncated=deadline with
// a resumable cursor, an EMPTY manifest, and -- the invariant Section 14.4 puts
// above every other -- nothing written to the store. A partial plan that
// reached persistence would be indistinguishable to every later reader from a
// complete one, because a manifest header carries no "this is a fragment" bit.
//
// This is the row C-D3 could not write: it drove the checkpoint by calling it,
// so compile's own deadline branch never ran and "the deadline path persists no
// manifest" was an argument about the code rather than a fact about it.
func TestTheDeadlineBranchAnswersACursorAndStoresNoManifest(t *testing.T) {
	t.Parallel()
	req := model.ContextRequest{Task: "make `Place` idempotent", Phase: model.PhaseVerify}
	fx := newContextFixture(t)
	clock := newBoundaryClock()
	c, spools := pagedCompiler(t, fx, clock.now, clock.halt)

	res, err := c.CompilePage(fx.ctx, req, "")
	if err != nil {
		t.Fatalf("a compile that ran out of deadline at the boundary failed instead of continuing: %v", err)
	}
	if !res.Truncated || res.TruncationReason != truncationDeadline || res.NextCursor == "" {
		t.Fatalf("the deadline boundary answered truncated=%v reason=%q cursor=%q, want a deadline continuation",
			res.Truncated, res.TruncationReason, res.NextCursor)
	}
	if res.Manifest.ID != "" || res.Manifest.EntryCount != 0 {
		t.Fatalf("a truncated compile answered manifest %+v, want the zero header", res.Manifest)
	}

	// Nothing persisted. The identity is computable without compiling, which is
	// what lets this assert on the STORE rather than on the returned value.
	id := requestManifestID(t, c, fx, req)
	if _, stored, err := c.reuseManifest(fx.ctx, id); err != nil {
		t.Fatalf("reuseManifest: %v", err)
	} else if stored {
		t.Fatalf("the deadline path persisted manifest %s; a partial plan must never reach the store", id)
	}

	// And the continuation finishes it.
	got, err := c.CompilePage(fx.ctx, req, res.NextCursor)
	if err != nil {
		t.Fatalf("resuming the deadline continuation failed: %v", err)
	}
	if got.Truncated || got.NextCursor != "" || got.Manifest.ID != id {
		t.Fatalf("the continuation answered truncated=%v cursor=%q manifest=%s, want the finished plan %s",
			got.Truncated, got.NextCursor, got.Manifest.ID, id)
	}
	if got.Manifest.EntryCount == 0 {
		t.Fatalf("the continuation selected nothing; the row above would have passed over an empty plan")
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
