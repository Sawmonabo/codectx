package link

import "context"

// types is the flow-based type inference of one ECMAScript or Python unit:
// the types a site's receiver may hold, propagated from what the worker
// recovered inside each function (wire.Type) through assignments, parameters
// and returns across the unit's functions and files, and the function values
// field-based resolution reads.
type types struct{}

// inferTypes runs the inference over the unit's tables.
func inferTypes(ctx context.Context, u *unit, h *hierarchy) (*types, error) { return &types{}, nil }

// receivers yields the type declarations the receiver of site may hold, each
// once.
func (t *types) receivers(ctx context.Context, site Ref, yield func(typ Ref) error) error { return nil }

// functions yields the function declarations the called value of site may
// hold: a function value that flowed to it, or one written to the field it
// calls through, each once with the technique that found it.
func (t *types) functions(ctx context.Context, site Ref, yield func(target Ref, detail string) error) error {
	return nil
}

// close releases what the inference spilled.
func (t *types) close() error { return nil }
