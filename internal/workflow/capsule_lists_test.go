package workflow

import (
	"context"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// waiverFixtureCount is the waiver population the page-bound row seals. It is a
// whole multiple of the fixture service's MaxPageItems so the expected call
// count is exact rather than a ceiling, and it is far larger than one page so a
// whole-list read cannot hide inside the page bound.
const waiverFixtureCount = 20000

// TestCapsuleWaiversAreReadOnePageAtATime protects wave-H audit item S5: the
// seal must read the session's waivers a page at a time, like every other
// capsule list, so seal-time heap is a function of the page and not of the
// session's waiver count.
//
// The invariant is stated at the store interface because that is where it is
// decidable. A whole-list Sessions.Waivers and a keyset WaiversAfter seal the
// same capsule with the same digest and the same rows -- the only difference is
// how many records crossed the interface in one call. Reverting capsuleSource
// .waivers to a whole-list read fails both assertions below: one call per pass
// instead of waiverFixtureCount/MaxPageItems+1, and that one call answering
// every record regardless of the page bound it was handed.
func TestCapsuleWaiversAreReadOnePageAtATime(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.store.AdvanceSession(ctx, model.AdvanceRequest{
		SessionID: fixtureSession, ActorID: fixtureActor,
		Target: model.StateConsolidateOpen, ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("open consolidate on the fixture session: %v", err)
	}
	h.seedWaivers(t, waiverFixtureCount)

	h.store.mu.Lock()
	h.store.waiverPages = nil
	h.store.mu.Unlock()

	// A recorded waiver and a satisfied strict gate is the one combination
	// Capsule.Validate refuses outright, so this seal is non-strict.
	c, err := h.svc.buildCapsule(ctx, h.session(fixtureSession),
		gate{ReadComplete: true, Ready: true, Strict: false, ScopeComplete: true})
	if err != nil {
		t.Fatalf("seal a capsule for a session with %d waivers: %v", waiverFixtureCount, err)
	}
	if c.Counts.Waivers != waiverFixtureCount {
		t.Fatalf("the sealed capsule counts %d waivers; the session recorded %d",
			c.Counts.Waivers, waiverFixtureCount)
	}

	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	pages := h.store.waiverPages
	// Three passes -- count, hash, write -- each walking the list a page at a
	// time and stopping on the empty page past the last record.
	perPass := waiverFixtureCount/model.MaxPageItems + 1
	if len(pages) != 3*perPass {
		t.Fatalf("the seal made %d waiver page reads for %d waivers; want %d (three passes of %d). "+
			"One read per pass means the list crossed the interface whole",
			len(pages), waiverFixtureCount, 3*perPass, perPass)
	}
	var previous model.FileID
	for i, p := range pages {
		if p.got > p.limit {
			t.Fatalf("waiver page read %d answered %d records against a page bound of %d; "+
				"the seal held a list larger than one page", i, p.got, p.limit)
		}
		if i%perPass == 0 {
			if p.after != "" {
				t.Fatalf("waiver pass %d opened at cursor %q, not at the start of the list", i/perPass, p.after)
			}
		} else if p.after <= previous {
			t.Fatalf("waiver page read %d resumed at cursor %q, which does not advance past %q; "+
				"a keyset that does not advance either repeats records or loops", i, p.after, previous)
		}
		previous = p.after
	}
}

// TestCapsuleFactOrderIsIndependentOfArrivalOrder closes the wave-H audit's
// unproven concern 2: capsule.go's fact collector sorts the relation ids WITHIN
// one observation and leans on the store's observation-id order ACROSS
// observations, and no row discriminated it -- the fresh-store determinism row
// records no observations at all.
//
// Two stores record the SAME accepted facts, and only the order differs: the
// observations arrive in a different sequence, and the relation ids inside each
// observation are listed ascending in one store and descending in the other.
// The observation id is derived from canonically sorted references, so the two
// stores hold byte-identical observations under identical ids; what is left to
// get wrong is the order the capsule emits them in.
//
// Deleting the sort.Slice over the relation ids in capsuleSource.facts fails
// this row: the two capsules then carry the same facts in opposite order under
// different row ordinals, and the streamed identity diverges.
func TestCapsuleFactOrderIsIndependentOfArrivalOrder(t *testing.T) {
	ctx := context.Background()
	g := gate{ReadComplete: true, Ready: true, Strict: true, ScopeComplete: true}

	seal := func(t *testing.T, descending bool, arrival []int) model.Capsule {
		t.Helper()
		h := newHarness(t)
		rec, _ := h.consolidating(t)
		h.seedAcceptedFacts(t, descending, arrival)
		c, err := h.svc.buildCapsule(ctx, rec, g)
		if err != nil {
			t.Fatalf("seal the capsule: %v", err)
		}
		return c
	}

	first := seal(t, false, []int{0, 1, 2})
	second := seal(t, true, []int{2, 0, 1})

	if first.Counts.AcceptedFacts != second.Counts.AcceptedFacts || first.Counts.AcceptedFacts == 0 {
		t.Fatalf("the two stores sealed %d and %d accepted facts; the fixtures record the same set and it is not empty",
			first.Counts.AcceptedFacts, second.Counts.AcceptedFacts)
	}
	if first.CanonicalHash != second.CanonicalHash {
		t.Fatalf("two stores holding the same facts in different arrival order sealed different identities: %s and %s",
			first.CanonicalHash, second.CanonicalHash)
	}
}

// seedWaivers writes n distinct waivers straight into the fake, which is how
// every row here arranges store state it is not testing the recording of. The
// file ids are the fixture's waived file salted by index, so they are distinct
// and their order is not the order they are appended in.
func (h *harness) seedWaivers(t *testing.T, n int) {
	t.Helper()
	fs := h.store.sessions[fixtureSession]
	h.file(fixtureSession, fileWaived).waived = true
	fs.waivers = nil
	for i := 0; i < n; i++ {
		fs.waivers = append(fs.waivers, model.WaiverRecord{
			SessionID: fixtureSession, ActorID: fixtureActor,
			FileID:      model.FileID(fixtureID("waived", strconv.Itoa(i))),
			ContentHash: fixtureID("waived-hash", strconv.Itoa(i)),
			Reason:      "vendored generated code " + strconv.Itoa(i),
			CreatedAt:   fixtureNow,
		})
	}
}

// seedAcceptedFacts writes three accept_fact observations, each naming three
// relation ids, straight into the fake. arrival is the order the observations
// are appended in and descending reverses the reference order inside each one,
// so two stores can hold the same facts recorded differently.
//
// The reference SET is identical in both spellings: NewObservationID hashes the
// reference count and the canonically sorted keys, so any difference beyond
// order would give the two stores different observation ids and the comparison
// would fail for a reason that has nothing to do with the capsule's ordering.
func (h *harness) seedAcceptedFacts(t *testing.T, descending bool, arrival []int) {
	t.Helper()
	fs := h.store.sessions[fixtureSession]
	for _, n := range arrival {
		refs := make([]model.ClaimReference, 0, 3)
		for j := 0; j < 3; j++ {
			k := j
			if descending {
				k = 2 - j
			}
			refs = append(refs, model.ClaimReference{
				RelationID: model.RelationID(fixtureID("relation", strconv.Itoa(n)+"-"+strconv.Itoa(k))),
			})
		}
		req := model.ObservationRequest{
			SessionID: fixtureSession, ActorID: fixtureActor, ExpectedScope: fs.rec.ScopeVersion,
			Kind: model.ObservationAcceptFact, References: refs,
			Note: "accepted fact " + strconv.Itoa(n),
		}
		fs.obs = append(fs.obs, model.Observation{
			ID: model.NewObservationID(req), SessionID: fixtureSession, ActorID: fixtureActor,
			ScopeVersion: fs.rec.ScopeVersion, Kind: model.ObservationAcceptFact,
			References: refs, Note: req.Note, CreatedAt: fixtureNow,
		})
	}
}
