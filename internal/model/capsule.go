package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// CapsuleList names one of the eight canonically ordered record lists a sealed
// capsule carries. It is the durable spelling: it is stored in
// context_capsule_rows.list, it orders the capsule digest, and the six pageable
// CapsuleView values are exactly the six of these a reader may project.
//
// Scope and the scope review ids are lists here but not views: they are hashed
// into the capsule identity and must be replayable from the rows, while the
// paged read surface Section 17.3 specifies exposes the other six.
type CapsuleList string

const (
	CapsuleListScope          CapsuleList = "scope"
	CapsuleListAcceptedFacts  CapsuleList = "accepted_facts"
	CapsuleListRejectedFacts  CapsuleList = "rejected_facts"
	CapsuleListContradictions CapsuleList = "contradictions"
	CapsuleListUnresolved     CapsuleList = "unresolved"
	CapsuleListScopeReviewIDs CapsuleList = "scope_review_ids"
	CapsuleListCoverage       CapsuleList = "coverage"
	CapsuleListWaivers        CapsuleList = "waivers"
)

// CapsuleListOrder is the fixed order the capsule identity absorbs its lists
// in, and the order a whole-capsule export streams them in. It is the order
// Section 17.3's digest has always used; changing it changes every sealed
// identity, so it is written once here rather than repeated per caller.
var CapsuleListOrder = [...]CapsuleList{
	CapsuleListScope,
	CapsuleListAcceptedFacts,
	CapsuleListRejectedFacts,
	CapsuleListContradictions,
	CapsuleListUnresolved,
	CapsuleListScopeReviewIDs,
	CapsuleListCoverage,
	CapsuleListWaivers,
}

// Valid reports whether the list is one of the eight.
func (l CapsuleList) Valid() bool {
	for _, known := range CapsuleListOrder {
		if l == known {
			return true
		}
	}
	return false
}

// List is the row list a capsule view projects. The two vocabularies are
// spelled identically on purpose; this method is the single place that says so,
// so a view added without a list fails here rather than paging an empty
// projection.
func (v CapsuleView) List() CapsuleList { return CapsuleList(v) }

// CapsuleCounts is how many records each of the capsule's eight lists holds.
// The sealed capsule blob carries these counts instead of the records
// themselves: the records live in context_capsule_rows and are read one page at
// a time, so a capsule of a repository-sized session is a small, fixed-size
// record and neither sealing nor exporting it holds a whole list in memory.
//
// The counts are not a summary of the rows; they are hashed into the capsule
// identity ahead of each list exactly as the length prefix always was, which is
// what lets a streamed digest be byte-identical to the whole-list digest.
type CapsuleCounts struct {
	Scope          int64 `json:"scope"`
	AcceptedFacts  int64 `json:"accepted_facts"`
	RejectedFacts  int64 `json:"rejected_facts"`
	Contradictions int64 `json:"contradictions"`
	Unresolved     int64 `json:"unresolved"`
	ScopeReviewIDs int64 `json:"scope_review_ids"`
	Coverage       int64 `json:"coverage"`
	Waivers        int64 `json:"waivers"`
}

// Of returns one list's count. An unknown list reports zero; callers reach it
// only through CapsuleListOrder or a validated view, both of which are closed.
func (c CapsuleCounts) Of(list CapsuleList) int64 {
	switch list {
	case CapsuleListScope:
		return c.Scope
	case CapsuleListAcceptedFacts:
		return c.AcceptedFacts
	case CapsuleListRejectedFacts:
		return c.RejectedFacts
	case CapsuleListContradictions:
		return c.Contradictions
	case CapsuleListUnresolved:
		return c.Unresolved
	case CapsuleListScopeReviewIDs:
		return c.ScopeReviewIDs
	case CapsuleListCoverage:
		return c.Coverage
	case CapsuleListWaivers:
		return c.Waivers
	}
	return 0
}

