package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// ladderLevels is the fixture's depth: enough rungs that a page resumes
// mid-walk and can still cross a level transition before it is interrupted.
const ladderLevels = 12

// newLadderFixture builds a graph of `levels` rungs, two nodes to a rung. The
// walk it feeds admits one rung per level and serves three relations there, so
// a page crosses several level transitions instead of spending its whole item
// budget inside one level -- which is what makes the interruption below land
// where it has to.
func newLadderFixture(t *testing.T, levels int) *graphFixture {
	t.Helper()
	f := &graphFixture{nodes: map[model.NodeID]model.Node{}, evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		}}
	rung := func(side string, level int) string { return fmt.Sprintf("rung-%s-%02d", side, level) }
	addNode := func(n string) {
		id := fixtureNodeID(n)
		f.nodes[id] = model.Node{ID: id, Kind: model.NodeFunction, Name: n, QualifiedName: n,
			Language: "go", SemanticSource: model.SemanticCanonical}
	}
	type edge struct{ from, to string }
	var edges []edge
	addNode(rung("x", 0))
	for i := 1; i <= levels; i++ {
		addNode(rung("x", i))
		addNode(rung("y", i))
		edges = append(edges,
			edge{rung("x", i-1), rung("x", i)},
			edge{rung("x", i-1), rung("y", i)},
			// The sibling edge, between two nodes of the SAME rung: its far end
			// is already admitted, so the direction rule decides it with the
			// frontier bitset rather than with the visited bits.
			edge{rung("x", i), rung("y", i)})
	}
	for i, e := range edges {
		id := fixtureRelationID(i)
		f.relations = append(f.relations, model.Relation{ID: id,
			From: fixtureNodeID(e.from), Kind: model.RelCalls, To: fixtureNodeID(e.to)})
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })
	return f
}

// interruptedReader fails one read of the current page with a RETRYABLE error,
// whose whole remedy is "present this cursor again".
//
// Which read it interrupts decides which state the page is left in, so both
// reads a page makes are available: naming a relation's route is the only read
// a SERVE makes, so a failure there leaves the level the page just committed
// unserved; an adjacency read belongs to a level's scan, so a failure there
// leaves every level before it served whole.
type interruptedReader struct {
	GraphReader
	fail      bool
	adjacency bool // interrupt a level's scan rather than the naming of a route
	failAt    int  // the read of this page the failure lands on, from one
	calls     int
	served    int // failures actually injected, so a search cannot pass by never failing
}

// interrupt reports whether this read is the one to fail.
func (r *interruptedReader) interrupt(adjacency bool) error {
	if r.adjacency != adjacency {
		return nil
	}
	r.calls++
	if !r.fail || r.calls < max(1, r.failAt) {
		return nil
	}
	r.fail = false
	r.served++
	return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
		Message: "the graph store was contended for this read"}
}

func (r *interruptedReader) RelationIDs(ctx context.Context, refs []RelRef) ([]model.RelationID, error) {
	if err := r.interrupt(false); err != nil {
		return nil, err
	}
	return r.GraphReader.RelationIDs(ctx, refs)
}

func (r *interruptedReader) Neighbours(ctx context.Context, refs []NodeRef, direction model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	if err := r.interrupt(true); err != nil {
		return EdgePos{}, err
	}
	return r.GraphReader.Neighbours(ctx, refs, direction, kinds, from, fn)
}

// walkLeg is one traversal engine over its own scratch, so two runs of the same
// request never share a retained directory.
type walkLeg struct {
	e       *Engine
	adj     *interruptedReader
	signer  *pagination.Signer
	scratch string
}

func newWalkLeg(t *testing.T, f *graphFixture, items int) *walkLeg {
	t.Helper()
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
	limits.MaxPageItems = items
	limits.MaxDepth = ladderLevels
	limits.QueryTimeout = time.Minute
	adj := &interruptedReader{GraphReader: memGraphFor(f)}
	e, err := New(Options{Adjacency: f, Reader: adj, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return &walkLeg{e: e, adj: adj, signer: signer, scratch: spools.SortDir()}
}

// retained names the files of the walk's retained directory, so a case can say
// which levels are committed and which runs are still on disk.
func (l *walkLeg) retained(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := filepath.WalkDir(l.scratch, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out[d.Name()] = true
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatalf("read the walk scratch: %v", err)
	}
	return out
}

// cursorAt decodes one continuation, which is how the case names the level a
// cursor resumes into without reaching inside the walk.
func (l *walkLeg) cursorAt(t *testing.T, token string) traversalCursor {
	t.Helper()
	raw, err := l.signer.Verify(token, pagination.PurposeCursor, time.Now())
	if err != nil {
		t.Fatalf("verify the continuation: %v", err)
	}
	var c traversalCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode the continuation: %v", err)
	}
	return c
}

func ladderRequest() model.GraphRequest {
	return model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("rung-x-00")},
		Direction: model.DirectionBoth, Relations: []model.RelationKind{model.RelCalls}}
}

