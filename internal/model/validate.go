package model

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Structural bounds enforced by this package, not the tunable resource policy
// of Section 20.1: configuration may lower an effective limit but never raise
// one past the value a stored column or a bounded response can hold. Sizes are
// byte counts, not rune counts.
//
// They are not all enforced the same way. A ceiling on a value that is matched,
// joined or addressed by its exact bytes is a REJECTION; a ceiling on a
// descriptive or explanatory field is a storage width its producer shortens and
// flags, and this package admits an over-wide value rather than failing the unit
// or the answer that carries it. truncatingField is that classification.
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

	MaxPageItems      = 200 // Section 20.1 resources.max_page_items
	MaxQueryTextBytes = 8192
	MaxFilterValues   = 64 // kinds/languages/paths/relations per request
	MaxSeeds          = 64
	// MaxStartNodes bounds the start list of ONE externally supplied traversal
	// request (ImpactRequest, GraphRequest and the CLI that builds them), which
	// is a wire bound on untrusted input and not a bound on how wide a walk may
	// be. It is a structural ceiling only -- it exists so a request's start
	// list is finite, not to size it -- and it mirrors
	// MaxCoverageFilesPerCapsule's referent: a caller may honestly name every
	// file a single context session could touch.
	//
	// The operative bound on the context compiler's OWN start set is the
	// user-set context.max_start_nodes (unlimited by default), applied to the
	// folded seed stream in context/scope.go. This constant is deliberately far
	// wider than any seed set one task can produce, so it never cuts that set;
	// a start list that reached it would be an externally supplied request, and
	// boundCount reports it rather than trimming it.
	MaxStartNodes              = 250000
	MaxReasonsPerEntry         = 8
	MaxReasonPathsPerEntry     = 3  // Section 20.1 context.max_reason_paths_per_entry
	MaxRelationsPerPath        = 64 // edges retained in one explanation path
	MaxAmbiguousCandidates     = 16 // Section 9.4 bounded may_refer_to candidates
	MaxCapabilityStates        = 256
	MaxReceiptsPerConfirmation = 16 // Section 20.1 coverage.max_receipts_per_confirmation
	// MaxRecordsPerResult bounds ONE PAGE of any list a result carries that is
	// not itself a Page: Section 6 requires an explicit finite bound on every
	// response.
	//
	// It is a page width, never a ceiling on the ANSWER. A repository whose
	// impact set is 1284 packages has an impact answer of 1284 packages; the
	// producer owes the caller two pages and a cursor, and boundPage says so
	// when it does not. Refusing the answer for its size is the failure the
	// bound exists to prevent, not the bound.
	//
	// It does not apply to the capsule, whose lists are durable rows read a page
	// at a time and bounded only by the two user-set context.max_capsule_* keys,
	// unlimited by default.
	MaxRecordsPerResult = 1000
	// MaxEvidencePerFact is a wire and record ceiling on the evidence one
	// fact hands over in a single batch, not a bound on how much evidence a
	// fact HAS. At 64 it was the latter: a provider clipped a fact's 65th
	// occurrence under default settings and storage deleted the surplus rows,
	// so an identity referenced 200 times in one file reported 64 of them
	// however the caller asked. Nothing downstream needs the small value --
	// evidence is stored as rows and every read of it is paged (the keyset
	// page in storage, the caller's per-relation limit in the graph), so a
	// large ceiling costs a reader nothing.
	//
	// 65536 is chosen to be unreachable by a unit a real file produces: it
	// would take a file with more than 65536 occurrences of one single
	// identity to reach it. What actually bounds a batch's memory is the
	// user-set resources.max_provider_record_bytes and the sink's batch
	// reservation, both of which apply whatever this value is. Exceeding it
	// is still disclosed, never silent.
	MaxEvidencePerFact = 65536
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
//
// A value over its ceiling is a rejection only where shortening it would
// change what the value MEANS. Every other ceiling is a storage width, and a
// stored fact that is wider than its column is shortened by its producer
// through TruncateField and flagged in the record's `truncated_fields` meta --
// never a reason to fail the unit that produced it or the answer that carries
// it. Refusing there answers nothing; a clipped signature still answers most
// questions about the symbol it belongs to.
//
// truncatedFields below is the whole of that classification, keyed by the
// field's last name segment so that a new record type inherits the verdict its
// fields already carry.
func boundField(field, value string, max int) *Error {
	if len(value) <= max {
		return nil
	}
	if truncatingField(field) {
		return nil
	}
	return invalid("%s is %d bytes, limit %d", field, len(value), max)
}

