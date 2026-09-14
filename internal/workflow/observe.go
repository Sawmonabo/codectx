package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Record persists one immutable actor observation. It checks the request, the
// actor, the expected scope version and the per-kind reference rules, proves
// every citation was already confirmed served to this actor, validates a scope
// review against the eight required categories, derives the id through
// model.NewObservationID -- the one Section 17.2 preimage -- and writes through
// PutObservation. Owned by L3.
func (s *Service) Record(ctx context.Context, req model.ObservationRequest) (model.Observation, model.SessionStatus, error) {
	return model.Observation{}, model.SessionStatus{}, errUnimplemented("Record")
}

// Waive records one required-file waiver and reports the resulting status. A
// waiver is an honest admission that a required file was not read: it never
// grants strict readiness, and SessionStatus.Validate refuses the combination
// outright. Owned by L3 (ownership unassigned in the lane plan -- see report).
func (s *Service) Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
	return model.WaiverRecord{}, model.SessionStatus{}, errUnimplemented("Waive")
}

// currentReview returns this actor's scope review for the session's current
// scope version and manifest hash, or nil when none is current. A review from a
// superseded scope version is not current and does not satisfy the gate.
// Owned by L3.
func (s *Service) currentReview(ctx context.Context, rec sqlite.SessionRecord, manifestHash string) (*model.Observation, error) {
	return nil, errUnimplemented("currentReview")
}

// citationsServed proves every source citation on the given references names an
// interval already confirmed served to this actor at that content hash, through
// Sessions.RangeConfirmed. This is what stops a review from marking a file read
// without the coverage to back it. Owned by L3.
func (s *Service) citationsServed(ctx context.Context, rec sqlite.SessionRecord, refs []model.ClaimReference) error {
	return errUnimplemented("citationsServed")
}
