package model

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// TestCapsuleCanonicalHashIsByteIdentical is the capsule identity's byte gate.
//
// A sealed capsule is a durable artifact a later session replays by its
// canonical hash, so the hash must not move when the capsule stops carrying its
// records and starts streaming them. Two things are asserted and they are not
// the same thing:
//
//  1. The streamed digest equals wholeCapsuleHash, a VERBATIM copy of the
//     whole-list hash as it stood before the records became rows. That is the
//     compatibility proof.
//  2. Both equal a pinned literal. That is the regression proof: a change to
//     either implementation that moves them together still fails here.
//
// The pinned literals are what a single-pass hash cannot produce. Each list's
// count is absorbed BEFORE its records, so the count has to come from its own
// pass over the source; a hash that streamed the records once and emitted the
// count afterwards, or omitted it, yields a different digest and fails this
// test. The mutation is recorded in the lane report.
func TestCapsuleCanonicalHashIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture capsuleFixture
		pinned  string
	}{
		{"full", fullCapsuleFixture(), "2b4713c79a05fa2e3505dfb0798794222b362975a4bb466065e800d74bad8ad3"},
		{"empty", emptyCapsuleFixture(), "242426dc7242401052de1a49aa9552584c555026ad939e4ff135178e8cfcacbc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			streamed, err := CapsuleCanonicalHash(context.Background(), tc.fixture.capsule, tc.fixture)
			if err != nil {
				t.Fatalf("streaming the capsule identity: %v", err)
			}
			if whole := tc.fixture.wholeCapsuleHash(); streamed != whole {
				t.Fatalf("streamed digest %s does not match the whole-capsule digest %s", streamed, whole)
			}
			if streamed != tc.pinned {
				t.Fatalf("capsule identity moved: got %s, pinned %s", streamed, tc.pinned)
			}
		})
	}
}

// TestCapsuleCountsDisagreeingWithTheStreamIsRefused guards the one way a
// streamed identity can lie: the counting pass and the record pass must see the
// same source. A digest hashed over a count the records do not reach names a
// record set the rows will not reproduce, so it is refused rather than sealed.
func TestCapsuleCountsDisagreeingWithTheStreamIsRefused(t *testing.T) {
	f := fullCapsuleFixture()
	f.capsule.Counts.Coverage += 3
	if _, err := CapsuleCanonicalHash(context.Background(), f.capsule, f); err == nil {
		t.Fatal("a capsule whose coverage count exceeds its streamed records was hashed")
	}
}

// capsuleFixture is a whole capsule held in memory, which is exactly what the
// production path no longer does: it is a test source, small by construction,
// and it is the only place the pre-row list shape still exists.
type capsuleFixture struct {
	capsule        Capsule
	scope          []CapsuleScopeNode
	accepted       []FactReference
	rejected       []FactReference
	contradictions []ObservationReference
	unresolved     []ObservationReference
	reviews        []CapsuleScopeReview
	coverage       []FileCoverage
	waivers        []WaiverRecord
}

// Rows implements CapsuleListSource. Every call restarts the named list, which
// is the contract the three sealing passes rely on.
func (f capsuleFixture) Rows(ctx context.Context, list CapsuleList, yield func(CapsuleRow) error) error {
	var records []any
	switch list {
	case CapsuleListScope:
		for _, v := range f.scope {
			records = append(records, v)
		}
	case CapsuleListAcceptedFacts:
		for _, v := range f.accepted {
			records = append(records, v)
		}
	case CapsuleListRejectedFacts:
		for _, v := range f.rejected {
			records = append(records, v)
		}
	case CapsuleListContradictions:
		for _, v := range f.contradictions {
			records = append(records, v)
		}
	case CapsuleListUnresolved:
		for _, v := range f.unresolved {
			records = append(records, v)
		}
	case CapsuleListScopeReviewIDs:
		for _, v := range f.reviews {
			records = append(records, v)
		}
	case CapsuleListCoverage:
		for _, v := range f.coverage {
			records = append(records, v)
		}
	case CapsuleListWaivers:
		for _, v := range f.waivers {
			records = append(records, v)
		}
	default:
		return invalid("unknown capsule list %q", list)
	}
	for i, rec := range records {
		row, err := NewCapsuleRow(list, int64(i), rec)
		if err != nil {
			return err
		}
		if err := yield(row); err != nil {
			return err
		}
	}
	return nil
}

