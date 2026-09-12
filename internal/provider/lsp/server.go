package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/source"
)

// serverState is the lifecycle of one language server process.
type serverState int

const (
	serverRunning serverState = iota
	serverClosing
	serverFailed
)

// Capabilities are the overlay operations the server advertised in its
// initialize result. An operation the server did not advertise is reported
// unavailable without a request being sent; nothing is substituted for it.
type Capabilities struct {
	Definition       bool `json:"definition"`
	TypeDefinition   bool `json:"type_definition"`
	Implementations  bool `json:"implementations"`
	References       bool `json:"references"`
	DocumentSymbols  bool `json:"document_symbols"`
	WorkspaceSymbols bool `json:"workspace_symbols"`
	CallHierarchy    bool `json:"call_hierarchy"`
}

// server is one running language server bound to one snapshot: its private
// materialization, its process, its connection and the documents it has been
// told about. It is shared by every Overlay opened for the same snapshot and
// profile and stopped when the last one closes and the idle TTL passes.
type server struct {
	key     serverKey
	profile Profile
	view    model.SnapshotView
	opts    Options
	manager *Manager

	mat  *snapshot.Materialization
	uris materializationURI
	conn *conn
	enc  source.ColumnEncoding
	caps Capabilities
	sync bool
	// binding labels every result: provider, server version and input digest.
	binding model.OverlayBinding

	runCancel context.CancelFunc
	stdinR    *io.PipeReader
	stdinW    *io.PipeWriter
	stdoutR   *io.PipeReader
	stdoutW   *io.PipeWriter
	// exited is closed once the process tree is reaped and the
	// materialization removed.
	exited chan struct{}

	mu      sync.Mutex
	state   serverState
	failure error
	refs    int
	idle    *time.Timer

	docMu    sync.Mutex
	docs     map[model.FileID]*document
	docOrder []model.FileID
	docBytes int64
	// opened maps each document the server has been told about to its path.
	opened    map[model.FileID]string
	openOrder []model.FileID
}

type serverKey struct {
	snapshot model.SnapshotID
	profile  string
}

// pipeStream joins the server's stdout (read side) and stdin (write side)
// into the one io.ReadWriter the connection speaks over.
type pipeStream struct {
	r io.Reader
	w io.Writer
}

func (p pipeStream) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeStream) Write(b []byte) (int, error) { return p.w.Write(b) }

// errServerGone is the write-side error after the process has exited.
var errServerGone = errors.New("the language server process has exited")

