package graph

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Candidates returns one ranked page of the name-only candidates of an open
// call site, or of a direct lookup by name, shape, argument count and
// language, read from the generation r is pinned to (ADR-0015): the
// declarations of the same name, as the language compares names, that fit the
// site's shape and language family (and, in the statically typed families,
// its argument count), ordered by closeness, then by how few compatible
// definitions the name has, then by a stable key. Total is exact and nothing
// is cut at a count.
//
// The lookup is not implemented yet. It validates the request and then
// answers CTX_PROVIDER_UNAVAILABLE, so a caller reports candidates as
// unavailable rather than as none.
func Candidates(ctx context.Context, r GraphReader, req model.CandidateRequest) (model.CandidatePage, error) {
	if err := req.Validate(); err != nil {
		return model.CandidatePage{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.CandidatePage{}, err
	}
	return model.CandidatePage{}, &model.Error{
		Code:    model.CodeProviderUnavailable,
		Message: "name-only call candidates are not implemented yet",
	}
}
