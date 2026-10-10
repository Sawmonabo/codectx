package context

import (
	stdcontext "context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The open-call-site fixture: `Run` calls `Helper` precisely, `Helper` calls
// `Deep` and `Deep` calls `Deeper`, so the plan holds required, recommended and
// optional precise entries. `Run` also calls `Speak` through an open call site
// no analysis bound, and one declaration named `Speak` lives elsewhere: the
// site's one name-only candidate. The fixture publishes one node per file, so
// the site node has a file of its own.
const (
	siteCaller = "internal/app/run.go"
	siteHelper = "internal/app/helper.go"
	siteDeep   = "internal/app/deep.go"
	siteDeeper = "internal/app/deeper.go"
	siteNode   = "internal/app/speak_site.go"
	siteTarget = "internal/zoo/speak.go"
)

func openSiteSpecs() []fixtureFileSpec {
	return []fixtureFileSpec{
		{siteCaller, "Run", model.NodeFunction, "package app\n\nfunc Run() { Helper(); v.Speak() }\n", false},
		{siteHelper, "Helper", model.NodeFunction, "package app\n\nfunc Helper() { Deep() }\n", false},
		{siteDeep, "Deep", model.NodeFunction, "package app\n\nfunc Deep() { Deeper() }\n", false},
		{siteDeeper, "Deeper", model.NodeFunction, "package app\n\nfunc Deeper() {}\n", false},
		{siteNode, "Speak", model.NodeMethod, "package app\n\n// the open call site v.Speak()\n", false},
		{siteTarget, "Speak", model.NodeMethod, "package zoo\n\nfunc (Dog) Speak() {}\n", false},
	}
}

func openSiteScope(fx *contextFixture) []model.Relation {
	return []model.Relation{
		fx.edge(siteCaller, model.RelCalls, siteHelper),
		fx.edge(siteHelper, model.RelCalls, siteDeep),
		fx.edge(siteDeep, model.RelCalls, siteDeeper),
		fx.edge(siteCaller, model.RelCalls, siteNode),
	}
}

// openSiteEngine is scopeEngine with node metadata: the open-site node carries
// the OpenSiteMetadata the compile identifies it by. scopeEngine builds its
// nodes from the file table alone, which holds no metadata.
func openSiteEngine(fx *contextFixture, meta map[model.NodeID]json.RawMessage) *graph.Engine {
	fx.t.Helper()
	adj := &scopeAdjacency{binding: fx.Binding, nodes: map[model.NodeID]model.Node{},
		relations: append([]model.Relation(nil), fx.Rels...), caps: fixtureCapabilities}
	for _, spec := range fx.Specs {
		fv := fx.File(spec.path)
		id := fx.Node(spec.path)
		adj.nodes[id] = model.Node{ID: id, Kind: spec.kind, Language: "go", Name: spec.symbol,
			QualifiedName: spec.path + "." + spec.symbol, FileID: fv.ID, ContentHash: fv.ContentHash,
			Metadata: meta[id]}
	}
	sort.Slice(adj.relations, func(i, j int) bool { return adj.relations[i].ID < adj.relations[j].ID })
	dir := fx.t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		fx.t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"),
		fx.Cfg.Resources.MaxTempBytes/fixtureSpoolBudgetDivisor, fx.Store)
	if err != nil {
		fx.t.Fatalf("NewSpools: %v", err)
	}
	eng, err := graph.New(graph.Options{Adjacency: adj, Reader: scopeMemoryGraph(adj),
		Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(fx.Store, pagination.DefaultCursorTTL),
		Limits: graph.Limits{
			MaxDepth:       fx.Cfg.Context.MaxGraphDepth.Int(),
			MaxVisited:     fx.Cfg.Context.MaxVisitedNodes.Int(),
			MaxEdges:       fx.Cfg.Context.MaxGraphEdges.Int(),
			MaxPageItems:   fx.Cfg.Resources.MaxPageItems,
			MaxReasonPaths: fx.Cfg.Context.MaxReasonPathsPerEntry.Int(),
			QueryTimeout:   fx.Cfg.Resources.QueryTimeout.Std(),
			CursorTTL:      fx.Cfg.Storage.QueryCursorTTL.Std(),
			FrontierBytes:  fx.Cfg.Resources.QueryMemoryBytes,
		}})
	if err != nil {
		fx.t.Fatalf("graph.New: %v", err)
	}
	return eng
}

