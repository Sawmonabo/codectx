package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	contextpkg "github.com/Sawmonabo/codectx/internal/context"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Workspace is one opened workspace: the composed stack plus the index
// coordinator over it. Opened for indexing it is the single cross-process owner
// of Section 13.2 -- the workspace lock is held from the open in a command
// whose whole life is one run, and for the duration of each operation in a
// serving session -- so exactly one of
// `index`, `refresh`, `watch` and the MCP server builds at a time and a second
// caller is told the workspace is busy rather than becoming a second writer.
// Opened for a report it holds no lock and can build nothing.
type Workspace struct {
	s     *stack
	coord *index.Coordinator
	// status is the one status path of this process, bound to the
	// composition's READER handle. In every mode but the server's that handle
	// IS the writer, so this is the path `codectx status` has always taken; in
	// the server it is the second, read-only handle, which is what keeps
	// codectx_index_status -- the one question an agent asks while a refresh
	// runs -- off the writer. Pinning a generation through the writer records
	// a retention lease, and that write commits the refresh's ingestion group
	// early and then queues the tool call behind it.
	status *index.StatusReader
}

// OpenOptions are the caller-supplied inputs of an indexing open. They are one
// exported struct because the CLI now resolves four independent flags into
// them -- the lock wait, `--rebuild`, and the `--scip-index`/`--scip-inputs`
// pair -- and four positional parameters that must agree read worse than one
// value that carries the agreement.
type OpenOptions struct {
	// Operation is what this process is doing, in the words a person would
	// recognise on the refusal another process reads when it finds the
	// workspace busy -- an index, a refresh, a watch, a connected session.
	// The holder records it in the lock file, so an open that will take the
	// lock and leaves it empty is refused rather than holding anonymously.
	Operation string
	// Wait is how long the acquisition of the workspace lock retries before
	// reporting CTX_WORKSPACE_BUSY; Wait <= 0 tries once. The acquisition is
	// the open itself for an indexing command and the first build for a
	// server, so this is what either of those waits.
	Wait time.Duration
	// Rebuild opens an explicitly requested new cache beside the configured
	// one and leaves the existing database untouched (Section 12.2).
	Rebuild bool
	// SCIPImport is the root-relative path of a supplied SCIP index inside the
	// snapshot, and SCIPManifest the root-relative path of the optional
	// input-hash manifest that describes it. Both are passed to the SCIP
	// provider as they are: scip.New validates them and rejects a manifest
	// with no index to describe, so neither is re-checked on the way here.
	SCIPImport, SCIPManifest string
}

// OpenWorkspace composes the workspace for a run that will index.
//
// A payload an admitted unit needs is installed on demand, by the unit
// (Section 11.7): an analyzer that is pinned but not yet downloaded is a fetch
// at unit time, not a missing capability and not a cost this call pays.
func OpenWorkspace(ctx context.Context, repo string, o OpenOptions) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeIndex, operation: o.Operation, wait: o.Wait, rebuild: o.Rebuild,
		scipImport: o.SCIPImport, scipManifest: o.SCIPManifest})
}

// OpenWorkspaceForReport composes the workspace for a read-only report. It
// takes no workspace lock, mutates no generation and installs nothing -- it
// creates only the cache's own directories -- so a report answers while
// another process indexes or watches.
func OpenWorkspaceForReport(ctx context.Context, repo string) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeReport})
}

// OpenWorkspaceForQuery composes the workspace for a command that only answers
// questions. It writes nothing to the database at all -- no writer connection
// is opened -- so it answers while another process is indexing or watching
// instead of waiting out that run's write transaction, and it delays that run
// by nothing. A command that records anything, a reading session included,
// uses OpenWorkspaceForReport.
func OpenWorkspaceForQuery(ctx context.Context, repo string) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeQuery})
}

// OpenWorkspaceForServer composes the workspace for the one process that both
// indexes and answers questions: it opens a second, read-only handle on the
// same database beside the writer, and it takes NEITHER the workspace lock nor
// a single write to open. An agent's server must come up and answer beside an
// index the person started in a terminal, so the lock, the startup recovery
// and the collection pass are taken by the operation that needs them -- the
// refresh tool, or a watch pass -- and given back when it ends, and a
// workspace that is busy then refuses that one operation rather than the
// session. Workspace.ReadServices is the facade bound to
// that handle, and it is what the server's read tools must be served through:
// they then answer while this process's own refresh writes, without committing
// its ingestion group early and without waiting behind it.
func OpenWorkspaceForServer(ctx context.Context, repo string, o OpenOptions) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeServe, operation: o.Operation, wait: o.Wait, rebuild: o.Rebuild,
		scipImport: o.SCIPImport, scipManifest: o.SCIPManifest})
}

