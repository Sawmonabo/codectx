package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The requirement: a retryable failure leaves the walk exactly as adoptable as
// the cursor the caller was told to present again. A page crosses several
// level transitions, and each level it serves whole finishes with that level's
// sorted run and with the frontier below it -- but the cursor the caller holds
// resumes BEHIND them, so a page that deleted them and then failed would send
// the caller back to a cursor whose walk reads a level the walk itself has
// removed. That is not a recoverable page: it is CTX_STORAGE_CORRUPT for a
// level the engine minted the continuation from, and the whole walk behind it
// is lost after one contended read.
//
// The levels are therefore HELD for the page and deleted when the next
// continuation is minted, which is the moment the cursor they belong to is
// superseded (releaseHeld).
//
// Mutation proof (fails this test): in serve(), remove the level's sorted run
// and the frontier below it outright instead of holding them -- replace the two
// hold calls with openRetainFile(o.Retain.home, ...).remove() -- and the retry
// answers
//
//	CTX_STORAGE_CORRUPT: graph: the sorted level a continuation names is gone: .../sorted.5
func TestARetriedPageFindsTheLevelsTheFailedPageServedPast(t *testing.T) {
	f := newLadderFixture(t, ladderLevels)
	const items = 12

	want, wantVisited := newWalkLeg(t, f, items).drain(t, ladderRequest())
	if len(want) == 0 {
		t.Fatal("the fixture served no relation")
	}

	leg := newWalkLeg(t, f, items)
	leg.adj.adjacency = true
	req := ladderRequest()
	first, err := leg.e.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("page 1 answered whole; this case needs a continuation")
	}
	got := relationKeys(first.Relations)
	req.Page, req.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0

	// The precondition: the interrupted page served a level PAST the one its
	// cursor names, whole. A page reaches the scan of level L+2 only after
	// level L+1 has been served out, so the committed transition of L+2 is
	// that fact on disk.
	var reached bool
	for page := 2; !reached; page++ {
		if page > 200 {
			t.Fatal("no interrupted page served a level past its cursor's")
		}
		c := leg.cursorAt(t, req.Page.Cursor)
		for at := 1; at <= 40; at++ {
			leg.adj.fail, leg.adj.failAt, leg.adj.calls = true, at, 0
			res, perr := leg.e.Neighbors(context.Background(), req)
			leg.adj.fail = false
			if perr != nil {
				var typed *model.Error
				if !errors.As(perr, &typed) || !typed.Retryable {
					t.Fatalf("the interrupted page reported %v, not a retryable failure: "+
						"the page before it deleted what this one resumes from", perr)
				}
				files := leg.retained(t)
				reached = files[levelFileName(admittedLevelPrefix, c.Level+2)]
				if reached {
					t.Logf("cursor at level %d (%v), interrupted at read %d; retained: %v",
						c.Level, c.LevelState, at, sortedNames(files))
					break
				}
				continue
			}
			// The page answered: it read no adjacency this deep, so it is the
			// cursor it mints that has to be interrupted.
			got = append(got, relationKeys(res.Relations)...)
			if res.Meta.NextCursor == "" {
				t.Fatal("the walk ended before a page served past its cursor's level")
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			break
		}
	}
	if leg.adj.served == 0 {
		t.Fatal("no failure was ever injected")
	}

	// The retry the failure invites: the same cursor, against a healthy store.
	rest, visited := leg.drain(t, req)
	got = append(got, rest...)
	if len(got) != len(want) {
		t.Fatalf("the retried walk served %d relations, the uninterrupted one %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("relation %d of the retried walk is %s, the uninterrupted walk's is %s",
				i, got[i], want[i])
		}
	}
	if visited != wantVisited {
		t.Fatalf("the retried walk visited %d nodes, the uninterrupted one %d", visited, wantVisited)
	}
}
