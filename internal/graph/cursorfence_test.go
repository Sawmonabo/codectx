package graph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestACursorIsFencedToItsGenerationAndItsPayloadVersion is the fence ADR-0005
// Decision 2 rests on. A v8 cursor names SURROGATES -- a scan position, a
// retained directory whose two bitsets are indexed by node and relation ref --
// and a surrogate means nothing outside the generation that minted it. Ref 41
// is one function in one generation and an unrelated one in the next, so a
// continuation applied across generations would not fail: it would silently
// answer about the wrong nodes.
//
// Two rejections therefore have to hold, and both are CTX_CURSOR_INVALID:
//
//   - a well-formed, correctly SIGNED v8 cursor presented to a reader pinned to
//     another generation, which is the fence proper;
//   - a correctly signed payload of an EARLIER version, which is the refusal
//     that keeps a token minted before the surrogate walk from being read under
//     its assumptions. The bitset layout version moves with it.
//
// Mutation proofs (each fails this test): drop the generation and analysis-key
// comparison in resumeTraversal; accept any version in traversalCursor.validate.
func TestACursorIsFencedToItsGenerationAndItsPayloadVersion(t *testing.T) {
	f := newGraphFixture(t)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// One item a page, so the first request is guaranteed to mint a real
	// continuation rather than answering whole.
	limits.MaxPageItems = 1
	engineFor := func(binding model.Binding) *Engine {
		t.Helper()
		g := memGraphFor(f)
		g.binding = binding
		e, err := New(Options{Adjacency: f, Reader: g, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		return e
	}

	pinned := engineFor(f.binding)
	req := model.ImpactRequest{GenerationID: f.binding.GenerationID,
		Start:     []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	first, err := pinned.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatalf("page 1 answered whole (%q); this case needs a real continuation",
			first.Meta.TruncationReason)
	}

	// The same token against the same generation is honoured: without this the
	// two refusals below would pass for a token that simply never worked.
	req.Page, req.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0
	if _, err := pinned.Impact(context.Background(), req); err != nil {
		t.Fatalf("page 2 on the pinned generation: %v", err)
	}

	t.Run("another generation refuses it", func(t *testing.T) {
		next := f.binding
		next.GenerationID = f.binding.GenerationID + 1
		next.AnalysisKey = model.AnalysisKey(fixtureID("akey-2"))
		_, err := engineFor(next).Impact(context.Background(), req)
		assertCursorInvalid(t, err)
	})

	t.Run("an earlier payload version refuses it", func(t *testing.T) {
		raw, err := signer.Verify(first.Meta.NextCursor, pagination.PurposeCursor, time.Now())
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		var c traversalCursor
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if c.Version != traversalCursorVersion {
			t.Fatalf("the minted cursor carries version %d, want %d", c.Version, traversalCursorVersion)
		}
		c.Version = traversalCursorVersion - 1
		payload, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		// SIGNED with the installation key, so the only thing that can refuse
		// it is the version check itself and not the tag.
		stale, err := signer.Sign(pagination.PurposeCursor, payload, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		old := req
		old.Page = model.PageRequest{Cursor: stale}
		_, err = pinned.Impact(context.Background(), old)
		assertCursorInvalid(t, err)
	})
}

func assertCursorInvalid(t *testing.T, err error) {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
		t.Fatalf("err = %v; want a typed %s", err, model.CodeCursorInvalid)
	}
}