// wholeCapsuleHash is the capsule identity as it was computed before the
// records became rows: the assembled lists, each preceded by its own length.
// It is kept verbatim so the streamed hash has something independent to be
// byte-identical to, and it is deliberately not built from CapsuleCanonicalHash
// or from capsuleElement.absorbCapsule -- a copy that shared their code could
// not detect a change in them.
func (f capsuleFixture) wholeCapsuleHash() string {
	c := f.capsule
	h := NewHasher(CapsuleHashDomain)
	h.AddString(c.ManifestHash)
	h.AddString(string(c.Binding.RepositoryID))
	h.AddString(string(c.Binding.SnapshotID))
	h.AddString(string(c.Binding.AnalysisKey))
	h.AddString(strconv.FormatInt(int64(c.ScopeVersion), 10))

	h.AddString(strconv.FormatInt(int64(len(f.scope)), 10))
	for _, n := range f.scope {
		h.AddString(string(n.NodeID))
	}
	for _, facts := range [][]FactReference{f.accepted, f.rejected} {
		h.AddString(strconv.FormatInt(int64(len(facts)), 10))
		for _, fact := range facts {
			h.AddString(string(fact.RelationID))
			h.AddString(strconv.FormatInt(int64(len(fact.EvidenceIDs)), 10))
			for _, e := range fact.EvidenceIDs {
				h.AddString(string(e))
			}
			h.AddString(string(fact.ObservationID))
		}
	}
	for _, refs := range [][]ObservationReference{f.contradictions, f.unresolved} {
		h.AddString(strconv.FormatInt(int64(len(refs)), 10))
		for _, o := range refs {
			h.AddString(string(o.ObservationID))
			h.AddString(string(o.Kind))
			h.AddString(strconv.FormatInt(int64(len(o.References)), 10))
			for _, r := range o.References {
				h.AddString(string(r.NodeID))
				h.AddString(string(r.RelationID))
				if r.Source == nil {
					h.AddString("")
					h.AddString("")
					h.AddString("")
					h.AddString("")
					continue
				}
				h.AddString(string(r.Source.FileID))
				h.AddString(r.Source.ContentHash)
				h.AddString(strconv.FormatUint(r.Source.Bytes.Start, 10))
				h.AddString(strconv.FormatUint(r.Source.Bytes.End, 10))
			}
			h.AddString(o.Note)
		}
	}
	h.AddString(strconv.FormatInt(int64(len(f.reviews)), 10))
	for _, r := range f.reviews {
		h.AddString(r.ObservationID)
	}
	h.AddString(strconv.FormatInt(int64(len(f.coverage)), 10))
	for _, cov := range f.coverage {
		h.AddString(string(cov.FileID))
		h.AddString(cov.ContentHash)
		h.AddString(strconv.FormatInt(cov.Size, 10))
		h.AddString(string(cov.State))
		h.AddString(strconv.FormatBool(cov.Waived))
	}
	h.AddString(strconv.FormatInt(int64(len(f.waivers)), 10))
	for _, w := range f.waivers {
		h.AddString(string(w.FileID))
		h.AddString(w.ContentHash)
		h.AddString(w.ActorID)
		h.AddString(w.Reason)
	}
	h.AddString(strconv.FormatBool(c.StrictGateSatisfied))
	return h.Sum()
}

// capsuleTestID renders a distinct content-derived id of the right shape.
func capsuleTestID(seed string) string {
	return strings.Repeat(seed, IDHexLen/len(seed))
}

func emptyCapsuleFixture() capsuleFixture {
	return capsuleFixture{capsule: Capsule{
		SessionID:    SessionID(capsuleTestID("0a")),
		ActorID:      "capsule-test",
		Binding:      Binding{RepositoryID: RepositoryID(capsuleTestID("1b")), SnapshotID: SnapshotID(capsuleTestID("2c")), AnalysisKey: AnalysisKey(capsuleTestID("3d"))},
		ManifestHash: capsuleTestID("4e"),
		ScopeVersion: 1,
	}}
}

func fullCapsuleFixture() capsuleFixture {
	f := emptyCapsuleFixture()
	f.scope = []CapsuleScopeNode{{NodeID: NodeID(capsuleTestID("5f"))}, {NodeID: NodeID(capsuleTestID("60"))}}
	f.accepted = []FactReference{{
		RelationID:    RelationID(capsuleTestID("71")),
		EvidenceIDs:   []EvidenceID{EvidenceID(capsuleTestID("82"))},
		ObservationID: ObservationID(capsuleTestID("93")),
	}}
	f.contradictions = []ObservationReference{{
		ObservationID: ObservationID(capsuleTestID("a4")),
		Kind:          ObservationContradiction,
		References: []ClaimReference{{
			Source: &SourceCitation{
				FileID:      FileID(capsuleTestID("b5")),
				ContentHash: capsuleTestID("c6"),
				Bytes:       ByteRange{Start: 4, End: 96},
			},
		}},
		Note: "the two accepted relations disagree",
	}}
	f.reviews = []CapsuleScopeReview{{ObservationID: capsuleTestID("d7")}}
	f.coverage = []FileCoverage{{
		FileID:         FileID(capsuleTestID("b5")),
		ContentHash:    capsuleTestID("c6"),
		Size:           4096,
		ConfirmedBytes: 4096,
		Requirement:    RequirementFull,
		State:          CoverageFullServed,
		Waived:         true,
	}}
	f.waivers = []WaiverRecord{{
		SessionID:   f.capsule.SessionID,
		ActorID:     f.capsule.ActorID,
		FileID:      FileID(capsuleTestID("b5")),
		ContentHash: capsuleTestID("c6"),
		Reason:      "the file is generated and is reviewed at its source",
	}}
	f.capsule.Counts = CapsuleCounts{
		Scope: int64(len(f.scope)), AcceptedFacts: int64(len(f.accepted)),
		Contradictions: int64(len(f.contradictions)), ScopeReviewIDs: int64(len(f.reviews)),
		Coverage: int64(len(f.coverage)), Waivers: int64(len(f.waivers)),
	}
	return f
}
