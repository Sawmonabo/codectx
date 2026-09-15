package app

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/index"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// graphGate is the process-scoped `resources.max_concurrent_graph_queries`
// semaphore. It is owned here and not by the engine because Workspace.Query
// builds one Engine per request: a per-Engine semaphore would gate nothing.
//
// Release returns the slot Acquire took and must be called exactly once per
// successful Acquire; a Release without one blocks, which is the loud failure
// an over-released semaphore deserves rather than a silently widened bound.
type graphGate struct {
	slots chan struct{}
}

// newGraphGate builds the gate with n concurrent slots. n below one is raised
// to one: a configuration that admitted no query at all would refuse every
// graph request with a resource limit nobody can satisfy.
func newGraphGate(n int) *graphGate {
	return &graphGate{slots: make(chan struct{}, maxInt(1, n))}
}

// Acquire takes a slot, waiting no longer than the request's own deadline.
// A free slot is taken without consulting the context, so an expired deadline
// is reported by the operation that actually does the work rather than turning
// an uncontended query into a resource limit; only genuine waiting past the
// deadline is CTX_RESOURCE_LIMIT.
func (g *graphGate) Acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
		return nil
	default:
	}
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return &model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message:     "every graph query slot was busy for the whole request deadline",
			Remediation: "retry when fewer queries are running, or raise resources.max_concurrent_graph_queries"}
	}
}

// Release returns the slot taken by a successful Acquire.
func (g *graphGate) Release() { <-g.slots }

// adjacency reads the traversal's facts from one pinned generation. It exists
// so internal/graph depends on its own port rather than on the store, and it is
// batched throughout: a per-node reader would turn every expansion step into
// N+1 queries against the relation indexes.
type adjacency struct {
	reader *sqlite.PinnedReader
}

func (a adjacency) Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	return a.reader.EdgesBatch(ctx, nodes, direction, kinds, after, limit)
}

// NodesByID drops the storage-only fields of a StoredNode: the engine hydrates
// the nodes a traversal admitted and has no use for their unit or byte range.
func (a adjacency) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	stored, err := a.reader.NodesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	nodes := make([]model.Node, 0, len(stored))
	for _, s := range stored {
		nodes = append(nodes, s.Node)
	}
	return nodes, nil
}

func (a adjacency) EvidenceFor(ctx context.Context, relations []model.RelationID,
	limit int) (map[model.RelationID][]model.EvidenceID, error) {
	// Storage hydrates whole evidence rows; traversal only needs their
	// identities and hydrates the rows it actually returns per page.
	stored, err := a.reader.EvidenceBatch(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	ids := make(map[model.RelationID][]model.EvidenceID, len(stored))
	for rel, rows := range stored {
		out := make([]model.EvidenceID, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.Evidence.ID)
		}
		ids[rel] = out
	}
	return ids, nil
}

// EvidenceRows is graph.EvidenceRowReader: whole evidence rows, so a reference
// occurrence carries the precision class, file and byte range that make it
// checkable against the source. EvidenceFor above returns the identities a
// traversal needs; this returns the rows an occurrence needs, from the same
// batched read.
func (a adjacency) EvidenceRows(ctx context.Context, relations []model.RelationID,
	limit int) (map[model.RelationID][]model.Evidence, error) {
	stored, err := a.reader.EvidenceBatch(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	rows := make(map[model.RelationID][]model.Evidence, len(stored))
	for rel, list := range stored {
		out := make([]model.Evidence, 0, len(list))
		for _, row := range list {
			// The byte interval is carried across, not dropped: it is the only
			// location a sealed occurrence has. StoredEvidence keeps it beside
			// the fact because the table stores offsets and no line or column,
			// and an occurrence whose location stopped at the file id cannot be
			// checked against the source, which is what it exists for.
			ev := row.Evidence
			ev.Bytes = row.Bytes
			out = append(out, ev)
		}
		rows[rel] = out
	}
	return rows, nil
}

// Containers is graph.ContainerReader, the OPTIONAL enumeration seam the
// repository map needs: every other method here reads facts hanging off node
// ids the caller already holds, and a map has no such seed. It drops the
// storage-only fields for the same reason NodesByID does.
//
// The value receiver is load-bearing. internal/graph discovers this seam by
// type-asserting the Adjacency it was handed, and a pointer receiver would drop
// the method from adjacency's method set: the assertion would fail at run time
// and Overview would refuse every request with a wiring error. The compile-time
// assertion below is what catches that instead.
func (a adjacency) Containers(ctx context.Context, kinds []model.NodeKind,
	after model.NodeID, limit int) ([]model.Node, error) {
	stored, err := a.reader.Containers(ctx, kinds, after, limit)
	if err != nil {
		return nil, err
	}
	nodes := make([]model.Node, 0, len(stored))
	for _, s := range stored {
		nodes = append(nodes, s.Node)
	}
	return nodes, nil
}

// The seam is asserted here rather than at the point of use, so a receiver or
// signature that drifts fails this package's build instead of silently
// disabling the repository map at run time.
var _ graph.ContainerReader = adjacency{}

func (a adjacency) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	return a.reader.Capabilities(ctx)
}

func (a adjacency) Binding() model.Binding { return a.reader.Binding() }

// promoter adapts the index coordinator to the engine's Promoter port, which
// declares the three pending fields locally so internal/graph never imports the
// coordinator package. It is installed only in index mode: Coordinator.Promote
// requires the workspace indexing lock and a report holds none.
type promoter struct {
	coord *index.Coordinator
}

func (p promoter) Promote(ctx context.Context, providerID, scopeKey string) (graph.PendingUnits, error) {
	pending, err := p.coord.Promote(ctx, providerID, scopeKey)
	if err != nil {
		return graph.PendingUnits{}, err
	}
	return graph.PendingUnits{Units: pending.Units, Position: pending.Position, Estimate: pending.Estimate}, nil
}

// graphLimits resolves the traversal budget from the configuration once, so
// every engine this process builds holds the same bounds and the engine itself
// never reads configuration. A zero on a request field is resolved to these by
// the engine; a zero arriving here would be a configuration that validation
// already refuses.
func graphLimits(cfg config.Config) graph.Limits {
	return graph.Limits{
		MaxDepth:       cfg.Context.MaxGraphDepth,
		MaxVisited:     cfg.Context.MaxVisitedNodes,
		MaxEdges:       cfg.Context.MaxGraphEdges,
		MaxPageItems:   cfg.Resources.MaxPageItems,
		MaxReasonPaths: cfg.Context.MaxReasonPathsPerEntry,
		QueryTimeout:   cfg.Resources.QueryTimeout.Std(),
		CursorTTL:      cfg.Storage.QueryCursorTTL.Std(),
		FrontierBytes:  cfg.Resources.QueryMemoryBytes,
	}
}