// startServer materializes the snapshot, starts the approved executable
// through the shared runner, performs the initialize/initialized handshake
// and verifies the version and position encoding. Any failure releases the
// process, the pipes and the materialization before returning.
func startServer(ctx context.Context, m *Manager, view model.SnapshotView, p Profile) (*server, error) {
	sum, err := p.checksum()
	if err != nil {
		return nil, err
	}
	snap := view.Header()
	mat, err := snapshot.Materialize(ctx, view, model.FileSelection{}, snapshot.MaterializeOptions{
		Dir: snapshot.MaterializeDir(m.opts.DataDir), MaxBytes: m.opts.MaxOverlayBytes,
	})
	if err != nil {
		return nil, err
	}
	s := &server{
		key:     serverKey{snapshot: snap.ID, profile: p.Name},
		profile: p, view: view, opts: m.opts, manager: m,
		mat:    mat,
		uris:   materializationURI{root: mat.Root()},
		exited: make(chan struct{}),
		docs:   make(map[model.FileID]*document),
		opened: make(map[model.FileID]string),
	}
	s.stdinR, s.stdinW = io.Pipe()
	s.stdoutR, s.stdoutW = io.Pipe()
	s.conn = newConn(pipeStream{r: s.stdoutR, w: s.stdinW}, m.opts.MaxFrameBytes, m.opts.MaxOverlayBytes,
		m.opts.MaxOutstandingRequests, s.handleServerRequest)

	if err := os.MkdirAll(p.WorkDir, 0o700); err != nil {
		mat.Close()
		return nil, unavailable("language server %q work directory cannot be created: %v", p.Name, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.runCancel = cancel
	spec := process.Spec{
		Path: p.Executable,
		Args: p.argv(mat.Root()),
		Dir:  p.WorkDir,
		Env:  p.env(),
		// The client writes requests into stdinR's other end for the life of
		// the server; the runner copies them to the child and closes the
		// child's stdin when the write end is closed at shutdown.
		Stdin:         s.stdinR,
		MaxStdinBytes: m.opts.MaxOverlayBytes + 1,
		// Responses stream into stdoutW, which the connection reads. Stderr
		// is a server's log and is not retained (Section 22: raw child
		// output stays out of ordinary logs); its bound still terminates a
		// server that floods it.
		Stdout:                 s.stdoutW,
		Stderr:                 io.Discard,
		MaxStdoutBytes:         m.opts.MaxOverlayBytes,
		MaxStderrBytes:         m.opts.MaxOverlayBytes,
		Timeout:                p.Timeout,
		Grace:                  m.opts.StopTimeout,
		MemoryReservationBytes: p.MemoryBudgetBytes,
		DiskReservationBytes:   p.DiskBudgetBytes,
	}
	started := make(chan error, 1)
	go func() {
		_, err := m.opts.Runner.Run(runCtx, spec)
		started <- err
		s.onExit(err)
	}()
	go func() {
		err := s.conn.run()
		s.mu.Lock()
		closing := s.state == serverClosing
		s.mu.Unlock()
		if closing {
			return
		}
		if err == nil {
			err = unavailable("the language server closed its connection")
		}
		s.fail(err)
	}()

	if err := s.initialize(ctx, snap, sum); err != nil {
		// A start that failed after the process exists must not leave it: the
		// same path a running server takes on failure.
		s.fail(err)
		<-s.exited
		if runErr := <-started; runErr != nil && !errors.Is(runErr, context.Canceled) {
			// The runner's reason (not installed, not executable, exited at
			// once) is more precise than the handshake timeout it caused.
			var typed *model.Error
			if errors.As(runErr, &typed) && typed.Code != model.CodeCanceled {
				return nil, runErr
			}
		}
		return nil, err
	}
	return s, nil
}

// initialize performs the handshake and checks what the server claimed.
func (s *server) initialize(ctx context.Context, snap model.Snapshot, checksum string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.StartTimeout)
	defer cancel()
	params := initializeParams{
		ClientInfo:       clientInfo{Name: "codectx", Version: model.CurrentBuildInfo().Version},
		RootURI:          s.uris.rootURI(),
		WorkspaceFolders: []workspaceFolder{{URI: s.uris.rootURI(), Name: "snapshot"}},
		Capabilities: clientCapabilities{
			General: generalCapabilities{PositionEncodings: offeredEncodings},
			TextDocument: textDocumentCapabilities{
				Definition:     linkCapability{LinkSupport: true},
				TypeDefinition: linkCapability{LinkSupport: true},
				Implementation: linkCapability{LinkSupport: true},
				DocumentSymbol: documentSymbolCapability{HierarchicalDocumentSymbolSupport: true},
			},
			Workspace: workspaceCapabilities{Configuration: true, WorkspaceFolders: true},
		},
	}
	var result initializeResult
	if err := s.conn.call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	wire := result.Capabilities.PositionEncoding
	if wire == "" {
		wire = "utf-16"
	}
	enc, ok := encodingOf(wire)
	if !ok {
		return outputInvalid("the language server chose position encoding %q, which this client did not offer", truncate(wire, 32))
	}
	s.enc = enc
	version := ""
	if result.ServerInfo != nil {
		version = result.ServerInfo.Version
	}
	if !versionMatches(s.profile.VersionConstraint, version) {
		return trustRequired("language server %q reported version %q, which does not satisfy the approved constraint %q",
			s.profile.Name, truncate(version, 64), s.profile.VersionConstraint).WithDetail("profile", s.profile.Name)
	}
	c := result.Capabilities
	s.caps = Capabilities{
		Definition:       provided(c.DefinitionProvider),
		TypeDefinition:   provided(c.TypeDefinitionProvider),
		Implementations:  provided(c.ImplementationProvider),
		References:       provided(c.ReferencesProvider),
		DocumentSymbols:  provided(c.DocumentSymbolProvider),
		WorkspaceSymbols: provided(c.WorkspaceSymbolProvider),
		CallHierarchy:    provided(c.CallHierarchyProvider),
	}
	s.sync = opensDocuments(c.TextDocumentSync)
	s.binding = model.OverlayBinding{
		ProviderID:      "lsp:" + s.profile.Name,
		ProviderVersion: serverVersion(version),
		InputDigest:     inputDigest(snap, s.profile, checksum, version, wire),
	}
	if err := s.binding.Validate(); err != nil {
		return err
	}
	return s.conn.notify("initialized", struct{}{})
}

// handleServerRequest is the explicit policy for server-initiated requests:
// workspace/configuration is answered with no settings, bounded in item
// count; every other request, workspace/applyEdit above all, is a protocol
// error. The client never edits, never runs a command and never opens a URI
// on a server's behalf.
func (s *server) handleServerRequest(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "workspace/configuration":
		var p configurationParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "configuration items are malformed"}
		}
		if len(p.Items) > maxConfigurationItems {
			return nil, &rpcError{Code: rpcInvalidParams, Message: "too many configuration items"}
		}
		return make([]any, len(p.Items)), nil
	case "workspace/applyEdit":
		return nil, &rpcError{Code: rpcMethodNotFound, Message: "codectx never applies edits: the materialization is read-only input"}
	default:
		return nil, &rpcError{Code: rpcMethodNotFound, Message: "codectx does not serve " + method}
	}
}

