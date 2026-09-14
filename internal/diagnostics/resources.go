package diagnostics

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Resources is the Section 23 accounting block `status --resources` and
// `codectx_index_status` report. L2 assembles it from the Sampler, the store
// reader and the resources configuration block, keeping every unmeasurable
// figure nil.
func (s *Service) Resources(_ context.Context) (model.ResourceReport, error) {
	return model.ResourceReport{}, notImplemented("resources", "L2")
}