// Set records one list's count, so a sealing pass that counts the eight lists
// in a loop does not have to name the eight fields.
func (c *CapsuleCounts) Set(list CapsuleList, n int64) {
	switch list {
	case CapsuleListScope:
		c.Scope = n
	case CapsuleListAcceptedFacts:
		c.AcceptedFacts = n
	case CapsuleListRejectedFacts:
		c.RejectedFacts = n
	case CapsuleListContradictions:
		c.Contradictions = n
	case CapsuleListUnresolved:
		c.Unresolved = n
	case CapsuleListScopeReviewIDs:
		c.ScopeReviewIDs = n
	case CapsuleListCoverage:
		c.Coverage = n
	case CapsuleListWaivers:
		c.Waivers = n
	}
}

// Validate refuses a negative count. There is no upper bound: a capsule is
// never refused for the size of the session it records, and the records it
// counts are read a page at a time.
func (c CapsuleCounts) Validate() error {
	for _, list := range CapsuleListOrder {
		if n := c.Of(list); n < 0 {
			return invalid("capsule.counts.%s is %d; a record count is never negative", list, n)
		}
	}
	return nil
}

// CapsuleRow is one record of one capsule list, already in the list's canonical
// order. It is the unit the seal writes and the paged read returns.
//
// Key is the keyset cursor: it is unique within a (capsule, list) and is the
// cursor a continuation names. JSON is the record's canonical encoding, the
// exact bytes context_capsule_rows.row_json stores and a reader decodes back
// into the list's element type.
type CapsuleRow struct {
	List    CapsuleList `json:"list"`
	Ordinal int64       `json:"ordinal"`
	Key     string      `json:"key"`
	JSON    []byte      `json:"json"`

	// elem is the typed record NewCapsuleRow encoded, kept so the same row can
	// be absorbed into the capsule digest without decoding its JSON back. A row
	// read from storage carries none and is never hashed: the identity is
	// derived once, at the seal, from the session's own records.
	elem capsuleElement
}

// Validate enforces the row shape. It does not validate the record itself --
// NewCapsuleRow did that from the typed value, before encoding it.
func (r CapsuleRow) Validate() error {
	if !r.List.Valid() {
		return invalid("capsule_row.list %q is not a capsule list", truncateForMessage(string(r.List)))
	}
	if r.Ordinal < 0 {
		return invalid("capsule_row.ordinal is %d; ordinals start at 0", r.Ordinal)
	}
	if r.Key == "" {
		return invalid("capsule_row.key is empty; every capsule row is addressable by its own key")
	}
	if len(r.JSON) == 0 {
		return invalid("capsule_row.json is empty")
	}
	return nil
}

// capsuleElement is one record's contribution to the capsule identity. Every
// list element type implements it, so the digest's component order for a record
// is written exactly once and cannot drift from the record the rows store.
type capsuleElement interface {
	capsuleKey() string
	absorbCapsule(h *Hasher)
}

// CapsuleScopeNode is a scope entry as a capsule record. Scope is a bare node
// id in the capsule's JSON; this wrapper gives it the key and the digest
// components every other list element has.
type CapsuleScopeNode struct {
	NodeID NodeID `json:"node_id"`
}

func (n CapsuleScopeNode) capsuleKey() string      { return string(n.NodeID) }
func (n CapsuleScopeNode) absorbCapsule(h *Hasher) { h.AddString(string(n.NodeID)) }

// Validate enforces the scope entry's shape. The typed nil is unwrapped: a
// *Error returned straight into an error interface is never nil.
func (n CapsuleScopeNode) Validate() error {
	if err := requireID("capsule.scope", string(n.NodeID)); err != nil {
		return err
	}
	return nil
}

// CapsuleScopeReview is a scope review id as a capsule record.
type CapsuleScopeReview struct {
	ObservationID string `json:"observation_id"`
}

func (r CapsuleScopeReview) capsuleKey() string      { return r.ObservationID }
func (r CapsuleScopeReview) absorbCapsule(h *Hasher) { h.AddString(r.ObservationID) }

// Validate enforces the review entry's shape.
func (r CapsuleScopeReview) Validate() error {
	if err := requireID("capsule.scope_review_ids", r.ObservationID); err != nil {
		return err
	}
	return nil
}

// capsuleKey for a fact needs both halves: one relation can be accepted by more
// than one observation, so the relation alone is not unique and a page keyed on
// it would drop records.
func (r FactReference) capsuleKey() string {
	return string(r.RelationID) + "|" + string(r.ObservationID)
}

