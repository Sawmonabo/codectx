package sqlite

import "github.com/Sawmonabo/codectx/internal/model"

// TierWindowStatement is the statement of one WalkTier window, for a plan
// test: from the tier's start (or from) when after is nil, otherwise after the
// index entry (key, path, start, end, unit, node).
func TierWindowStatement(r *PinnedReader, f NodeFilter, from *TierSpan, after []any, size int) (string, []any, error) {
	w, err := r.newTierWalk(f, from)
	if err != nil {
		return "", nil, err
	}
	var k *tierKey
	if after != nil {
		k = &tierKey{key: after[0].(string), path: after[1].(string), start: after[2].(int64), end: after[3].(int64),
			unit: after[4].(int64), node: after[5].(int64)}
	}
	query, args := w.windowArgs(k, size)
	return query, args, nil
}

// TierProbeStatement is the statement that reads the rows of a window's nodes
// (given as a JSON array of node surrogates), for a plan test.
func TierProbeStatement(r *PinnedReader, f NodeFilter, nodes string) (string, []any, error) {
	w, err := r.newTierWalk(f, nil)
	if err != nil {
		return "", nil, err
	}
	return w.probe, append([]any{nodes}, w.match...), nil
}

// NodesStatement is the statement of one Nodes page, for a plan test.
func NodesStatement(r *PinnedReader, f NodeFilter, after string, limit int) (string, []any, error) {
	return r.nodesStatement(f, model.NodeID(after), limit)
}