// relationKeys names the relations of one page, whole: a walk that served the
// same edge under another owner or kind is not the same answer.
func relationKeys(rels []model.Relation) []string {
	out := make([]string, 0, len(rels))
	for _, r := range rels {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", r.ID, r.From, r.To, r.Kind))
	}
	return out
}

// drain pages the request to its end and returns every relation it served, in
// order, with the cumulative visited count of the last page.
func (l *walkLeg) drain(t *testing.T, req model.GraphRequest) ([]string, int64) {
	t.Helper()
	var (
		rels    []string
		visited int64
	)
	for page := 1; ; page++ {
		if page > 500 {
			t.Fatal("the walk did not end in 500 pages")
		}
		res, err := l.e.Neighbors(context.Background(), req)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		rels = append(rels, relationKeys(res.Relations)...)
		visited = res.VisitedCount
		if res.Meta.NextCursor == "" {
			return rels, visited
		}
		req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
	}
}

// The requirement: the frontier bitset a leg works against is the admitted set
// of the level it is serving -- the set closeGroup charges the visited budget
// from, and the set the NEXT level's scan applies the direction rule against.
// The bitset is rebuilt in place by each transition, so the two states where a
// leg picks a level up WITHOUT running its transition have to restore it:
//
//   - a page fails retryably after it has gone on past the level its cursor
//     names, and the caller presents that cursor again (the retained state's
//     frontier is then the later level's);
//   - the leg reaches a level whose transition committed in an earlier leg and
//     serves it out of the run that transition left (the frontier is then the
//     level BEFORE it, the last one this leg transitioned).
//
// Both are reached below by interrupting a page during the serve of a level it
// has just committed, which is the only interruption that leaves the level
// committed and its run unserved.
//
// Mutation proofs, each failing this test:
//
//   - stub alignFrontier with `if true { return nil }`: the retry re-serves its
//     level against the LATER level's bits, closeGroup charges nothing for the
//     groups it closes, and the walk reports fewer visited nodes than the
//     uninterrupted one.
//   - call alignFrontier instead of setFrontier on collect's committed path:
//     the level is served with the previous level's bits and the scan after it
//     drops every edge whose far end that level admitted -- the rung's sibling
//     relation vanishes from a walk that reports itself complete.
func TestTheFrontierIsRealignedForACursorRepresentedAfterALaterLevelCommitted(t *testing.T) {
	f := newLadderFixture(t, ladderLevels)
	const items = 12

	// The answer an uninterrupted walk gives, which is what a retryable failure
	// may not change.
	want, wantVisited := newWalkLeg(t, f, items).drain(t, ladderRequest())
	if len(want) == 0 {
		t.Fatal("the fixture served no relation")
	}

	leg := newWalkLeg(t, f, items)
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

	// The precondition: a page entered on this cursor commits the level AFTER
	// the one the cursor names, begins serving it, and only then fails
	// retryably. The failure is moved one read further into the page until that
	// state is on disk.
	var (
		reached bool
		c       traversalCursor
	)
	for page := 2; !reached; page++ {
		if page > 200 {
			t.Fatal("no interrupted page committed the level after its cursor's")
		}
		c = leg.cursorAt(t, req.Page.Cursor)
		for at := 1; at <= 40; at++ {
			leg.adj.fail, leg.adj.failAt, leg.adj.calls = true, at, 0
			res, perr := leg.e.Neighbors(context.Background(), req)
			leg.adj.fail = false
			if perr != nil {
				var typed *model.Error
				if !errors.As(perr, &typed) || !typed.Retryable {
					t.Fatalf("the interrupted page reported %v, not a retryable failure", perr)
				}
				files := leg.retained(t)
				// The state under test, whole: the level after the cursor's is
				// committed -- so the frontier bitset is THAT level's -- and
				// what the cursor resumes from is still on disk.
				reached = files[levelFileName(admittedLevelPrefix, c.Level+1)] &&
					files[levelFileName(admittedLevelPrefix, c.Level)] &&
					files[levelFileName(sortedLevelPrefix, c.Level)]
				if reached {
					t.Logf("cursor at level %d (%v), interrupted at read %d; retained: %v",
						c.Level, c.LevelState, at, sortedNames(files))
					break
				}
				continue
			}
			// The page answered: it read nothing this deep, so it is the cursor
			// it mints that has to be interrupted.
			got = append(got, relationKeys(res.Relations)...)
			if res.Meta.NextCursor == "" {
				t.Fatal("the walk ended before a page could commit the level after its cursor's")
			}
			req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
			break
		}
	}
	if leg.adj.served == 0 {
		t.Fatal("no failure was ever injected")
	}

	// The retry the failure invites: the same cursor, against a healthy store.
	// The frontier bitset holds the LATER level's admissions, and the walk that
	// resumes here serves the earlier one.
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

// sortedNames is the retained directory's file names in a stable order, for a
// failure message.
func sortedNames(files map[string]bool) []string {
	out := make([]string, 0, len(files))
	for name := range files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
