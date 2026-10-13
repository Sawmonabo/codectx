package sqlite

import "github.com/Sawmonabo/codectx/internal/model"

// TierPageStatement is the statement of one WalkTier page, for a plan test:
// from the tier's start (or from) when after is nil, otherwise after the index
// entry (key, path, start, end, unit, node).
func TierPageStatement(r *PinnedReader, f NodeFilter, from *TierSpan, after []any, limit int) (string, []any, error) {
	var k *tierKey
	if after != nil {
		k = &tierKey{key: after[0].(string), path: after[1].(string), start: after[2].(int64), end: after[3].(int64),
			unit: after[4].(int64), node: after[5].(int64)}
	}
	return r.tierPageStatement(f, from, k, limit)
}

// NodesStatement is the statement of one Nodes page, for a plan test.
func NodesStatement(r *PinnedReader, f NodeFilter, after string, limit int) (string, []any, error) {
	return r.nodesStatement(f, model.NodeID(after), limit)
}
