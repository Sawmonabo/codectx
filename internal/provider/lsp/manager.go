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
	"path/filepath"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
)

// Options bound one Manager. Every bound is finite; a zero value takes the
// default from config.Defaults() or the package default named below, and a
// negative value is rejected, because Section 20.2 forbids a setting that
// means unlimited.
type Options struct {
	// Runner is the shared process runner every server starts through.
	Runner *process.Runner
	// DataDir is the data directory; materializations live under
	// snapshot.MaterializeDir(DataDir).
	DataDir string
	// MaxServers caps concurrent language servers (providers.lsp.max_servers).
	MaxServers int
	// MaxOutstandingRequests caps in-flight requests per server
	// (providers.lsp.max_outstanding_requests).
	MaxOutstandingRequests int
	// RequestTimeout bounds one request (providers.lsp.request_timeout).
	RequestTimeout time.Duration
	// IdleTTL is how long a server with no open overlay is kept
	// (providers.lsp.idle_ttl).
	IdleTTL time.Duration
	// StartTimeout bounds the initialize handshake; StopTimeout bounds the
	// shutdown exchange and is the runner's grace before a forced stop.
	StartTimeout time.Duration
	StopTimeout  time.Duration
	// MaxOverlayBytes is the user's overlay bound and is unlimited by default.
	// When set it bounds, separately, the materialized snapshot (files that do
	// not fit are named and left out, never refused), one file admitted to the
	// pinned coordinate cache, and the bytes sent to a server in one rolling
	// window. It is a config.Limit so that the sentinel can never be used as a
	// number: arithmetic on it does not compile.
	MaxOverlayBytes config.Limit
	// MaxFrameBytes bounds one protocol message.
	MaxFrameBytes int64
}

// Package defaults for the bounds configuration does not name.
const (
	DefaultStartTimeout = 60 * time.Second
	DefaultStopTimeout  = 5 * time.Second
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
	// live holds every server whose process tree has not yet been reaped,
	// including servers that failed and were forgotten. Close waits on it, so
	// "Close returns once every process tree is reaped" is true of a failed
	// server too, not only of the ones still holding a slot.
	live   map[*server]struct{}
	closed bool
}

// entry is one server slot: ready is closed once the start attempt finished,
// with either srv or err set.
type entry struct {
	ready chan struct{}
	srv   *server
	err   error
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
	for _, b := range []struct {
		name  string
		value *int
		def   int
	}{
		{"max_servers", &opts.MaxServers, def.MaxServers},
		{"max_outstanding_requests", &opts.MaxOutstandingRequests, def.MaxOutstandingRequests},
	} {
		if *b.value < 0 {
			return nil, invalid("lsp %s is %d; a bound must not be negative", b.name, *b.value)
		}
		if *b.value == 0 {
			*b.value = b.def
		}
	}
	for _, b := range []struct {
		name  string
		value *time.Duration
		def   time.Duration
	}{
		{"request_timeout", &opts.RequestTimeout, def.RequestTimeout.Std()},
		{"idle_ttl", &opts.IdleTTL, def.IdleTTL.Std()},
		{"start_timeout", &opts.StartTimeout, DefaultStartTimeout},
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
	return &Manager{opts: opts, servers: make(map[serverKey]*entry), live: make(map[*server]struct{})}, nil
}

// Open returns an overlay over view answered by profile at profile.Root,
// starting the server lazily on first use and sharing a running one
// afterwards. Two projects of one repository are two servers: a server
// resolves a project from the directory it was started in, so one server
// rooted at a monorepo's workspace root knows none of the projects under it.
// The profile must come from Resolve. When every server slot is taken by a server nobody is
// using, the idle one is stopped to make room; when all are in use, the
// answer is CTX_RESOURCE_LIMIT rather than a queue.
func (m *Manager) Open(ctx context.Context, view model.SnapshotView, profile Profile) (*Overlay, error) {
	if view == nil {
		return nil, invalid("an overlay needs a snapshot view")
	}
	if len(profile.Tool.ArgvPrefix) == 0 || profile.Name == "" {
		return nil, trustRequired("the profile was not constructed by lsp.Resolve; a server the tool lock did not pin is never started")
	}
	key := serverKey{snapshot: view.Header().ID, profile: profile.Name, root: profile.Root}
	// Two attempts: the second covers a shared server that failed or began
	// stopping between being found and being acquired.
	for attempt := 0; attempt < 2; attempt++ {
		e, starter, err := m.slot(key)
		if err != nil {
			return nil, err
		}
		if starter {
			srv, err := startServer(ctx, m, view, profile)
			m.mu.Lock()
			e.srv, e.err = srv, err
			// A server that failed between starting and being recorded has
			// already been through forget, which found no entry to remove
			// because e.srv was still nil. Removing it here, under the same
			// lock that publishes it, is what keeps the dead entry from being
			// handed to every later Open. The identity check matters: expire
			// or forget may have replaced this entry already.
			if e2, ok := m.servers[key]; ok && e2 == e && (err != nil || srv.running() != nil) {
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
		}
		if e.err != nil {
			return nil, e.err
		}
		if err := e.srv.acquire(); err != nil {
			m.forget(e.srv)
			if attempt == 0 {
				continue
			}
			return nil, err
		}
		return &Overlay{s: e.srv}, nil
	}
	return nil, unavailable("the language server could not be acquired")
}

// slot finds or reserves the entry for key. starter is true when the caller
// must start the server and complete the entry.
func (m *Manager) slot(key serverKey) (*entry, bool, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, false, unavailable("the lsp manager is closed")
		}
		if e, ok := m.servers[key]; ok {
			m.mu.Unlock()
			return e, false, nil
		}
		if len(m.servers) < m.opts.MaxServers {
			e := &entry{ready: make(chan struct{})}
			m.servers[key] = e
			m.mu.Unlock()
			return e, true, nil
		}
		// Every slot is taken. Stop one idle server, if any, and try again.
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
		if idle == nil {
			return nil, false, resourceLimit("all %d language server slots are in use", m.opts.MaxServers).
				WithDetail("limit", "max_servers")
		}
		idle.stop()
	}
}

// isIdle reports a running server with no overlay open on it.
func (s *server) isIdle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == serverRunning && s.refs == 0
}

// expire is the idle timer's action: stop the server if it is still unused.
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
// starts a fresh one instead of finding the dead entry.
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

// Close stops every server and refuses further opens. It returns once every
// process tree is reaped and every materialization removed, including the
// trees of servers that failed and were forgotten: those are stopped through
// the same path, which for an already dying server is the wait for its exit.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.servers))
	for k, e := range m.servers {
		entries = append(entries, e)
		delete(m.servers, k)
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
	return nil
}

// Servers reports how many servers are currently running or starting.
func (m *Manager) Servers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}
