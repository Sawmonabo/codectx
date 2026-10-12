package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// rowDecl is one unit's declaration of one node in the rows fixture: the unit's
// scope and source binding, and the facts that unit publishes. Declarations
// sharing a key are ONE node that several units disagree about.
type rowDecl struct {
	scope         string
	binding       model.SourceBinding
	key           string
	name          string
	qualifiedName string
	// body, when set, is a search document the unit publishes for the node.
	body string
}

// rowsFixture is an activated generation over one file whose nodes are declared
// by the units rowDecl lists.
type rowsFixture struct {
	ctx   context.Context
	opts  Options
	nodes map[string]model.NodeID
}

// newRowsFixture publishes decls, one unit each, over one file, and activates
// the generation.
func newRowsFixture(t *testing.T, decls []rowDecl) *rowsFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "codectx.db"), storeOptions(false))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := model.RepositoryID(model.H("rows-fixture", "1"))
	if err := st.EnsureRepository(ctx, repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	cas, err := snapshot.OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	content := "package pkg\n" + strings.Repeat("// padding\n", 20)
	rec, err := cas.Put(ctx, strings.NewReader(content))
	if err != nil {
		t.Fatalf("CAS.Put: %v", err)
	}
	if err := st.PutBlob(ctx, rec); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	const path = "pkg/rows.go"
	fileID := model.NewFileID(repo, path)
	fv := model.FileVersion{ID: fileID, Path: path, Status: model.FileTracked,
		Size: int64(len(content)), ContentHash: rec.Hash, Language: "go"}
	manifest := model.H("rows-manifest")
	snap := model.Snapshot{ID: model.NewSnapshotID(repo, "", model.H("policy"), manifest), RepositoryID: repo,
		CaptureConsistency: model.CaptureValidated, SourcePolicyHash: model.H("policy"), FileCount: 1,
		ManifestHash: manifest, SourceBytes: uint64(len(content)), CreatedAt: time.Now().UTC()}
	err = st.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error { return yield(fv) })
	if err != nil {
		t.Fatalf("PutSnapshot: %v", err)
	}
	gen, err := st.BeginGeneration(ctx, repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := st.BeginProviderRun(ctx, gen, fixtureProviderID, fixtureProviderVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	f := &rowsFixture{ctx: ctx, nodes: map[string]model.NodeID{}}
	for _, d := range decls {
		key := model.CanonicalNodeKey(path, d.key)
		nodeID := model.NewNodeID(repo, model.NodeFunction, key)
		f.nodes[d.key] = nodeID
		input := model.UnitInput{FileID: fileID, ContentHash: rec.Hash}
		h := model.NewUnitInputHasher()
		if err := h.Add(input); err != nil {
			t.Fatalf("UnitInputHasher.Add: %v", err)
		}
		spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
			ScopeKey: d.scope, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
		spec.ID = model.NewUnitID(spec, fixtureConfigHash)
		build := model.UnitBuild{Spec: spec, AnalysisConfigHash: fixtureConfigHash, OriginRunID: run,
			SourceBinding: d.binding}
		w, err := st.BeginUnit(ctx, gen, build,
			func(yield func(model.UnitInput) error) error { return yield(input) })
		if err != nil {
			t.Fatalf("BeginUnit(%s): %v", d.scope, err)
		}
		rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1},
			End: model.Position{Byte: 10, Line: 1, Column: 10}}
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: fixtureProviderID,
			ProviderVersion: fixtureProviderVersion, OriginRunID: run, NodeID: nodeID,
			Precision: model.PrecisionSyntax, FileID: fileID, ContentHash: rec.Hash, Range: rng}
		ev.ID = model.NewEvidenceID(ev)
		node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: d.name,
			QualifiedName: d.qualifiedName, Signature: "func " + d.name + "()", FileID: fileID,
			ContentHash: rec.Hash, Range: rng}
		if err := w.PutNodes(ctx, []model.NodeFact{{Node: node, CanonicalKey: key,
			Evidence: []model.Evidence{ev}}}); err != nil {
			t.Fatalf("PutNodes(%s): %v", d.scope, err)
		}
		if d.body != "" {
			if err := w.PutSearchUnits(ctx, []model.SearchUnit{{
				ID: model.H("rows-doc", d.scope), NodeID: nodeID, FileID: fileID, Path: path,
				Kind: model.NodeFunction, Name: d.name, QualifiedName: d.qualifiedName,
				Signature: node.Signature, Bytes: model.ByteRange{Start: 0, End: 10}, Body: d.body,
			}}); err != nil {
				t.Fatalf("PutSearchUnits(%s): %v", d.scope, err)
			}
		}
		if err := st.SealUnit(ctx, w); err != nil {
			t.Fatalf("SealUnit(%s): %v", d.scope, err)
		}
	}
	if _, err := st.Activate(ctx, gen, 0, model.HealthFresh,
		[]model.CapabilityState{{ProviderID: fixtureProviderID, Capability: "structure", Scope: "workspace",
			State: model.CapabilityFresh}}, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	f.opts = Options{Store: st, Repo: repo, Signer: signer,
		Leases: pagination.NewLeases(st, pagination.DefaultCursorTTL), Content: cas,
		Resources: config.Defaults().Resources, CursorTTL: pagination.DefaultCursorTTL,
		Now: func() time.Time { return time.Now().UTC() }}
	return f
}

