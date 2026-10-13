package sqlite

import "github.com/Sawmonabo/codectx/internal/model"

// TierPageStatement is the statement of one WalkTier page, for a plan test:
// from the tier's start when unit is 0, otherwise after the index entry
// (key, unit, node).
func TierPageStatement(r *PinnedReader, f NodeFilter, key string, unit, node int64, limit int) (string, []any, error) {
	var after *tierKey
	if unit != 0 {
		after = &tierKey{key: key, unit: unit, node: node}
	}
	return r.tierPageStatement(f, after, limit)
}

// NodesStatement is the statement of one Nodes page, for a plan test.
func NodesStatement(r *PinnedReader, f NodeFilter, after string, limit int) (string, []any, error) {
	return r.nodesStatement(f, model.NodeID(after), limit)
}
