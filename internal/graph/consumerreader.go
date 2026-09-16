package graph

import "github.com/Sawmonabo/codectx/internal/model"

// Reader is the pinned packed-adjacency reader every traversal reads structure
// through (ADR-0005 Decision 1): the rollup, the repository map, the reference
// walk, the path search, and the context compiler's relation-attribute pass.
//
// It is a method rather than a bare field read because Options.Reader is still
// optional while the remaining traversals move onto the port: an engine wired
// without one must say so in the operator's vocabulary rather than panic on a
// nil interface halfway through a walk.
func (e *Engine) Reader() (GraphReader, error) {
	if e.reader == nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message:     "this workspace was opened without the packed adjacency reader every traversal reads structure through",
			Remediation: "this is a wiring defect; report it with the command you ran"}
	}
	return e.reader, nil
}