func (r FactReference) absorbCapsule(h *Hasher) {
	h.AddString(string(r.RelationID))
	h.AddString(capsuleInt(int64(len(r.EvidenceIDs))))
	for _, e := range r.EvidenceIDs {
		h.AddString(string(e))
	}
	h.AddString(string(r.ObservationID))
}

func (r ObservationReference) capsuleKey() string { return string(r.ObservationID) }

func (r ObservationReference) absorbCapsule(h *Hasher) {
	h.AddString(string(r.ObservationID))
	h.AddString(string(r.Kind))
	h.AddString(capsuleInt(int64(len(r.References))))
	for _, ref := range r.References {
		ref.absorbCapsule(h)
	}
	h.AddString(r.Note)
}

// absorbCapsule for a claim reference is fixed arity: an absent source is four
// empty components rather than none, so an omission can never alias a present
// value.
func (r ClaimReference) absorbCapsule(h *Hasher) {
	h.AddString(string(r.NodeID))
	h.AddString(string(r.RelationID))
	if r.Source == nil {
		h.AddString("")
		h.AddString("")
		h.AddString("")
		h.AddString("")
		return
	}
	h.AddString(string(r.Source.FileID))
	h.AddString(r.Source.ContentHash)
	h.AddString(strconv.FormatUint(r.Source.Bytes.Start, 10))
	h.AddString(strconv.FormatUint(r.Source.Bytes.End, 10))
}

func (c FileCoverage) capsuleKey() string { return string(c.FileID) }

func (c FileCoverage) absorbCapsule(h *Hasher) {
	h.AddString(string(c.FileID))
	h.AddString(c.ContentHash)
	h.AddString(capsuleInt(c.Size))
	h.AddString(string(c.State))
	h.AddString(strconv.FormatBool(c.Waived))
}

func (w WaiverRecord) capsuleKey() string { return string(w.FileID) }

func (w WaiverRecord) absorbCapsule(h *Hasher) {
	h.AddString(string(w.FileID))
	h.AddString(w.ContentHash)
	h.AddString(w.ActorID)
	h.AddString(w.Reason)
}

// capsuleInt renders an integer digest component, matching the manifest hash's
// own spelling.
func capsuleInt(n int64) string { return strconv.FormatInt(n, 10) }

// NewCapsuleRow encodes one record of one list at its canonical ordinal.
//
// It is the one place a capsule record becomes both a stored row and a digest
// contribution, which is what makes the two impossible to drift apart: a seal
// streams its records through here once, the digest absorbs the row and the
// write stores the same row's JSON.
//
// The record's Go type is what says which list it belongs to, so a record
// offered to the wrong list is a programming error and is refused here rather
// than stored under a list whose reader cannot decode it.
func NewCapsuleRow(list CapsuleList, ordinal int64, record any) (CapsuleRow, error) {
	var elem capsuleElement
	switch v := record.(type) {
	case CapsuleScopeNode:
		elem = v
	case CapsuleScopeReview:
		elem = v
	case FactReference:
		elem = v
	case ObservationReference:
		elem = v
	case FileCoverage:
		elem = v
	case WaiverRecord:
		elem = v
	default:
		return CapsuleRow{}, capsuleInternalf("capsule list %q was offered a %T, which is not a capsule record", list, record)
	}
	if !capsuleListAccepts(list, elem) {
		return CapsuleRow{}, capsuleInternalf("a %T is not a record of the capsule's %s list", record, list)
	}
	if v, ok := record.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return CapsuleRow{}, err
		}
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return CapsuleRow{}, capsuleInternalf("capsule %s record could not be encoded: %v", list, err)
	}
	row := CapsuleRow{List: list, Ordinal: ordinal, Key: elem.capsuleKey(), JSON: payload, elem: elem}
	if err := row.Validate(); err != nil {
		return CapsuleRow{}, err
	}
	return row, nil
}

