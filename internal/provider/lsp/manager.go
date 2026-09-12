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
// Trust comes from configuration alone. A Definition names a supported
// server; only a Profile constructed by Trusted from an [analyzers.<name>]
// approval can be started, and detection of a repository's kind never
// authorizes execution. Servers run through the shared internal/process
// runner with no shell, an allowlisted environment and bounded streams.
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
	// MaxOverlayBytes bounds, separately, the materialized snapshot, the
	// pinned bytes cached for coordinate conversion, and the bytes sent to
	// and received from a server over its lifetime.
	MaxOverlayBytes int64
	// MaxFrameBytes bounds one protocol message.
	MaxFrameBytes int64
}

// Package defaults for the bounds configuration does not name.
const (
	DefaultStartTimeout    = 60 * time.Second
	DefaultStopTimeout     = 5 * time.Second
	DefaultMaxOverlayBytes = 512 << 20
	DefaultMaxFrameBytes   = 8 << 20
)

// Manager starts trusted servers lazily, one per (snapshot, profile), shares
// them among overlays, caps their number and stops them when idle. It is safe
// for concurrent use.
type Manager struct {
	opts Options

	mu      sync.Mutex
	servers map[serverKey]*entry
	closed  bool
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
		{"max_overlay_bytes", &opts.MaxOverlayBytes, DefaultMaxOverlayBytes},
		{"max_frame_bytes", &opts.MaxFrameBytes, DefaultMaxFrameBytes},
	} {
		if *b.value < 0 {
			return nil, invalid("lsp %s is %d; a bound must not be negative", b.name, *b.value)
		}
		if *b.value == 0 {
			*b.value = b.def
		}
	}
	if opts.MaxFrameBytes > opts.MaxOverlayBytes {
		return nil, invalid("lsp max_frame_bytes %d exceeds max_overlay_bytes %d", opts.MaxFrameBytes, opts.MaxOverlayBytes)
	}
	return &Manager{opts: opts, servers: make(map[serverKey]*entry)}, nil
}

// Open returns an overlay over view answered by profile, starting the server
// lazily on first use and sharing a running one afterwards. The profile must
// come from Trusted. When every server slot is taken by a server nobody is
// using, the idle one is stopped to make room; when all are in use, the
// answer is CTX_RESOURCE_LIMIT rather than a queue.
func (m *Manager) Open(ctx context.Context, view model.SnapshotView, profile Profile) (*Overlay, error) {
	if view == nil {
		return nil, invalid("an overlay needs a snapshot view")
	}
	if profile.Executable == "" || profile.Name == "" {
		return nil, trustRequired("the profile was not constructed by lsp.Trusted; an unapproved server is never started")
	}
	key := serverKey{snapshot: view.Header().ID, profile: profile.Name}
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
			if err != nil && m.servers[key] == e {
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

// Close stops every server and refuses further opens. It returns once every
// process tree is reaped and every materialization removed.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.servers))
	for k, e := range m.servers {
		entries = append(entries, e)
		delete(m.servers, k)
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.ready
		if e.srv != nil {
			e.srv.stop()
		}
	}
	return nil
}

// Servers reports how many servers are currently running or starting.
func (m *Manager) Servers() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}
