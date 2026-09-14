package scip

import (
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The call-site join of Section 11.3.
//
// A call site is a syntax fact. SCIP cannot produce one: no indexer
// distinguishes a call-site reference from a reference to a function value,
// and none sets a write role (docs/research/08-scip-empirical-six-indexers.md,
// confirmed for all six). So the tree-sitter provider publishes each call site
// with the alias below on its syntactic callee node, this importer publishes
// the same alias on the compiler-resolved symbol every reference occurrence
// names, and the reconciler merges the two identities where the byte ranges
// are equal. The `calls` relation then carries a compiler-precision target
// while both evidence rows survive.
//
// The join is exact-range equality on one file version, so an alias this
// provider emits at a range where tree-sitter found no call site is inert: it
// aliases the symbol node to a key nothing else names. That is why the alias
// is emitted for every non-definition occurrence rather than for a guessed
// subset — guessing which reference is a call is exactly what SCIP cannot do.
//
// callsiteAlias is the one place the key is spelled. Section 11.3 defines the
// range as the **1-based inclusive** byte range of the callee identifier,
// while model.SourceRange is the zero-based half-open byte range of Section
// 9.3, so the conversion is start+1 and end. Worked example: the identifier
// `Bar` occupying zero-based bytes [34,37) of `pkg/a.go` is
//
//	scope `file:pkg/a.go`, key `callsite:pkg/a.go:35-37`
//
// Both providers must spell this byte for byte or the join silently produces
// nothing, so lane A6 and this lane derive it from the same paragraph.
func callsiteAlias(p string, rng *model.SourceRange) (scopeKey, nativeKey string, ok bool) {
	// A zero-width occurrence has no identifier to join on, and a range that
	// does not advance cannot be expressed as an inclusive range at all.
	if rng == nil || rng.End.Byte <= rng.Start.Byte {
		return "", "", false
	}
	scopeKey = fileScope(p)
	nativeKey = "callsite:" + p + ":" + strconv.FormatUint(rng.Start.Byte+1, 10) + "-" + strconv.FormatUint(rng.End.Byte, 10)
	// A path may be up to model.MaxPathBytes long while an alias scope and
	// native key are bounded at model.MaxScopeKeyBytes and
	// model.MaxNativeKeyBytes, so a long path can overflow either bound. An
	// over-long alias would fail model.NativeAlias.Validate and take the whole
	// unit down for one unjoinable key, so it is skipped and counted instead.
	if len(scopeKey) > model.MaxScopeKeyBytes || len(nativeKey) > model.MaxNativeKeyBytes {
		return "", "", false
	}
	return scopeKey, nativeKey, true
}

// fileScope is the file-local alias scope of Section 11.3 and ruling R9-1.
func fileScope(p string) string { return "file:" + p }
