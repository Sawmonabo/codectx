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
// confirmed for all six). The structural provider finds every call site and
// stores it in its file's link table with the byte range of its callee token
// and its caller; the call linker binds the site from that table. This
// importer reads the same table for each changed document (callSites): a
// reference occurrence whose bytes are exactly a site's callee token is the
// compiler's resolution of that call, and joinCall publishes `calls` from the
// site's caller to the resolved symbol at compiler precision, keyed by the
// call-site key below, beside the occurrence's `references` edge. The
// linker's `calls` for the same site keeps its own evidence, so the call
// carries both rows: an equal target is one relation with two rows, a
// different one two relations from one caller at one key.
//
// The join is exact-range equality on one file version: the table and the
// index describe the same pinned bytes, and an occurrence over any other
// bytes than a callee token joins nothing.
//
// callsiteKey is where this importer spells the key. Section 11.3 defines the
// range as the **1-based inclusive** byte range of the callee identifier,
// while model.SourceRange is the zero-based half-open byte range of Section
// 9.3, so the conversion is start+1 and end. Worked example: the identifier
// `Bar` occupying zero-based bytes [34,37) of `pkg/a.go` is
//
//	callsite:pkg/a.go:35-37
//
// It must equal the linker's spelling byte for byte, so that both evidence
// rows of one call carry one key.
//
// ok is false only when the key does not fit: a path may be up to
// model.MaxPathBytes long while a native key is bounded at
// model.MaxNativeKeyBytes. The caller then counts the occurrence under
// callsite_keys_skipped.
func callsiteKey(p string, rng *model.SourceRange) (string, bool) {
	key := "callsite:" + p + ":" + strconv.FormatUint(rng.Start.Byte+1, 10) + "-" + strconv.FormatUint(rng.End.Byte, 10)
	if len(key) > model.MaxNativeKeyBytes {
		return "", false
	}
	return key, true
}

// fileScope is the file-local alias scope of Section 11.3.
func fileScope(p string) string { return "file:" + p }