// truncatedFields are the field names whose value is shortened and flagged by
// its producer instead of rejected here. They are keyed by NAME, not by
// ceiling: MaxNameBytes and MaxReasonBytes are both 512 and
// MaxQualifiedNameBytes, MaxScopeKeyBytes and MaxNativeKeyBytes are all 2048,
// so the ceiling does not separate a storage width from a join key.
//
// What is in the set: the descriptive fields of a stored fact (name,
// qualified_name, from_name, signature), the machine-readable particulars of a
// degradation (detail, details, remediation), and the explanatory lists an
// answer carries (reasons, warnings, notices, truncation_reason). None of them
// is matched, joined or addressed by its exact bytes, and every one of them
// sits on a record nobody can resubmit: refusing one fails the unit that
// produced it or the answer that carries it, and answers nothing.
//
// What is deliberately NOT in it, and why each is identity:
//
//   - id, version, provider_id, capability, actor_id, policy_version,
//     analysis_config_hash, action, code, diagnostic_code (MaxIdentifierBytes)
//     -- matched exactly; a reader switches on the code and joins on the id, so
//     a shortened one names a different thing or nothing.
//   - language (MaxLanguageBytes) -- an exactly matched tag; "typescrip" names
//     no language, and no legitimate tag approaches 64 bytes.
//   - path (MaxPathBytes) -- a truncated path names a different file, or none.
//   - scope_key (MaxScopeKeyBytes), native_key / strong_key / canonical_key
//     (MaxNativeKeyBytes) -- the keys unit reuse and symbol resolution join on.
//   - cursor, next_cursor, receipt (MaxTokenBytes) and read_chunk_response
//     content (MaxChunkContentBytes) -- signed tokens and wire-encoding
//     ceilings; a clipped cursor verifies as nothing.
//   - note, task, query, reason on a waiver or an exclusion -- an actor's own
//     text on a REQUEST. The caller is present and can resubmit, and silently
//     clipping an attestation or a query falsifies it rather than shortening
//     it. Rejecting a request is not the failure this rule is about.
var truncatedFields = map[string]bool{
	"name": true, "qualified_name": true, "from_name": true, "signature": true,
	"detail": true, "details": true, "remediation": true,
	"reasons": true, "warnings": true, "notices": true, "truncation_reason": true,
}

// fieldVerdictOverrides settles the three fields whose last name segment gives
// the wrong verdict, so the segment rule stays simple and the exceptions are
// visible rather than implied.
var fieldVerdictOverrides = map[string]bool{
	// A doctor check is addressed by its name -- an operator and a script both
	// match on it -- so it is an identifier that happens to be spelled "name".
	"doctor_check.name": false,
	// Both of these are answer-carried explanation, not the actor request text
	// the other `reason`-shaped fields are: nobody can resubmit the compiled
	// context an entry was excluded from, or the session status that names
	// which guarantee limited it.
	"session_status.guarantee_limit": true,
	"excluded_context_entry.reason":  true,
}

// truncatingField reports whether a value over its ceiling is shortened and
// flagged by its producer rather than rejected here. field is a dotted path
// such as "node.qualified_name" or "search_hit.reasons[2]"; the verdict is
// carried by its last segment unless fieldVerdictOverrides settles the whole
// path, so a new record type inherits the verdict its fields already carry.
func truncatingField(field string) bool {
	if i := strings.IndexByte(field, '['); i >= 0 {
		field = field[:i]
	}
	if verdict, ok := fieldVerdictOverrides[field]; ok {
		return verdict
	}
	if i := strings.LastIndexByte(field, '.'); i >= 0 {
		field = field[i+1:]
	}
	return truncatedFields[field]
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

// boundPage bounds ONE PAGE of a result list at MaxRecordsPerResult.
//
// Over-length here is a PRODUCER defect, not a client error and never a verdict
// on the size of the repository: the caller asked a question whose honest
// answer has more rows than one page holds, and the producer owed it a page
// plus a cursor. So this reports CodeInternal and names the obligation, rather
// than handing the caller a CTX_ARGUMENT_INVALID they can do nothing about --
// which is what refused a whole 1284-package impact answer on a 4019-file
// repository.
func boundPage(field string, n int) *Error {
	if n <= MaxRecordsPerResult {
		return nil
	}
	return &Error{Code: CodeInternal, Message: fmt.Sprintf(
		"%s carries %d records in one page, over the %d-record page width; "+
			"the producer must return one page and a cursor for the rest",
		field, n, MaxRecordsPerResult)}
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
