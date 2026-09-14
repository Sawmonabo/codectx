package toolchain

import (
	"context"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ResolveInstalled is Resolve's store path with the fetch branch refused: it
// reports the payload only when the store already holds it in a verified
// state. A provider constructs from installed payloads and fetches only when a
// unit that needs one actually runs, so a machine that has not run
// `codectx tools prefetch` does not install every language's indexer the first
// time a provider is built (Section 11.7).
//
// The second result is "the store holds it": false with a nil error is "not
// installed here", which is not a failure. Every other condition -- an
// unsupported platform, a corrupt store entry, an invalid override -- stays a
// typed CTX_TOOL_* error, because those are not repaired by running the tool
// later.
//
// A resolver that is itself configured offline (`tools.offline = true`) keeps
// reporting CTX_TOOL_OFFLINE as an error rather than as honest absence: there
// the refusal is the user's standing instruction, a later fetch will not
// happen either, and a caller that treated it as "will be fetched on demand"
// would plan work that can never run.
//
// Resolution runs against a copy of the resolver with the fetch disabled, not
// by mutating the receiver: a Resolver is shared by concurrent callers and
// holds no mutable state, which is exactly the property a temporary flip would
// destroy. The copy also carries the flag into the runtime resolution Resolve
// performs one level deep, so a payload whose managed runtime is absent is
// reported as not installed rather than fetching the runtime.
func (r *Resolver) ResolveInstalled(ctx context.Context, name string) (Tool, bool, error) {
	installedOnly := *r
	installedOnly.offline = true
	t, err := installedOnly.Resolve(ctx, name)
	if err == nil {
		return t, true, nil
	}
	var typed *model.Error
	if !r.offline && errors.As(err, &typed) && typed.Code == model.CodeToolOffline {
		return Tool{}, false, nil
	}
	return Tool{}, false, err
}