// TestNameOnlyCandidatesAreNeverRequiredAndPackAfterEveryPreciseEntry pins the
// name-only candidate tier (ADR-0015 decision 3): the candidate of an open call
// site joins the plan as optional context, after every precise entry, never as
// required scope, and the manifest says so.
//
// Mutation it catches: add `may_call` to scopeRelations. The walk then crosses
// the candidate edge as a precise boundary, the candidate takes a precise
// requirement from its depth, and it is ordered among the precise entries.
//
// It runs against the real candidate lookup, with no stand-in, so it exercises
// the tier once that lookup answers; while the lookup answers unavailable it
// fails naming the unavailable notice.
func TestNameOnlyCandidatesAreNeverRequiredAndPackAfterEveryPreciseEntry(t *testing.T) {
	fx := newFixtureFrom(t, openSiteSpecs(), openSiteScope)
	site, err := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReceiverUntyped, Shape: model.CallShapeMember, Arguments: 0})
	if err != nil {
		t.Fatalf("marshal open-site metadata: %v", err)
	}
	meta := map[model.NodeID]json.RawMessage{fx.Node(siteNode): site}
	c, err := New(Options{
		Store: fx.Store, Repo: fx.Repo, Search: searchService(t, fx),
		Graph: func(_ stdcontext.Context, gen model.GenerationID) (*graph.Engine, func() error, error) {
			if gen != fx.Gen {
				t.Fatalf("the graph factory was asked for generation %d, want the pinned %d", gen, fx.Gen)
			}
			return openSiteEngine(fx, meta), func() error { return nil }, nil
		},
		Config: fx.Cfg, Now: fx.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		SortDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	m, err := c.Compile(fx.ctx, model.ContextRequest{Task: "harden `Run`", Phase: model.PhaseVerify})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	for _, n := range m.Notices {
		if strings.Contains(n, "name-only call candidates are unavailable") {
			t.Fatalf("the candidate lookup is unavailable (%q), so the tier this test pins was not exercised", n)
		}
	}
	entries, _, _ := readBackManifest(t, fx, m.ID)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Ordinal < entries[j].Ordinal })

	target := fx.Node(siteTarget)
	at := -1
	lastPrecise := -1
	for i, e := range entries {
		if e.NodeID == target {
			at = i
			continue
		}
		lastPrecise = i
	}
	if at < 0 {
		t.Fatalf("the open call site's one name-only candidate is not in the plan; entries: %+v", entries)
	}
	got := entries[at]
	if got.Requirement != model.RequirementOptional {
		t.Errorf("the name-only candidate is %s, want %s: a candidate may not run, so it is never more than optional",
			got.Requirement, model.RequirementOptional)
	}
	if at < lastPrecise {
		t.Errorf("the name-only candidate is entry %d of %d, before the precise entry %d; every precise entry packs first",
			at, len(entries), lastPrecise)
	}
	// The precise optional entry is what makes the order observable: a
	// candidate ranked by requirement alone could sit among the optional
	// precise entries.
	sawPreciseOptional := false
	for i, e := range entries {
		if i == at {
			continue
		}
		if e.NodeID == fx.Node(siteDeeper) && e.Requirement == model.RequirementOptional {
			sawPreciseOptional = true
		}
	}
	if !sawPreciseOptional {
		t.Errorf("the plan holds no optional precise entry for %s, so the candidate's place after the precise tier is unproven",
			siteDeeper)
	}
	for _, e := range entries {
		if e.NodeID == target && isRequired(e.Requirement) {
			t.Errorf("a name-only candidate is required scope (%s)", e.Requirement)
		}
	}
	notice := false
	for _, n := range m.Notices {
		if strings.Contains(n, "name-only call candidate(s)") && strings.Contains(n, "none is required scope") {
			notice = true
		}
	}
	if !notice {
		t.Errorf("the manifest does not say how many name-only candidates it admitted; notices: %q", m.Notices)
	}
}
