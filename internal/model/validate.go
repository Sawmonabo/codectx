package model

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Structural bounds enforced by this package. They are absolute contract
// ceilings, not the tunable resource policy of Section 20.1: configuration may
// lower an effective limit but never raise one past the value a stored column
// or a bounded response can hold. Sizes are byte counts, not rune counts.
const (
	MaxIdentifierBytes    = 256   // provider/capability/actor/policy identifiers
	MaxLanguageBytes      = 64    // language tag
	MaxNameBytes          = 512   // Node.Name and SearchUnit.Name
	MaxQualifiedNameBytes = 2048  // Node.QualifiedName
	MaxSignatureBytes     = 4096  // Node.Signature
	MaxPathBytes          = 4096  // normalized root-relative path
	MaxScopeKeyBytes      = 2048  // provider scope key and alias scope
	MaxNativeKeyBytes     = 2048  // provider-native symbol key
	MaxDetailBytes        = 1024  // Evidence.Detail, error details, remediation
	MaxReasonBytes        = 512   // one ranking or exclusion reason
	MaxNoteBytes          = 8192  // observation note
	MaxTaskBytes          = 8192  // context task text
	MaxMetadataBytes      = 8192  // Node.Metadata JSON payload
	MaxSearchBodyBytes    = 32768 // Section 11.2: source chunks are at most 32 KiB
	MaxTokenBytes         = 4096  // signed cursor or receipt token
	// MaxRawChunkBytes is the configured raw source maximum of Section 16.2.
	// Section 20.1's coverage.max_chunk_bytes may lower it, never raise it.
	MaxRawChunkBytes = 1048576
	// MaxChunkContentBytes bounds the encoded body of one chunk response.
	// Section 20.2 requires the raw chunk plus its worst-case wire encoding to
	// fit the source response budget, and base64 is the widest encoding this
	// package admits: ceil(MaxRawChunkBytes/3)*4.
	MaxChunkContentBytes = ((MaxRawChunkBytes + 2) / 3) * 4

	MaxPageItems               = 200 // Section 20.1 resources.max_page_items
	MaxQueryTextBytes          = 8192
	MaxFilterValues            = 64 // kinds/languages/paths/relations per request
	MaxSeeds                   = 64
	MaxStartNodes              = 64
	MaxReasonsPerEntry         = 8
	MaxReasonPathsPerEntry     = 3  // Section 20.1 context.max_reason_paths_per_entry
	MaxRelationsPerPath        = 64 // edges retained in one explanation path
	MaxAmbiguousCandidates     = 16 // Section 9.4 bounded may_refer_to candidates
	MaxCapabilityStates        = 256
	MaxReceiptsPerConfirmation = 16 // Section 20.1 coverage.max_receipts_per_confirmation
	// MaxCoverageFilesPerCapsule is a structural ceiling only: a capsule may
	// honestly record coverage for every file a context session could touch, and
	// Section 20.1's workspace.max_files tops out at 250000. The operative limit
	// is the configured max_capsule_bytes byte budget, which the workflow layer
	// enforces; this constant exists so the list is finite, not to size it.
	MaxCoverageFilesPerCapsule = 250000
	// MaxRecordsPerResult bounds any list a single result carries that is not
	// itself a Page: Section 6 requires an explicit finite bound on every
	// response, and Section 17.3 requires the capsule's lists to be bounded.
	MaxRecordsPerResult = 1000
	MaxEvidencePerFact  = 64 // Section 11.1: a fact carries bounded evidence
)

// maxSigned64 is the largest value SQLite stores in an INTEGER column. Section
// 9.3 requires every bound value to fit it.
const maxSigned64 = uint64(math.MaxInt64)

// truncateUTF8 cuts s to at most limit bytes without splitting a rune. It is the
// only place this package shortens a string to a byte ceiling; a second
// implementation would drift and could emit invalid UTF-8 on the wire.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// TruncateDetail bounds one capability detail value to MaxDetailBytes without
// splitting a rune, which is the rule every detail map obeys: a value is
// shortened rather than rejected, because the detail exists to explain a state
// and a missing explanation is worse than a clipped one. It is exported so a
// caller that builds its own detail map -- provider.Detection, which is not a
// CapabilityState -- enforces the same bound from the same implementation.
func TruncateDetail(value string) string {
	return truncateUTF8(value, MaxDetailBytes)
}

