package context

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// explanationOf is the explanation of the impact answer res opened: read from
// its reasons and routes views through its answer token, or, when the engine
// served the whole answer inline and retained none, taken from res itself.
func explanationOf(ctx context.Context, eng *graph.Engine, req model.ImpactRequest,
	res model.ImpactResult) (*explanation, error) {
	if res.Answer != "" {
		return newExplanation(eng, req, res.Answer), nil
	}
	if res.Meta.NextCursor != "" {
		return nil, contextErr(ctx, &model.Error{Code: model.CodeInternal,
			Message: "context: the impact answer was not retained, so its entries cannot be explained"})
	}
	x := newExplanation(eng, req, "")
	x.reasons.buf, x.reasons.done = res.Reasons, true
	x.routes.buf, x.routes.done = res.Routes, true
	return x, nil
}
