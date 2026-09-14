package diagnostics

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Doctor runs the Section 22 check list. L1 implements it against the frozen
// interfaces: the expensive integrity and parser smoke checks belong to
// req.Deep alone, and remediation is generated from the typed code, never from
// an analyzer's text.
func (s *Service) Doctor(_ context.Context, req model.DoctorRequest) (model.DoctorReport, error) {
	if err := req.Validate(); err != nil {
		return model.DoctorReport{}, err
	}
	return model.DoctorReport{}, notImplemented("doctor", "L1")
}
