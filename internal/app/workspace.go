package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/coverage"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Workspace is one opened workspace: the composed stack plus the index
// coordinator over it. Opened for indexing it is the single cross-process owner
// of Section 13.2 -- the workspace lock is held for its whole lifetime -- so
// exactly one of `index`, `refresh`, `watch` and the MCP server builds at a
// time and a second caller is told the workspace is busy rather than becoming a
// second writer. Opened for a report it holds no lock and can build nothing.
type Workspace struct {
	s     *stack
	coord *index.Coordinator
}

// OpenWorkspace composes the workspace for a run that will index. wait is how
// long it waits for the workspace lock before reporting CTX_WORKSPACE_BUSY;
// wait <= 0 tries once. rebuild opens an explicitly requested new cache beside
// the configured one and leaves the existing database untouched (Section 12.2).
//
// A payload an admitted unit needs is installed on demand, by the unit
// (Section 11.7): an analyzer that is pinned but not yet downloaded is a fetch
// at unit time, not a missing capability and not a cost this call pays.
func OpenWorkspace(ctx context.Context, repo string, wait time.Duration, rebuild bool) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeIndex, wait: wait, rebuild: rebuild})
}

// OpenWorkspaceForReport composes the workspace for a read-only report. It
// takes no workspace lock, mutates no generation and installs nothing -- it
// creates only the cache's own directories -- so a report answers while
// another process indexes or watches.
func OpenWorkspaceForReport(ctx context.Context, repo string) (*Workspace, error) {
	return open(ctx, repo, openOptions{mode: modeReport})
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
		Lock:     s.lock,
		Pool:     s.pool,
		Watcher:  s.watcher,
		States:   s.states,
		Logger:   s.logger,
		Now:      time.Now,
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
	w := &Workspace{s: s, coord: coord}
	// The compiler is composed last because its graph factory is w.Query, so
	// it cannot exist before the workspace it queries through.
	if err := s.openCompiler(w.Query); err != nil {
		coord.Close()
		s.Close()
		return nil, err
	}
	return w, nil
}

// Coordinator is the index coordinator over this workspace.
func (w *Workspace) Coordinator() *index.Coordinator { return w.coord }

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

// Search answers the discovery endpoints over this workspace: exact symbol and
// path lookup, and generation-local lexical retrieval. It pins a generation per
// request, so it needs no workspace lock and answers in a report as it does in
// an indexing session.
func (w *Workspace) Search() *search.Service { return w.s.search }

// Coverage answers the Section 16 source-read endpoints over this workspace:
// it opens actor-scoped sessions over a persisted context manifest, serves
// snapshot-pinned source in bounded chunks and keeps receipt-confirmed coverage
// of what was actually delivered. Like Search it pins what it reads per
// session, so it needs no workspace lock and answers in a report.
func (w *Workspace) Coverage() *coverage.Service { return w.s.coverage }

// Query pins gen -- zero selects the active generation -- and builds the graph
// engine bound to it. The returned closer releases the retention lease and must
// be called once, whatever the query returns; the engine must not be used after
// it.
//
// The engine is built per request, which is why the concurrency gate it is
// given is the stack's process-scoped one. The coordinator is passed as the
// promoter only when this workspace holds the indexing lock: Coordinator.Promote
// requires it, so a report promotes nothing and reports the deferred capability
// rows instead (Section 11.6).
func (w *Workspace) Query(ctx context.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
	reader, err := w.s.store.PinGeneration(ctx, w.s.repo, gen, w.s.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return nil, nil, err
	}
	var promote graph.Promoter
	if w.s.lock != nil {
		promote = promoter{coord: w.coord}
	}
	engine, err := graph.New(graph.Options{
		Adjacency: adjacency{reader: reader},
		Promoter:  promote,
		Signer:    w.s.signer,
		Spools:    w.s.spools,
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
