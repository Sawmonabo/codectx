// Package lsp is the snapshot-qualified working-tree overlay of Section 11.5:
// on-demand definition, references, implementations, type definition,
// document and workspace symbols and call hierarchy answered by an approved
// local language server running against a private materialization of the
// pinned snapshot.
//
// It is a query adapter, not a provider.Provider. Nothing it returns is
// persisted: every result is an ephemeral answer labelled with the server,
// its version and an input digest, carries language_server precision, and
// disappears without corrupting anything when the server stops. Canonical
// facts, generations and units are never read or written here.
//
// Trust comes from the embedded tool lock. A Definition names a supported
// server; only a Profile constructed by Resolve, which hands back the payload
// the lock pinned and internal/toolchain verified, can be started, and
// detection of a repository's kind never authorizes execution. Servers run
// through the shared internal/process runner with no shell, an allowlisted
// environment and bounded streams.
package lsp

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// Options configure one Manager. A zero duration or frame bound takes the
// default from config.Defaults() or the package default named below; the two
// config.Limit bounds are unlimited at zero, as every such bound is. A
// negative value is rejected.
type Options struct {
	// Runner is the shared process runner every server starts through.
	Runner *process.Runner
	// DataDir is the data directory; materializations live under
	// snapshot.MaterializeDir(DataDir).
	DataDir string
	// Admission is the process's one memory admission ledger -- the handle the
	// composition root built, never a second one: a manager with a running
	// total of its own would admit servers against an allocation the analysis
	// engine is already holding, and the process would reserve a multiple of
	// the machine's memory. Servers are admitted by the sum of the memory
	// reservations their pinned definitions declare, never by a count: a
	// monorepo answers as many projects at once as the machine has room for,
	// and one whose reservation is larger than the whole allocation runs alone
	// rather than being refused.
	//
	// It is not the run ledger below; the two share a word and nothing else.
	Admission *admission.Ledger
	// MaxOutstandingRequests bounds in-flight requests per server
	// (providers.lsp.max_outstanding_requests). Unlimited by default: each
	// caller is already bounded by its own gate.
	MaxOutstandingRequests config.Limit
	// RequestStallTimeout is how long a request tolerates the server making no
	// observable progress before it is declared hung
	// (providers.lsp.stall_timeout). It is a hang detector, never a deadline on
	// an answer: a server that is still consuming processor time is working,
	// and only one that is neither computing nor answering is wedged.
	RequestStallTimeout time.Duration
	// IdleTTL is how long a server with no open overlay is kept
	// (providers.lsp.idle_ttl).
	IdleTTL time.Duration
	// StopTimeout is the grace a server is given to leave once it has been
	// told to stop: the shutdown answer and the exit share it, and it is the
	// runner's grace between asking the tree to end and forcing it. It bounds
	// nothing else; the initialize handshake runs under RequestStallTimeout.
	StopTimeout time.Duration
	// MaxOverlayBytes is the user's overlay bound and is unlimited by default.
	// When set it bounds, separately, the materialized snapshot (files that do
	// not fit are named and left out, never refused), one file admitted to the
	// pinned coordinate cache, and the bytes sent to a server in one rolling
	// window. It is a config.Limit so that the sentinel can never be used as a
	// number: arithmetic on it does not compile.
	MaxOverlayBytes config.Limit
	// MaxFrameBytes bounds one protocol message.
	MaxFrameBytes int64
	// Ledger is the process's run ledger handle -- the one the composition
	// root composed, never a second one: the ledger file has a single
	// collector, and a manager that opened its own would be a second writer on
	// it. It may be nil, and then the manager records nothing, which is what a
	// composition with no ledger (a report, which never holds the workspace
	// lock) gets. A handle whose collector is detached -- a server start from a
	// query, with no hold -- records nothing in exactly the same way, rather
	// than writing without the workspace lock.
	//
	// The manager records into a run of its own rather than a generation's: a
	// server start is lazy, pooled and shared between generations, so it
	// belongs to none of them.
	Ledger *ledger.Ledger
}

// stageServerStart is the stage a language server's start is recorded under.
const stageServerStart = "server_start"

