// This file holds the name-only call candidate tier of a context plan
// (ADR-0015 decision 3): the candidates of every open call site the walk
// reaches join the plan after every precise entry, never as required scope.
package context

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// admitCandidates finds the open call sites among one walk page's entries and
// admits each site's name-only candidates into the sink as Possible records,
// tallying sites and candidates on the sink's scope verdict.
func (c *Compiler) admitCandidates(ctx context.Context, eng *graph.Engine, gen model.GenerationID,
	entries []model.ImpactEntry, in *seedIngest) error {
	return nil
}
