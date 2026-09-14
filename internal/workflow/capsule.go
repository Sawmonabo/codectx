package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Capsule pages one projection of a session's sealed capsule (Section 17.3).
// Owned by L5.
func (s *Service) Capsule(ctx context.Context, req model.CapsuleRequest) (model.CapsulePage, error) {
	return model.CapsulePage{}, errUnimplemented("Capsule")
}

// Export returns the whole sealed capsule for one session. Owned by L5.
func (s *Service) Export(ctx context.Context, req model.SessionRequest) (model.Capsule, error) {
	return model.Capsule{}, errUnimplemented("Export")
}

// buildCapsule assembles the capsule from stored observations, coverage and
// waivers only -- never from a live recomputation, which is what makes the
// identity reproducible. The caller then hashes it, writes it through the
// write-once PutCapsule and compares the returned CanonicalHash with the
// computed one, reporting CTX_VERSION_CONFLICT on a mismatch rather than
// recomputing and overwriting. Owned by L5.
func (s *Service) buildCapsule(ctx context.Context, rec sqlite.SessionRecord, g gate) (model.Capsule, error) {
	return model.Capsule{}, errUnimplemented("buildCapsule")
}

// canonicalCapsuleHash derives the Section 17.3 capsule identity over
// canonicalCapsuleDomain. Timestamps stay in the stored record and out of the
// preimage, so the same inputs hash identically across two builds. Owned by L5.
func canonicalCapsuleHash(c model.Capsule) string {
	return ""
}