// Package defaults for the bounds configuration does not name.
const (
	DefaultStopTimeout = 5 * time.Second
	// DefaultDocCacheBytes is the pinned coordinate cache's ceiling when the
	// overlay bound is unlimited. The cache is lossless -- eviction costs a
	// re-read of bytes the snapshot still holds -- so it keeps a finite
	// ceiling, which is what makes the overlay's peak flat under an unlimited
	// bound instead of repository-sized.
	DefaultDocCacheBytes = 512 << 20
	DefaultMaxFrameBytes = 8 << 20
)

// Manager starts trusted servers lazily, one per (snapshot, profile), shares
// them among overlays, caps their number and stops them when idle. It is safe
// for concurrent use.
type Manager struct {
	opts Options

	mu      sync.Mutex
	servers map[serverKey]*entry
	// mats holds one materialization per snapshot, shared read-only by every
	// server of that snapshot. A server only ever READS the tree -- everything
	// it writes goes to its own private working directory -- so a copy each
	// would be servers x the whole snapshot on disk for nothing, which on a
	// large monorepo with several projects is several full copies of the
	// repository.
	mats map[model.SnapshotID]*materialization
	// live holds every server whose process tree has not yet been reaped,
	// including servers that failed and were forgotten. Close waits on it, so
	// "Close returns once every process tree is reaped" is true of a failed
	// server too, not only of the ones still holding a slot.
	live   map[*server]struct{}
	closed bool
	// overlay is this process's overlay run, opened by the FIRST server start
	// and deleted at Close. It is opened lazily and not at construction
	// because a run with no span under it is never written until the ledger
	// stops, and stopping would then write one row per process that never
	// started a server -- the row this run exists to avoid leaving behind.
	overlay *ledger.Run
}

// materialization is one snapshot's shared tree: ready is closed once the fill
// attempt finished, with either mat or err set, and refs counts the servers
// rooted inside it. The tree is removed when the last of them has exited.
type materialization struct {
	ready chan struct{}
	mat   *snapshot.Materialization
	err   error
	refs  int
}

// entry is one server slot. It is published before its room is reserved, so a
// second open of the same server waits on it instead of queuing a reservation
// of its own. ready is closed once the attempt finished, with either srv or
// err set. abandoned marks an attempt that ended while it was still waiting
// for room because the open reserving it stopped waiting: a waiter whose own
// context is live asks again rather than returning another caller's
// cancellation. cancel ends that wait; Close uses it so it never waits on room
// that may not be granted until other work ends.
//
// The room itself is never held by the entry: slot hands it to the starter,
// and startServer hands it to the server, which gives it back when its process
// has exited.
type entry struct {
	ready     chan struct{}
	srv       *server
	err       error
	abandoned bool
	cancel    context.CancelFunc
}

// New validates and defaults the options.
func New(opts Options) (*Manager, error) {
	if opts.Runner == nil {
		return nil, invalid("the lsp manager needs the shared process runner")
	}
	if !filepath.IsAbs(opts.DataDir) {
		return nil, invalid("the lsp manager needs an absolute data directory")
	}
	def := config.Defaults().Providers.LSP
	if opts.Admission == nil {
		return nil, invalid("the lsp manager needs the process memory admission ledger servers are admitted against")
	}
	if opts.MaxOutstandingRequests < 0 {
		return nil, invalid("lsp max_outstanding_requests is %s; a bound must not be negative", opts.MaxOutstandingRequests)
	}
	for _, b := range []struct {
		name  string
		value *time.Duration
		def   time.Duration
	}{
		{"stall_timeout", &opts.RequestStallTimeout, def.StallTimeout.Std()},
		{"idle_ttl", &opts.IdleTTL, def.IdleTTL.Std()},
		{"stop_timeout", &opts.StopTimeout, DefaultStopTimeout},
	} {
		if *b.value < 0 {
			return nil, invalid("lsp %s is %s; a bound must not be negative", b.name, *b.value)
		}
		if *b.value == 0 {
			*b.value = b.def
		}
	}
	for _, b := range []struct {
		name  string
		value *int64
		def   int64
	}{
		{"max_frame_bytes", &opts.MaxFrameBytes, DefaultMaxFrameBytes},
	} {
		if *b.value < 0 {
			return nil, invalid("lsp %s is %d; a bound must not be negative", b.name, *b.value)
		}
		if *b.value == 0 {
			*b.value = b.def
		}
	}
	if opts.MaxOverlayBytes < 0 {
		return nil, invalid("lsp max_overlay_bytes is %s; a bound must not be negative", opts.MaxOverlayBytes)
	}
	// Only a bound the user set can be exceeded; an unlimited one is the top of
	// the lattice and no frame size is over it.
	if opts.MaxOverlayBytes.Exceeded(opts.MaxFrameBytes) {
		return nil, invalid("lsp max_frame_bytes %d exceeds max_overlay_bytes %s", opts.MaxFrameBytes, opts.MaxOverlayBytes)
	}
	return &Manager{opts: opts,
		servers: make(map[serverKey]*entry), live: make(map[*server]struct{}),
		mats: make(map[model.SnapshotID]*materialization)}, nil
}