// capsuleListAccepts reports whether a record type may appear in a list. The
// fact and observation types each serve two lists -- accepted and rejected,
// contradictions and unresolved -- so the caller's choice stands as long as it
// is one of that type's own two.
func capsuleListAccepts(list CapsuleList, elem capsuleElement) bool {
	switch elem.(type) {
	case CapsuleScopeNode:
		return list == CapsuleListScope
	case CapsuleScopeReview:
		return list == CapsuleListScopeReviewIDs
	case FactReference:
		return list == CapsuleListAcceptedFacts || list == CapsuleListRejectedFacts
	case ObservationReference:
		return list == CapsuleListContradictions || list == CapsuleListUnresolved
	case FileCoverage:
		return list == CapsuleListCoverage
	case WaiverRecord:
		return list == CapsuleListWaivers
	}
	return false
}

// capsuleInternalf reports a defect in a caller that streamed the wrong record
// or an encoder that failed: it is never a user-correctable input.
func capsuleInternalf(format string, args ...any) *Error {
	return &Error{Code: CodeInternal, Message: fmt.Sprintf(format, args...)}
}

// CapsuleListSource streams a sealing capsule's canonically ordered lists.
//
// It is the seal's only view of the session's records: the counting pass, the
// digest pass and the row write each call Rows, and none of them holds a list
// in memory. Rows starts the named list at its first record every time it is
// called, so the three passes see the same sequence; a source whose passes
// could disagree would seal an identity its own rows do not reproduce.
//
// An implementation yields records through NewCapsuleRow so the stored row and
// the hashed components are one encoding, and pages its own reads so its peak
// heap is one page regardless of how many records the list holds.
type CapsuleListSource interface {
	// Rows calls yield once per record of list, in canonical order, with
	// ordinals ascending from 0. An error from yield stops the walk and is
	// returned unchanged.
	Rows(ctx context.Context, list CapsuleList, yield func(CapsuleRow) error) error
}

// CapsuleCanonicalHash derives the Section 17.3 capsule identity by streaming,
// in CapsuleListOrder, the lists src holds.
//
// It is byte-identical to hashing the assembled capsule: every component is
// length-framed and each list is preceded by its own count, so the counts c
// carries -- taken from the counting pass over the same source -- occupy
// exactly the position the assembled list's length prefix did. That identity is
// what the pinned digests in capsule_test.go gate, and it is why the count must
// come from its own pass: a single-pass hash that emits a list's count after
// its records, or not at all, produces a different digest.
//
// It excludes CreatedAt and every audit timestamp, the local generation id, the
// session and run ids and cursor tokens, so the same records hash identically
// across two builds and across a store reopened later.
func CapsuleCanonicalHash(ctx context.Context, c Capsule, src CapsuleListSource) (string, error) {
	h := NewHasher(CapsuleHashDomain)
	h.AddString(c.ManifestHash)
	h.AddString(string(c.Binding.RepositoryID))
	h.AddString(string(c.Binding.SnapshotID))
	h.AddString(string(c.Binding.AnalysisKey))
	h.AddString(capsuleInt(int64(c.ScopeVersion)))
	for _, list := range CapsuleListOrder {
		count := c.Counts.Of(list)
		h.AddString(capsuleInt(count))
		var seen int64
		err := src.Rows(ctx, list, func(row CapsuleRow) error {
			if row.elem == nil {
				return capsuleInternalf("capsule %s row %d was read back from storage; the identity is derived at the seal", list, row.Ordinal)
			}
			if row.Ordinal != seen {
				return capsuleInternalf("capsule %s row arrived at ordinal %d, expected %d", list, row.Ordinal, seen)
			}
			seen++
			row.elem.absorbCapsule(h)
			return nil
		})
		if err != nil {
			return "", err
		}
		if seen != count {
			// The counting pass and the digest pass disagree, so the digest
			// would name a record set the rows do not hold. Sealing a capsule
			// whose identity is unreproducible is worse than refusing it.
			return "", capsuleInternalf("capsule %s streamed %d records after counting %d", list, seen, count)
		}
	}
	h.AddString(strconv.FormatBool(c.StrictGateSatisfied))
	return h.Sum(), nil
}

// CapsuleHashDomain is the hash domain of the Section 17.3 capsule identity.
// It lives beside the hash it frames: the domain and the component order are
// one definition, and a change to either is a change to every sealed identity.
const CapsuleHashDomain = "codectx.capsule.canonical.v1"
