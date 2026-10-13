package sqlite

import "github.com/Sawmonabo/codectx/internal/model"

// NodesAmongStatement is the statement NodesAmong runs, for a plan test.
func NodesAmongStatement(r *PinnedReader, f NodeFilter, ids []model.NodeID) (string, []any, error) {
	return r.nodesAmongStatement(f, ids)
}

// CountNodesStatement is the statement CountNodes runs, for a plan test.
func CountNodesStatement(r *PinnedReader, f NodeFilter) (string, []any, error) {
	return r.countNodesStatement(f)
}
