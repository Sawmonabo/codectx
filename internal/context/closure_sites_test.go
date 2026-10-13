package context

import (
	"encoding/json"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestTheClosureStartsAtTheSitesThatNameAnUnreachedNode pins the closure's
// start set (closeKeptFiles step 1): beside the kept files' unreached nodes,
// the callers of the open call sites in any file that may call one of them by
// name. A site only an unread match reaches lends a kept-file node a name-only
// candidate in the full read, so the closure must walk from its component to
// find that match.
//
// The open-site fixture: `Run` calls the site `v.Speak()`, and the declaration
// `Speak` it may call lives in another file, standing for a kept file's
// unreached node.
//
// Mutation caught: the closure starting from the unreached nodes alone (the
// site is missing from the start set, so its candidate never joins the plan).
func TestTheClosureStartsAtTheSitesThatNameAnUnreachedNode(t *testing.T) {
	fx := newFixtureFrom(t, openSiteSpecs(), openSiteScope)
	site, err := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReceiverUntyped, Shape: model.CallShapeMember, Arguments: 0})
	if err != nil {
		t.Fatalf("marshal open-site metadata: %v", err)
	}
	eng := openSiteEngine(fx, map[model.NodeID]json.RawMessage{fx.Node(siteNode): site})
	c := intCompiler(t, fx, fx.Now)
	in, err := c.newSeedIngest(openSorts(t, fx.Cfg))
	if err != nil {
		t.Fatalf("newSeedIngest: %v", err)
	}
	target := fx.Node(siteTarget)
	run, err := c.withNamingSites(fx.ctx, in, eng, fx.Gen, []string{string(target)}, false)
	if err != nil {
		t.Fatalf("withNamingSites: %v", err)
	}
	got := map[string]bool{}
	if err := run.Each(func(r idRec) error { got[r.ID] = true; return nil }); err != nil {
		t.Fatalf("read the start set: %v", err)
	}
	caller := fx.Node(siteCaller)
	if !got[string(target)] || !got[string(caller)] || len(got) != 2 {
		t.Fatalf("the closure starts at %v, want the unreached node %s and %s, the caller of the site that names it",
			got, target, caller)
	}
	if !in.scope.ScopeComplete {
		t.Fatal("reading the sites narrowed the scope; nothing was cut")
	}
}

// TestASiteOutsideTheKeptFilesLendsOnlyItsKeptCandidates pins the other half
// of the closure's open-site rule (closeKeptFiles step 4): a site the closure
// reaches outside the kept files is not part of the plan, and of its
// candidates only those declared in a kept file join it.
//
// Mutation caught: admitCandidates ignoring its file filter, so the closure
// adds name-only candidates in files the plan dropped.
func TestASiteOutsideTheKeptFilesLendsOnlyItsKeptCandidates(t *testing.T) {
	fx := newFixtureFrom(t, openSiteSpecs(), openSiteScope)
	site, err := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReceiverUntyped, Shape: model.CallShapeMember, Arguments: 0})
	if err != nil {
		t.Fatalf("marshal open-site metadata: %v", err)
	}
	eng := openSiteEngine(fx, map[model.NodeID]json.RawMessage{fx.Node(siteNode): site})
	c := intCompiler(t, fx, fx.Now)
	in, err := c.newSeedIngest(openSorts(t, fx.Cfg))
	if err != nil {
		t.Fatalf("newSeedIngest: %v", err)
	}
	reached := []model.ImpactEntry{{NodeID: fx.Node(siteNode), FileID: fx.File(siteNode).ID}}
	before := in.seq
	if err := c.admitCandidates(fx.ctx, eng, fx.Gen, reached, in, map[model.FileID]bool{fx.File(siteCaller).ID: true}); err != nil {
		t.Fatalf("admitCandidates, candidate file not kept: %v", err)
	}
	if in.seq != before {
		t.Fatalf("the site lent %d candidates outside the kept files, want none", in.seq-before)
	}
	if err := c.admitCandidates(fx.ctx, eng, fx.Gen, reached, in, map[model.FileID]bool{fx.File(siteTarget).ID: true}); err != nil {
		t.Fatalf("admitCandidates, candidate file kept: %v", err)
	}
	if in.seq != before+1 {
		t.Fatalf("the site lent %d candidates to the kept file, want its one candidate `Speak`", in.seq-before)
	}
	if in.scope.CandidatesUnavailable != "" {
		t.Fatalf("the candidate lookup answered unavailable: %s", in.scope.CandidatesUnavailable)
	}
}