func open(ctx context.Context, repo string, o openOptions) (*Workspace, error) {
	s, err := openStack(ctx, repo, o)
	if err != nil {
		return nil, err
	}
	coord, err := index.New(index.Options{
		Root:     s.root,
		Config:   s.cfg,
		Store:    s.store,
		Registry: s.registry,
		CAS:      s.cas,
		Git:      s.git,
		Lock:     s.locker(),
		// The collector is composed at the end of openStack precisely so it can
		// be handed over here: the coordinator's post-activation path is the
		// only place in this process that holds both the cross-process
		// workspace lock and the indexing mutex a collection pass requires.
		Collector: s.collector,
		Ledger:    s.ledger,
		// The read-only side of the same file, so a finished run states in its
		// result what it did. It opens the ledger per call and never writes.
		RunLedgerReader: runLedger{dir: s.dataDir},
		Pool:            s.pool,
		Watcher:         s.watcher,
		States:          s.states,
		// The supplied `--scip-index` path reaches the SCIP provider at
		// composition time and is invisible to the coordinator that writes the
		// generation row, so it is handed over here as well. The scope key is
		// spelled by the provider that reads it; internal/index only stores it.
		SuppliedIndexes: suppliedIndexes(o.scipImport),
		Logger:          s.logger,
		Now:             time.Now,
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	if err := s.openQueries(coord.Repository()); err != nil {
		coord.Close()
		s.Close()
		return nil, err
	}
	status, err := coord.StatusReader(s.queryStore)
	if err != nil {
		coord.Close()
		s.Close()
		return nil, err
	}
	w := &Workspace{s: s, coord: coord, status: status}
	// The compiler is composed last because its graph factory is w.Query, so
	// it cannot exist before the workspace it queries through.
	if err := s.openCompiler(w.Query); err != nil {
		coord.Close()
		s.Close()
		return nil, err
	}
	// The workflow service is composed after the compiler because Include
	// recompiles through it, and eagerly because every one of its bounds is a
	// wiring fact: a non-positive one is a composition defect that must fail
	// here rather than on the first `codectx context ...` call.
	if err := s.openWorkflow(); err != nil {
		coord.Close()
		s.Close()
		return nil, err
	}
	return w, nil
}

// Coordinator is the index coordinator over this workspace.
func (w *Workspace) Coordinator() *index.Coordinator { return w.coord }

// Spans registers fn to be called with every stage of this workspace's run as
// it finishes. It is how a command prints progressive lines and how the MCP
// server reports progress, and it is the ONLY way to reach the composed run
// ledger's stream: a caller that opened its own ledger would be a second
// writer on a file whose whole design rests on there being one, and would
// contend with the run it is reporting on.
//
// fn runs on a goroutine of the workspace's own, fed by a bounded queue the
// collector never waits on, so a slow subscriber costs rows and never paces
// the run being measured. A workspace that composed no ledger -- a report
// holds no lock and records nothing -- registers nothing and calls fn never.
func (w *Workspace) Spans(fn func(model.StageRecord)) { w.s.spans.subscribe(fn) }

// IndexRunID is the identifier of the indexing run this workspace is recording
// at this moment, and false when none is open. It is how a surface that
// follows one run -- the MCP progress notifications of a single call -- tells
// this run's stages from the stages of the per-process overlay run a language
// server's start opens, which can be recorded concurrently with it.
//
// A workspace that composed no ledger records nothing and answers false, so a
// follower of it counts nothing rather than counting everything.
func (w *Workspace) IndexRunID() (string, bool) { return w.s.ledger.IndexRunID() }

// Resolver is the managed-toolchain resolver this workspace resolves analyzers
// through, and ToolStore is the store it reads. A report renders them; nothing
// else reaches for a tool outside a provider.
func (w *Workspace) Resolver() *toolchain.Resolver { return w.s.resolver }

// ToolStore is the absolute path of the tool store the resolver reads.
func (w *Workspace) ToolStore() string { return w.s.toolDir }

// DataDir is the cache this workspace actually opened. It is the configured
// data directory, or the sibling `index --rebuild` created, which is the one
// thing a caller cannot derive from the configuration alone.
func (w *Workspace) DataDir() string { return w.s.dataDir }

// Config is the configuration this workspace actually opened with, by value:
// the resolved file plus whatever the open itself settled, which is why
// Storage.DataDir here is the cache that was opened rather than the configured
// one. A caller that needs the configuration of an open workspace reads it
// here instead of loading the files a second time and risking a second answer.
func (w *Workspace) Config() config.Config { return w.s.cfg }

// Query pins gen -- zero selects the active generation -- and builds the graph
// engine bound to it. The returned closer releases the reader's QUERY lease and
// must be called once, whatever the query returns; the engine must not be used
// after it.
//
// A continuation token never names that lease. The engine is given the stack's
// lease store and mints a cursor-scoped lease per token, because the query
// lease is gone the moment this request returns and the next CLI invocation
// would find the spool it names already released.
//
// The engine is built per request, which is why the concurrency gate it is
// given is the stack's process-scoped one. The coordinator is passed as the
// promoter only when this workspace holds the indexing lock: Coordinator.Promote
// requires it, so a report promotes nothing and reports the deferred capability
// rows instead (Section 11.6).
func (w *Workspace) Query(ctx context.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
	return w.query(ctx, gen, false)
}

// query is Query with the choice of handle made explicit. readOnly selects the
// composition's reader handle, which in a modeServe process is a second handle
// on the same database with no writer connection: the pin takes no retention
// lease and reaches no write transaction, so a tool call cannot commit or wait
// on the ingestion group this process's own refresh has open.
//
// A read-only engine is given NO lease store, which is the engine's own
// documented "continuations are not offered" ending: every mint site returns an
// empty token and the answer is reported truncated. Handing it the writer's
// lease store instead would put the cursor lease -- a write -- back on the very
// transaction this handle exists to stay off, and handing it a lease store over
// the reader would fail the page rather than end it.
func (w *Workspace) query(ctx context.Context, gen model.GenerationID, readOnly bool) (*graph.Engine, func() error, error) {
	store, leases, mayPromote := w.s.store, w.s.leases, w.s.lock != nil
	if readOnly && w.s.queryStore != w.s.store {
		// The reader handle, no lease store, and no promoter: promotion builds
		// a deferred capability, which is a write. A read tool in a serving
		// process reports the deferred rows exactly as a report does.
		store, leases, mayPromote = w.s.queryStore, nil, false
	}
	reader, err := store.PinGeneration(ctx, w.s.repo, gen, w.s.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return nil, nil, err
	}
	var promote graph.Promoter
	if mayPromote {
		promote = promoter{coord: w.coord}
	}
	// The packed per-generation adjacency is the structure every traversal
	// reads (ADR-0005); it is opened on the same pinned generation, so the
	// engine's structural and delivery reads describe one instant.
	packed, err := sqlite.NewGraphReader(ctx, reader)
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	engine, err := graph.New(graph.Options{
		Adjacency: adjacency{reader: reader},
		Reader:    packed,
		Promoter:  promote,
		Signer:    w.s.signer,
		Spools:    w.s.spools,
		Leases:    leases,
		Gate:      w.s.gate,
		Limits:    graphLimits(w.s.cfg),
		Now:       time.Now,
	})
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	return engine, reader.Close, nil
}

// Compile compiles one immutable context manifest over this workspace
// (Section 15). It is the single entry point to the context compiler: the
// compiler pins its own generation per request, so this needs no workspace lock
// and answers in a report as it does in an indexing session.
//
// The manifest it returns is the header; the entries, slices and exclusions are
// read back through the store's manifest pages, which is the surface the
// coverage session of Task 16 walks.
func (w *Workspace) Compile(ctx context.Context, req model.ContextRequest) (model.ContextManifest, error) {
	return w.s.compiler.Compile(ctx, req)
}

// CompilePage is the CONTINUABLE form of Compile (ruling C7): a compile that
// runs out of query deadline ends the pass it is in rather than the answer, and
// reports the token the next call resumes from. An empty cursor starts a fresh
// compile.
//
// Compile is kept beside it and not reimplemented in terms of this one: its
// callers ask a question that has no continuation to hand back, and for them a
// deadline must stay the error it always was.
func (w *Workspace) CompilePage(ctx context.Context, req model.ContextRequest, cursor string) (contextpkg.CompileResult, error) {
	return w.s.compiler.CompilePage(ctx, req, cursor)
}

// Close releases the coordinator and then everything below it, in reverse.
// Both halves run even when the first fails: the workspace lock must be
// released whatever else went wrong.
func (w *Workspace) Close() error {
	if w == nil {
		return nil
	}
	var first error
	if w.coord != nil {
		first = w.coord.Close()
	}
	if err := w.s.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// maxAmbiguousCandidates bounds the candidate list an ambiguous name is
// rejected with (ruling Q9). It is an app-internal presentation bound, not a
// new protocol limit: the answer is the rejection, and a list long enough to
// flood a terminal would not help the operator disambiguate.
const maxAmbiguousCandidates = 16

// ResolveNodes turns the `<name-or-id>` arguments of a query command into
// resolved NodeIDs against one pinned generation.
//
// An argument that is already a resolved id is passed through untouched, so an
// id-taking caller pays for no lookup. A name is resolved through
// PinnedReader.Nodes -- storage, not search, which is what keeps Section 30.1's
// "graph accepts resolved IDs and does not depend on search" true of this path
// as well. Exact `name` is tried first and exact `qualified_name` second, so an
// unqualified spelling wins where it is unique and a fully qualified spelling
// still resolves when the short name is not.
//
// More than one candidate is CTX_ARGUMENT_INVALID naming at most
// maxAmbiguousCandidates of them. Picking the first silently is the one thing
// this must never do: the candidates are different symbols, and answering about
// the wrong one is a wrong answer the caller cannot detect.
func (w *Workspace) ResolveNodes(ctx context.Context, gen model.GenerationID, names []string) ([]model.NodeID, error) {
	if len(names) == 0 {
		return nil, nil
	}
	resolved := make([]model.NodeID, len(names))
	var reader *sqlite.PinnedReader
	defer func() {
		if reader != nil {
			reader.Close()
		}
	}()
	for i, name := range names {
		if model.ValidHexID(name) {
			resolved[i] = model.NodeID(name)
			continue
		}
		if reader == nil {
			// The generation is pinned once, and only when a name actually
			// needs looking up, so an all-ids invocation opens no lease.
			r, err := w.s.store.PinGeneration(ctx, w.s.repo, gen, w.s.cfg.Storage.QueryCursorTTL.Std())
			if err != nil {
				return nil, err
			}
			reader = r
		}
		id, err := resolveOneName(ctx, reader, name)
		if err != nil {
			return nil, err
		}
		resolved[i] = id
	}
	return resolved, nil
}

// resolveOneName resolves a single name against the pinned reader. It asks for
// one more candidate than it will list, so "exactly the bound" and "more than
// the bound" are distinguishable and the rejection can say which.
func resolveOneName(ctx context.Context, reader *sqlite.PinnedReader, name string) (model.NodeID, error) {
	candidates, err := reader.Nodes(ctx, sqlite.NodeFilter{Name: name}, "", maxAmbiguousCandidates+1)
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		candidates, err = reader.Nodes(ctx, sqlite.NodeFilter{QualifiedName: name}, "", maxAmbiguousCandidates+1)
		if err != nil {
			return "", err
		}
	}
	switch {
	case len(candidates) == 0:
		return "", (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "no node in this generation carries that name"}).
			WithDetail("name", name).
			WithRemediation("run `codectx symbol` to find the symbol, or pass its resolved node id")
	case len(candidates) == 1:
		return candidates[0].Node.ID, nil
	}
	return "", ambiguousName(name, candidates)
}

// ambiguousName builds the rejection for a name several nodes carry. The
// candidates ride in the remediation text rather than in the details map
// because they are what the operator reads to choose one, and each is rendered
// as its qualified name plus a short id prefix -- enough to tell the candidates
// apart and to paste back, without printing sixteen 64-character ids.
func ambiguousName(name string, candidates []sqlite.StoredNode) error {
	shown := candidates
	more := false
	if len(shown) > maxAmbiguousCandidates {
		shown, more = shown[:maxAmbiguousCandidates], true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d nodes carry this name", len(candidates))
	if more {
		fmt.Fprintf(&b, " (showing the first %d)", maxAmbiguousCandidates)
	}
	b.WriteString("; pass one of these node ids: ")
	for i, c := range shown {
		if i > 0 {
			b.WriteString(", ")
		}
		label := c.Node.QualifiedName
		if label == "" {
			label = c.Node.Name
		}
		fmt.Fprintf(&b, "%s %s…", label, string(c.Node.ID)[:12])
	}
	return (&model.Error{Code: model.CodeArgumentInvalid,
		Message: "this name resolves to more than one node"}).
		WithDetail("name", name).
		WithDetail("candidates", strconv.Itoa(len(candidates))).
		WithRemediation(b.String())
}

// suppliedIndexes turns the composition-time `--scip-index` path into the
// record the coordinator writes against each published generation. An empty
// path is no supplied index and records nothing, which is the honest answer:
// the record must distinguish "none supplied" from "supplied and unresolved",
// so a run that supplied none must leave no row.
func suppliedIndexes(scipImport string) []index.SuppliedIndex {
	if scipImport == "" {
		return nil
	}
	return []index.SuppliedIndex{{Path: scipImport, ProviderID: scip.ID, ScopeKey: scip.ImportScope(scipImport)}}
}