// walkAll pages a query to its end at limit 1 and answers every hit served.
func walkAll(t *testing.T, ctx context.Context, s *Service, query string) []model.SearchHit {
	t.Helper()
	req := model.SearchRequest{Query: query, Page: model.PageRequest{Limit: 1}}
	var out []model.SearchHit
	for page := 1; ; page++ {
		if page > 50 {
			t.Fatalf("%q: the walk did not end after %d pages", query, page-1)
		}
		got, err := s.Search(ctx, req)
		if err != nil {
			t.Fatalf("%q: Search(page %d): %v", query, page, err)
		}
		out = append(out, got.Items...)
		if got.Meta.NextCursor == "" {
			return out
		}
		req.Page.Cursor = got.Meta.NextCursor
	}
}

// rowsDecls is the corpus of the tier-rule tests. "Twice" is one node two
// units disagree about: the verified unit (the precedence winner) names it
// "Twice" with the qualified name "pkg.Twice", the unverified one qualifies it
// "Twice". So for the query "Twice" it matches exact_qualified_name through the
// losing row and exact_name through the winning row, whose qualified name the
// query does not prefix. "Other" is a second node, of qualified_name_prefix,
// that sorts between those two tiers. "Solo" is the same disagreement for the
// query "Mixed.Solo": exact_qualified_name matches the unverified row only,
// and the search document comes from the verified unit, whose fields match no
// exact tier.
var rowsDecls = []rowDecl{
	{scope: "twice-strict", binding: model.SourceBindingVerified, key: "Twice", name: "Twice",
		qualifiedName: "pkg.Twice"},
	{scope: "twice-loose", binding: model.SourceBindingUnverified, key: "Twice", name: "Twice",
		qualifiedName: "Twice"},
	{scope: "other", binding: model.SourceBindingVerified, key: "Other", name: "Other",
		qualifiedName: "Twice.Other"},
	{scope: "solo-strict", binding: model.SourceBindingVerified, key: "Solo", name: "Solo",
		qualifiedName: "pkg.Solo", body: "func Solo() { mixed solo }"},
	{scope: "solo-loose", binding: model.SourceBindingUnverified, key: "Solo", name: "Solo",
		qualifiedName: "Mixed.Solo"},
}

// TestANodeIsServedOnceAcrossExactTierPages proves the tier of a node is a
// function of the pinned generation, not of the page: a node whose units match
// exact_qualified_name through one row and exact_name through another is served
// once, at the earlier tier, however the pages fall. At limit 1 the two
// matches land on pages 1 and 3, with "Other" on page 2 between them.
//
// Mutation: decide the repeat per page (drop tierRule.earlier's tier reads and
// keep only the row-in-hand predicates, as a per-page heap dedupe would) and the
// node is served again at exact_name on page 3.
func TestANodeIsServedOnceAcrossExactTierPages(t *testing.T) {
	f := newRowsFixture(t, rowsDecls)
	s := newService(t, f.opts)
	hits := walkAll(t, f.ctx, s, "Twice")
	var twice []model.SearchTier
	other := 0
	for _, h := range hits {
		switch h.NodeID {
		case f.nodes["Twice"]:
			twice = append(twice, h.Tier)
		case f.nodes["Other"]:
			other++
			if h.Tier != model.TierQualifiedNamePrefix {
				t.Errorf("Other is served at %q, want %q", h.Tier, model.TierQualifiedNamePrefix)
			}
		}
	}
	if len(twice) != 1 || twice[0] != model.TierExactQualifiedName {
		t.Fatalf("the node two units disagree about is served at %v, want once at %q", twice,
			model.TierExactQualifiedName)
	}
	if other != 1 {
		t.Fatalf("Other is served %d times, want once", other)
	}
}

// TestALexicalResultOfAnExactlyServedNodeIsFolded proves the lexical fold asks
// every visible row of a node, not the precedence winner and not the result's
// own document: "Solo" is served at exact_qualified_name through its unverified
// row, while its search document and its winning row carry "pkg.Solo", which
// matches no exact tier for the query "Mixed.Solo".
//
// Mutation: confirm the fold against reader.Node (the precedence winner) and
// the result's own fields, and the node is served a second time, lexically.
func TestALexicalResultOfAnExactlyServedNodeIsFolded(t *testing.T) {
	f := newRowsFixture(t, rowsDecls)
	s := newService(t, f.opts)
	hits := walkAll(t, f.ctx, s, "Mixed.Solo")
	var tiers []model.SearchTier
	for _, h := range hits {
		if h.NodeID == f.nodes["Solo"] {
			tiers = append(tiers, h.Tier)
		}
	}
	if len(tiers) != 1 || tiers[0] != model.TierExactQualifiedName {
		t.Fatalf("Solo is served at %v, want once at %q", tiers, model.TierExactQualifiedName)
	}

	// The document does match the query lexically: without an exact tier to
	// fold it into, it is served. This is what makes the fold above observable.
	lexical := walkAll(t, f.ctx, s, "mixed solo")
	found := false
	for _, h := range lexical {
		if h.NodeID == f.nodes["Solo"] && h.Tier == model.TierLexical {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Solo document is not a lexical match of its own words: %+v", lexical)
	}
}
