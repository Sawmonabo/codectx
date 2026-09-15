package graph

import "github.com/Sawmonabo/codectx/internal/model"

// consumerReader is the pinned packed-adjacency reader the rollup, the
// repository map, the reference walk and the path search read structure
// through (ADR-0005 Decision 1). It is a method rather than a bare field read
// because Options.Reader is still optional while the remaining traversals move
// onto the port: an engine wired without one must say so in the operator's
// vocabulary rather than panic on a nil interface halfway through a walk.
//
// It is deliberately NOT named for the walk's own reader accessor: this is the
// consumers' seam, and the two lanes that own the two halves must be able to
// land independently.
func (e *Engine) consumerReader() (GraphReader, error) {
	if e.reader == nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message:     "this workspace was opened without the packed adjacency reader every traversal reads structure through",
			Remediation: "this is a wiring defect; report it with the command you ran"}
	}
	return e.reader, nil
}