// onExit runs once the runner has reaped the process tree. It ends the
// stream on both sides so the reader sees end of file and any writer fails
// instead of blocking on a pipe nobody drains, removes the materialization,
// and reports the exit as a failure unless this was a requested stop.
func (s *server) onExit(runErr error) {
	s.stdoutW.Close()
	s.stdinR.CloseWithError(errServerGone)
	matErr := s.mat.Close()
	s.mu.Lock()
	closing := s.state == serverClosing
	s.mu.Unlock()
	if !closing {
		err := runErr
		if err == nil || errors.Is(err, context.Canceled) {
			err = unavailable("the language server exited")
		}
		if matErr != nil {
			err = errors.Join(err, matErr)
		}
		s.fail(err)
	}
	close(s.exited)
}

// fail latches the server as failed: pending calls are released with err,
// the process tree is terminated, and the manager forgets the server so the
// next Open starts a fresh one. Canonical facts are untouched: this package
// never writes any. It is idempotent.
func (s *server) fail(err error) {
	s.mu.Lock()
	if s.state == serverFailed {
		s.mu.Unlock()
		return
	}
	s.state = serverFailed
	s.failure = err
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.mu.Unlock()
	s.conn.fail(err)
	s.stdoutR.CloseWithError(err)
	s.runCancel()
	s.manager.forget(s)
}

// stop is the orderly shutdown: shutdown request, exit notification, close
// the server's stdin, and wait for the process tree to be reaped, forcing it
// through the runner when it does not leave on its own. It returns once the
// process is gone and the materialization removed.
func (s *server) stop() {
	s.mu.Lock()
	if s.state != serverRunning {
		s.mu.Unlock()
		<-s.exited
		return
	}
	s.state = serverClosing
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.opts.StopTimeout)
	defer cancel()
	// Errors here are not actionable: whatever the server answers, it is told
	// to exit and its stdin is closed, and the runner terminates what remains.
	_ = s.conn.call(ctx, "shutdown", nil, nil)
	_ = s.conn.notify("exit", nil)
	s.conn.fail(unavailable("the language server was stopped"))
	s.stdinW.Close()
	select {
	case <-s.exited:
	case <-time.After(s.opts.StopTimeout):
		s.runCancel()
		<-s.exited
	}
	s.stdoutR.Close()
}

// acquire registers one more overlay on a running server.
func (s *server) acquire() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != serverRunning {
		if s.failure != nil {
			return s.failure
		}
		return unavailable("the language server is stopping")
	}
	s.refs++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	return nil
}

// release drops one overlay; the last one starts the idle timer.
func (s *server) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs > 0 {
		s.refs--
	}
	if s.refs == 0 && s.state == serverRunning && s.idle == nil {
		s.idle = time.AfterFunc(s.opts.IdleTTL, func() { s.manager.expire(s) })
	}
}

