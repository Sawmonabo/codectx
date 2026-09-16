package graph

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// peakCounter watches the walk's retained directory from inside the walk. The
// directory is removed when the walk ends, so the only place its PEAK can be
// measured is between the reads the walk makes.
type peakCounter struct {
	GraphReader
	scratch string
	sorted  int
	frontie int
}

func (p *peakCounter) Neighbours(ctx context.Context, refs []NodeRef, direction model.Direction,
	kinds []KindCode, from EdgePos, fn func(Edge) error) (EdgePos, error) {
	p.sample()
	return p.GraphReader.Neighbours(ctx, refs, direction, kinds, from, fn)
}

// RelationIDs is the one read a SERVE makes, so sampling it is what catches
// the sorted run of the level being served out.
func (p *peakCounter) RelationIDs(ctx context.Context, refs []RelRef) ([]model.RelationID, error) {
	p.sample()
	return p.GraphReader.RelationIDs(ctx, refs)
}

func (p *peakCounter) sample() {
	var sorted, frontier int
	_ = filepath.WalkDir(p.scratch, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch {
		case strings.HasPrefix(d.Name(), sortedLevelPrefix):
			sorted++
		case strings.HasPrefix(d.Name(), admittedLevelPrefix):
			frontier++
		}
		return nil
	})
	p.sorted = max(p.sorted, sorted)
	p.frontie = max(p.frontie, frontier)
}

// The requirement: a walk that runs to completion inside one request keeps the
// files of the level it is on, not of every level it has crossed.
//
// A level served whole leaves a sorted run and the frontier it was collected
// from behind, and they are held rather than deleted, because a retryable
// failure later in the page sends the caller back to the cursor it was handed
// and that cursor resumes behind both. The hold is released by the mint of the
// next continuation, which is what supersedes that cursor.
//
// An aggregating walk mints nothing at its level boundaries: it chains its
// legs internally and the only cursor its caller can present again is the one
// the request began with. So every level it crossed stayed on disk until the
// walk ended -- two files per level, in a directory the continuation byte
// budget deliberately does not charge, with the depth bound shipping
// unlimited. That is unbounded peak disk in the wave whose subject is disk.
//
// Mutation: hold at the level transition whether or not the leg can mint (drop
// the Mints branch in serveLevel) and the peak is one pair per level.
func TestAWalkThatMintsNothingDoesNotKeepEveryLevelItCrossed(t *testing.T) {
	f := newLadderFixture(t, ladderLevels)
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
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 100000
	limits.QueryTimeout = time.Minute
	watch := &peakCounter{GraphReader: memGraphFor(f), scratch: spools.SortDir()}
	e, err := New(Options{Adjacency: f, Reader: watch, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	res, err := e.Impact(context.Background(), model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("rung-x-00")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if res.Meta.Truncated || res.Meta.NextCursor != "" {
		t.Fatalf("the walk did not run to completion in one request: truncated=%v paged=%v",
			res.Meta.Truncated, res.Meta.NextCursor != "")
	}
	if len(res.Entries) == 0 {
		t.Fatal("the walk served nothing; the fixture proves nothing")
	}
	// The level being served, and at most the level it was collected from:
	// what one level transition has in flight, not what the walk has crossed.
	if watch.sorted > 2 || watch.frontie > 3 {
		t.Fatalf("a %d-level walk that minted no cursor held %d sorted runs and %d frontiers at once; "+
			"a leg that mints nothing is finished with each level as it crosses it",
			ladderLevels, watch.sorted, watch.frontie)
	}
	t.Logf("peak across %d levels: %d sorted runs, %d frontiers", ladderLevels, watch.sorted, watch.frontie)

	entries, err := os.ReadDir(spools.SortDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "walkretain-") {
			t.Errorf("the completed walk left its retained directory behind: %s", en.Name())
		}
	}
}