// TruncateField bounds one indexed field value to its byte ceiling without
// splitting a rune and reports the value's original length, so a caller can
// flag the truncation on the fact it publishes. It is the one helper every
// field ceiling goes through: a value that exceeds a storage bound is
// shortened and flagged, never a reason to refuse the fact or fail the unit
// that produced it, because a dropped unit answers nothing while a clipped
// name still answers most questions about it.
//
// The second result is the ORIGINAL byte length. It is greater than
// len(bounded) exactly when the value was cut, which is the condition a
// caller records alongside the original length.
//
// This is index-time, permanent truncation of a stored field. It is a
// different fact from a result page that was cut short by a limit and is
// completed by following a cursor, and the two must never share a flag: a
// caller that conflates them cannot answer whether it received everything.
func TruncateField(value string, max int) (bounded string, originalLen int) {
	return truncateUTF8(value, max), len(value)
}

// truncateForMessage keeps a rejected value out of an unbounded log line while
// staying useful. It never returns a partial UTF-8 sequence.
func truncateForMessage(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return truncateUTF8(s, limit) + "..."
}

// requireField rejects an empty value and enforces its byte ceiling.
func requireField(field, value string, max int) *Error {
	if value == "" {
		return invalid("%s is required", field)
	}
	return boundField(field, value, max)
}

// boundField enforces a byte ceiling on an optional value.
func boundField(field, value string, max int) *Error {
	if len(value) > max {
		return invalid("%s is %d bytes, limit %d", field, len(value), max)
	}
	return nil
}

// requireTrimmed rejects a value that is empty or only whitespace, matching the
// Section 12.2 `length(trim(...)) > 0` checks on actor IDs, notes and reasons.
func requireTrimmed(field, value string, max int) *Error {
	if strings.TrimSpace(value) == "" {
		return invalid("%s must not be empty or blank", field)
	}
	return boundField(field, value, max)
}

// requireID validates a mandatory public content-derived ID.
func requireID(field, id string) *Error {
	if id == "" {
		return invalid("%s is required", field)
	}
	return optionalID(field, id)
}

// optionalID validates an ID that may legitimately be absent.
func optionalID(field, id string) *Error {
	if id == "" {
		return nil
	}
	if !ValidHexID(id) {
		return invalid("%s %q is not %d lowercase hex characters", field, truncateForMessage(id), IDHexLen)
	}
	return nil
}

// boundCount enforces a bounded array length; Section 19.3 forbids unbounded
// request or response arrays.
func boundCount(field string, n, max int) *Error {
	if n > max {
		return invalid("%s has %d entries, limit %d", field, n, max)
	}
	return nil
}

// boundSigned64 enforces the Section 9.3 rule that an unsigned byte quantity
// must still fit the signed 64-bit integer SQLite stores.
func boundSigned64(field string, v uint64) *Error {
	if v > maxSigned64 {
		return invalid("%s %d exceeds the signed 64-bit range SQLite stores", field, v)
	}
	return nil
}

// requireNonNegative is the single non-negativity check in this package. It
// covers both of the shapes that need one, because they have the same rule:
//
//   - stored quantities (sizes, ordinals, counts), where the schema's `>= 0`
//     CHECK is the authority and zero is an ordinary value; and
//   - request bounds and generation selectors, where zero means "use the
//     configured or endpoint default" / "the active generation" per the
//     convention documented on PageRequest, and a negative value is a client
//     bug rather than a sentinel.
//
// In neither case does zero mean unlimited (Section 20.2).
func requireNonNegative(field string, v int64) *Error {
	if v < 0 {
		return invalid("%s must not be negative, got %d", field, v)
	}
	return nil
}

// indexed names one element of a bounded array in a rejection message.
func indexed(field string, i int) string {
	return field + "[" + strconv.Itoa(i) + "]"
}

// boundStrings bounds both the length of a filter list and each element.
func boundStrings(field string, values []string, maxCount, maxBytes int) *Error {
	if err := boundCount(field, len(values), maxCount); err != nil {
		return err
	}
	for i, v := range values {
		if err := requireField(indexed(field, i), v, maxBytes); err != nil {
			return err
		}
	}
	return nil
}