// running returns the failure that ended the server, or nil while it serves.
func (s *server) running() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case serverFailed:
		return unavailable("the language server overlay failed: %v", s.failure).WithDetail("profile", s.profile.Name)
	case serverClosing:
		return unavailable("the language server overlay is closed").WithDetail("profile", s.profile.Name)
	}
	return nil
}

// document returns the pinned bytes of one snapshot file, reading them from
// the verified view (never the materialization the server may have written
// to, never the live checkout) and caching them under the overlay byte bound
// with least-recently-used eviction. notFound is true when the snapshot has
// no such file.
func (s *server) document(ctx context.Context, id model.FileID) (doc *document, notFound bool, err error) {
	s.docMu.Lock()
	defer s.docMu.Unlock()
	if d, ok := s.docs[id]; ok {
		s.touch(id)
		return d, false, nil
	}
	rc, fv, err := s.view.Open(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	defer rc.Close()
	if fv.Size > s.opts.MaxOverlayBytes {
		return nil, false, resourceLimit("file %s is %d bytes, over the %d-byte overlay bound", fv.Path, fv.Size, s.opts.MaxOverlayBytes).
			WithDetail("limit", "max_overlay_bytes")
	}
	data, err := io.ReadAll(io.LimitReader(rc, fv.Size+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) != fv.Size {
		return nil, false, &model.Error{Code: model.CodeSourceIntegrity, Message: "retained bytes differ in length from the manifest"}
	}
	for s.docBytes+fv.Size > s.opts.MaxOverlayBytes && len(s.docOrder) > 0 {
		oldest := s.docOrder[0]
		s.docOrder = s.docOrder[1:]
		s.docBytes -= int64(len(s.docs[oldest].data))
		delete(s.docs, oldest)
	}
	d := &document{version: fv, data: data, cursor: source.NewCursor(data)}
	s.docs[id] = d
	s.docOrder = append(s.docOrder, id)
	s.docBytes += fv.Size
	return d, false, nil
}

func (s *server) touch(id model.FileID) {
	for i, other := range s.docOrder {
		if other == id {
			s.docOrder = append(s.docOrder[:i], s.docOrder[i+1:]...)
			break
		}
	}
	s.docOrder = append(s.docOrder, id)
}

// ensureOpen synchronizes one document to the server before a request
// positioned in it: didOpen with the pinned text once, didClose for the
// least recently opened when more than MaxOpenDocuments are open. The bytes
// never change while the snapshot is pinned, so didChange is never needed.
func (s *server) ensureOpen(d *document) error {
	if !s.sync {
		return nil
	}
	s.docMu.Lock()
	defer s.docMu.Unlock()
	if _, ok := s.opened[d.version.ID]; ok {
		return nil
	}
	if !isText(d.data) {
		return invalid("file %s is not UTF-8 text; a language server cannot address positions in it", d.version.Path)
	}
	for len(s.openOrder) >= maxOpenDocuments {
		oldest := s.openOrder[0]
		s.openOrder = s.openOrder[1:]
		path := s.opened[oldest]
		delete(s.opened, oldest)
		if err := s.conn.notify("textDocument/didClose", didCloseParams{TextDocument: textDocumentIdentifier{URI: s.uris.uri(path)}}); err != nil {
			return err
		}
	}
	err := s.conn.notify("textDocument/didOpen", didOpenParams{TextDocument: textDocumentItem{
		URI: s.uris.uri(d.version.Path), LanguageID: languageID(d.version.Language), Version: 1, Text: string(d.data),
	}})
	if err != nil {
		return err
	}
	s.opened[d.version.ID] = d.version.Path
	s.openOrder = append(s.openOrder, d.version.ID)
	return nil
}

// maxOpenDocuments bounds the documents a server holds open for this client.
const maxOpenDocuments = 16

// languageID maps a snapshot manifest language tag to the LSP languageId.
// The tags agree except for TSX; JSX files carry the manifest's "javascript".
func languageID(tag string) string {
	if tag == "tsx" {
		return "typescriptreact"
	}
	if tag == "" {
		return "plaintext"
	}
	return tag
}

// isNotFound recognizes a view rejecting the file identity itself: a lookup
// that found no row (storage's CTX_ARGUMENT_INVALID with reason=not_found)
// or a deletion tombstone. Either way the snapshot has no bytes for it.
func isNotFound(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid
}