// Open returns an overlay over view answered by profile at profile.Root,
// starting the server lazily on first use and sharing a running one
// afterwards. Two projects of one repository are two servers: a server
// resolves a project from the directory it was started in, so one server
// rooted at a monorepo's workspace root knows none of the projects under it.
// The profile must come from Resolve. Room for the server is taken from the
// process's one admission ledger, in turn behind every other heavy child that
// asked first. When the machine has no room left for this server, an idle one
// is stopped to make room; when every running server is in use, the open waits
// for room rather than being refused -- another project's server already
// running is never an answer of CTX_RESOURCE_LIMIT.
func (m *Manager) Open(ctx context.Context, view model.SnapshotView, profile Profile) (*Overlay, error) {
	if view == nil {
		return nil, invalid("an overlay needs a snapshot view")
	}
	if len(profile.Tool.ArgvPrefix) == 0 || profile.Name == "" {
		return nil, trustRequired("the profile was not constructed by lsp.Resolve; a server the tool lock did not pin is never started")
	}
	key := serverKey{snapshot: view.Header().ID, profile: profile.Name, root: profile.Root}
	// Two attempts: the second covers a shared server that failed or began
	// stopping between being found and being acquired. A wait on a start that
	// was abandoned before it had room is not an attempt: nothing was found.
	for attempt := 0; ; {
		e, room, err := m.slot(ctx, key, admission.Reservation{
			MemoryBytes: profile.MemoryBudgetBytes,
			DiskBytes:   profile.DiskBudgetBytes,
		})
		if err != nil {
			return nil, err
		}
		if room != nil {
			srv, err := startServer(ctx, m, view, profile, room)
			m.mu.Lock()
			e.srv, e.err = srv, err
			// A server that failed between starting and being recorded has
			// already been through forget, which found no entry to remove
			// because e.srv was still nil. Removing it here, under the same
			// lock that publishes it, is what keeps the dead entry from being
			// handed to every later Open. The identity check matters: Close may
			// have removed this entry already.
			if cur, ok := m.servers[key]; ok && cur == e && (err != nil || srv.running() != nil) {
				delete(m.servers, key)
			}
			m.mu.Unlock()
			close(e.ready)
		} else {
			select {
			case <-e.ready:
			case <-ctx.Done():
				return nil, model.Canceled(ctx.Err())
			}
			if e.abandoned {
				continue
			}
		}
		if e.err != nil {
			return nil, e.err
		}
		// Acquired under the manager's lock, and only while the entry is still
		// published: stopIdle and expire judge a server idle and remove it
		// under the same lock, so a server one of them is stopping is never
		// handed out as a live one.
		m.mu.Lock()
		err = unavailable("the language server is stopping")
		if cur, ok := m.servers[key]; ok && cur == e {
			err = e.srv.acquire()
		}
		m.mu.Unlock()
		if err != nil {
			m.forget(e.srv)
			if attempt++; attempt < 2 {
				continue
			}
			return nil, err
		}
		return &Overlay{s: e.srv}, nil
	}
}

