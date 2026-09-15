package model

// Impact analysis request and result shapes of Section 14.3. They live beside
// the traversal types in query.go rather than inside it because Task 14 owns
// this file exclusively; the limits, validators and vocabulary they reuse are
// the ones already declared in validate.go and facts.go. No new limit constant
// is introduced here: an impact answer is bounded by the same seed, filter,
// record and reason-path caps as every other query.

// ImpactRequest asks which entities a change to the seed nodes may affect. It
// carries the same explicit bounds as GraphRequest because impact is a bounded
// traversal with a ranking pass on top: a zero bound means "use the configured
// default", never "unlimited" (Section 20.1).
type ImpactRequest struct {
	GenerationID GenerationID   `json:"generation_id,omitempty"`
	Start        []NodeID       `json:"start"`
	Relations    []RelationKind `json:"relations,omitempty"`
	Direction    Direction      `json:"direction"`
	MaxDepth     int            `json:"max_depth"`
	MaxVisited   int            `json:"max_visited"`
	MaxEdges     int            `json:"max_edges"`
	Page         PageRequest    `json:"page"`
}

// Validate enforces bounded seeds, a known relation vocabulary, a known
// direction and nonnegative work budgets.
func (r ImpactRequest) Validate() error {
	if len(r.Start) == 0 {
		return invalid("impact.start is required")
	}
	if err := boundCount("impact.start", len(r.Start), MaxStartNodes); err != nil {
		return err
	}
	for i, id := range r.Start {
		if err := requireID(indexed("impact.start", i), string(id)); err != nil {
			return err
		}
	}
	if err := boundCount("impact.relations", len(r.Relations), MaxFilterValues); err != nil {
		return err
	}
	for i, k := range r.Relations {
		if !k.Valid() {
			return invalid("%s %q is not a known relation kind", indexed("impact.relations", i), truncateForMessage(string(k)))
		}
	}
	if !r.Direction.Valid() {
		return invalid("impact.direction %q is not a known direction", truncateForMessage(string(r.Direction)))
	}
	// Zero defers to the configured traversal caps; see PageRequest.
	for _, b := range []struct {
		field string
		value int
	}{
		{"impact.max_depth", r.MaxDepth},
		{"impact.max_visited", r.MaxVisited},
		{"impact.max_edges", r.MaxEdges},
	} {
		if err := requireNonNegative(b.field, int64(b.value)); err != nil {
			return err
		}
	}
	if err := r.Page.ValidatePinned("impact", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// ImpactResult is heterogeneous like GraphResult: the ranked affected entries
// are returned alongside the package rollup that summarises them and the
// traversal accounting Section 14.3 requires. Exhausting a hard budget is
// reported through Meta.Truncated rather than presented as exhaustive impact.
type ImpactResult struct {
	Meta         QueryMeta     `json:"meta"`
	Entries      []ImpactEntry `json:"entries"`
	Packages     []PackageEdge `json:"packages,omitempty"`
	VisitedCount int64         `json:"visited_count"`
	EdgeCount    int64         `json:"edge_count"`
}

// Validate enforces the result shape and its traversal accounting.
func (r ImpactResult) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if err := boundPage("impact_result.entries", len(r.Entries)); err != nil {
		return err
	}
	for _, e := range r.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	if err := boundPage("impact_result.packages", len(r.Packages)); err != nil {
		return err
	}
	for _, p := range r.Packages {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	if err := requireNonNegative("impact_result.visited_count", r.VisitedCount); err != nil {
		return err
	}
	if err := requireNonNegative("impact_result.edge_count", r.EdgeCount); err != nil {
		return err
	}
	return nil
}

// PackageEdge is one aggregated package-to-package dependency: the distinct
// (from-package, to-package) pair plus the counts that back it. Section 14.3
// forbids presenting a rollup as a precise symbol-level call, so the counts are
// carried explicitly and the endpoints are container nodes, never symbols.
type PackageEdge struct {
	FromNodeID    NodeID `json:"from_node_id"`
	ToNodeID      NodeID `json:"to_node_id"`
	FromPath      string `json:"from_path"`
	ToPath        string `json:"to_path"`
	PairCount     int64  `json:"pair_count"`
	EvidenceCount int64  `json:"evidence_count"`
}

// Validate enforces the rollup shape. A pair count of zero would mean the edge
// summarises nothing, so it is rejected; an evidence count of zero is legal
// because a container relation need not carry occurrence evidence.
func (e PackageEdge) Validate() error {
	if err := requireID("package_edge.from_node_id", string(e.FromNodeID)); err != nil {
		return err
	}
	if err := requireID("package_edge.to_node_id", string(e.ToNodeID)); err != nil {
		return err
	}
	if err := boundField("package_edge.from_path", e.FromPath, MaxPathBytes); err != nil {
		return err
	}
	if err := boundField("package_edge.to_path", e.ToPath, MaxPathBytes); err != nil {
		return err
	}
	if e.PairCount <= 0 {
		return invalid("package_edge.pair_count must be positive; a rollup edge summarises at least one pair")
	}
	if err := requireNonNegative("package_edge.evidence_count", e.EvidenceCount); err != nil {
		return err
	}
	return nil
}