// slot finds the entry for key, or publishes one and reserves its room
// against the process's admission ledger. The reservation carries both
// dimensions: a server holds memory while it runs and disk while it indexes,
// and the two are granted in one act so no server holds half of what another
// needs. A non-nil room means the caller is the starter: it must start the
// server, hand room to it and complete the entry. Otherwise the caller waits
// on the entry it was given.
//
// The entry is published BEFORE the room is asked for, so one server is one
// reservation however many opens ask for it at once: a second open that
// queued a reservation of its own would hold the head of the ledger's queue,
// and every engine unit behind it, for room it would hand straight back. The
// ledger is never asked while this manager's mutex is held: the wait can be
// long, and the ledger calls stopIdle back, which takes that mutex. A wait
// that ends without room removes the entry and wakes its waiters, so none of
// them is left waiting on it.
func (m *Manager) slot(ctx context.Context, key serverKey, res admission.Reservation) (*entry, func(), error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, unavailable("the lsp manager is closed")
	}
	if e, ok := m.servers[key]; ok {
		m.mu.Unlock()
		return e, nil, nil
	}
	reserving, cancel := context.WithCancel(ctx)
	e := &entry{ready: make(chan struct{}), cancel: cancel}
	m.servers[key] = e
	m.mu.Unlock()

	// Queued first-in-first-out behind every other heavy child of this process,
	// so a server can never overtake an engine unit that asked first. What it
	// may still do, when it reaches the head and does not fit, is stop a server
	// nobody is using: another project's server already running is never an
	// answer of CTX_RESOURCE_LIMIT.
	room, err := m.opts.Admission.ReserveWith(reserving, res, m.stopIdle)

	m.mu.Lock()
	defer m.mu.Unlock()
	cancel()
	e.cancel = nil
	if m.closed {
		// Close removed the entry and ended the wait, or the room was granted
		// just as it closed; either way no server starts.
		if err == nil {
			room()
		}
		err = unavailable("the lsp manager is closed")
	} else if err != nil {
		e.abandoned = true
	}
	if err != nil {
		if cur, ok := m.servers[key]; ok && cur == e {
			delete(m.servers, key)
		}
		e.err = err
		close(e.ready)
		return nil, nil, err
	}
	return e, room, nil
}

// stopIdle stops one server nobody is using, which gives its room back to the
// ledger once its process has exited -- never before, or the ledger would
// admit the next child while the stopping one still holds the memory. It is
// the make-room step the ledger runs for a server open that has reached the
// head of the queue and does not fit; a manager holding nothing idle frees
// nothing and the open simply waits its turn. An entry still starting, or
// still waiting for room, is not ready and is never chosen.
func (m *Manager) stopIdle() {
	m.mu.Lock()
	var idle *server
	for k, e := range m.servers {
		select {
		case <-e.ready:
		default:
			continue
		}
		if e.srv != nil && e.srv.isIdle() {
			idle = e.srv
			delete(m.servers, k)
			break
		}
	}
	m.mu.Unlock()
	if idle != nil {
		idle.stop()
	}
}

// materialize returns the snapshot's shared tree, filling it on the first
// call and taking a reference for the caller. Every reference is given back
// through releaseMat.
func (m *Manager) materialize(ctx context.Context, view model.SnapshotView) (*snapshot.Materialization, error) {
	id := view.Header().ID
	m.mu.Lock()
	shared, ok := m.mats[id]
	if ok {
		shared.refs++
		m.mu.Unlock()
		select {
		case <-shared.ready:
		case <-ctx.Done():
			m.releaseMat(id)
			return nil, model.Canceled(ctx.Err())
		}
		if shared.err != nil {
			m.releaseMat(id)
			return nil, shared.err
		}
		return shared.mat, nil
	}
	shared = &materialization{ready: make(chan struct{}), refs: 1}
	m.mats[id] = shared
	m.mu.Unlock()
	shared.mat, shared.err = snapshot.Materialize(ctx, view, model.FileSelection{}, snapshot.MaterializeOptions{
		Dir: snapshot.MaterializeDir(m.opts.DataDir), MaxBytes: m.opts.MaxOverlayBytes.Value(),
	})
	close(shared.ready)
	if shared.err != nil {
		// A failed fill must not be handed to the next caller: releasing the
		// starter's own reference removes it, so the next Open tries again.
		err := shared.err
		m.releaseMat(id)
		return nil, err
	}
	return shared.mat, nil
}

// releaseMat gives one reference back and removes the tree once the last
// server rooted in it has exited. Removal goes through Materialization.Close,
// which is the arena's own paced release.
func (m *Manager) releaseMat(id model.SnapshotID) error {
	m.mu.Lock()
	shared, ok := m.mats[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	shared.refs--
	if shared.refs > 0 {
		m.mu.Unlock()
		return nil
	}
	delete(m.mats, id)
	m.mu.Unlock()
	if shared.mat == nil {
		return nil
	}
	return shared.mat.Close()
}

// isIdle reports a running server with no overlay open on it.
func (s *server) isIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == serverRunning && s.refs == 0
}

// expire is the idle timer's action: stop the server if it is still unused.
// Its room comes back when the process has exited.
func (m *Manager) expire(s *server) {
	m.mu.Lock()
	e, ok := m.servers[s.key]
	if !ok || e.srv != s || !s.isIdle() {
		m.mu.Unlock()
		return
	}
	delete(m.servers, s.key)
	m.mu.Unlock()
	s.stop()
}

// forget removes a server that failed or is stopping, so the next Open
// starts a fresh one instead of finding the dead entry. Its room comes back
// when the process has exited, which the failure path is already bringing
// about.
func (m *Manager) forget(s *server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.servers[s.key]; ok && e.srv == s {
		delete(m.servers, s.key)
	}
}

// track registers a started server until its process tree is reaped; untrack
// is called from the exit path, after the materialization is removed.
func (m *Manager) track(s *server) {
	m.mu.Lock()
	m.live[s] = struct{}{}
	m.mu.Unlock()
}

func (m *Manager) untrack(s *server) {
	m.mu.Lock()
	delete(m.live, s)
	m.mu.Unlock()
}

// overlayContext returns a context whose spans belong to this process's
// overlay run, opening that run on the first call. repositoryID is the
// repository the snapshot being served names, which is the only place the
// manager can learn it without spelling the identity a second time.
//
// A ledger failure never fails a server start: the answer the server gives is
// correct whatever the accounting did, so the failure is logged with its
// diagnostic code and the start proceeds with a run that records nothing. A
// start with no hold gets the same nil run, because the handle it is given has
// no collector attached then.
//
// The run is opened once per process and may outlive the hold that recorded
// it. internal/ledger finalizes such a run when its collector detaches and
// refuses everything it publishes afterwards, so this cache can never reach
// into a later hold's accounting.
func (m *Manager) overlayContext(ctx context.Context, repositoryID string) context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.overlay == nil {
		run, err := m.opts.Ledger.NewRun(ledger.KindOverlay, repositoryID)
		if err != nil {
			code := model.CodeInternal
			var typed *model.Error
			if errors.As(err, &typed) {
				code = typed.Code
			}
			slog.Warn("language server starts are not being recorded in the run ledger",
				"component", "lsp", "error_code", code, "error", err.Error())
			return ctx
		}
		if run == nil {
			return ctx
		}
		m.overlay = run
	}
	return m.overlay.Context(ctx)
}

// endOverlay ends this process's overlay run and removes it. A process that
// exits cleanly takes its own row with it; the ledger's collection pass takes
// the rows of the processes that did not.
func (m *Manager) endOverlay() error {
	m.mu.Lock()
	run := m.overlay
	m.overlay = nil
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	run.Finish(ledger.OutcomeOK)
	return m.opts.Ledger.DiscardRun(context.Background(), run)
}

// Close stops every server and refuses further opens. It returns once every
// process tree is reaped, its room given back and every materialization
// removed, including the trees of servers that failed and were forgotten:
// those are stopped through the same path, which for an already dying server
// is the wait for its exit. A start still waiting for room stops waiting.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.servers))
	for k, e := range m.servers {
		entries = append(entries, e)
		delete(m.servers, k)
		if e.cancel != nil {
			e.cancel()
		}
	}
	live := make([]*server, 0, len(m.live))
	for s := range m.live {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.ready
		if e.srv != nil {
			e.srv.stop()
		}
	}
	for _, s := range live {
		s.stop()
	}
	// Last, once no start can still open a span under it.
	return m.endOverlay()
}

// Servers reports how many servers are currently running, starting or waiting
// for room.
func (m *Manager) Servers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}
