package search

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
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
	// path is the file the unit declares the node in; empty is rowsPath.
	path string
}

// rowsPath is the file a rowDecl declares its node in unless it names another.
// A node's identity is keyed on it whichever file a unit declares the node in,
// so two units can disagree about the file as well as the names.
const rowsPath = "pkg/rows.go"

// rowsFixture is an activated generation over the files whose nodes are
// declared by the units rowDecl lists.
type rowsFixture struct {
	ctx   context.Context
	opts  Options
	nodes map[string]model.NodeID
}

// newRowsFixture publishes decls, one unit each, and activates the generation.
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
	var files []model.FileVersion
	for _, d := range decls {
		p := cmp.Or(d.path, rowsPath)
		if !slices.ContainsFunc(files, func(fv model.FileVersion) bool { return fv.Path == p }) {
			files = append(files, model.FileVersion{ID: model.NewFileID(repo, p), Path: p, Status: model.FileTracked,
				Size: int64(len(content)), ContentHash: rec.Hash, Language: "go"})
		}
	}
	slices.SortFunc(files, func(a, b model.FileVersion) int { return cmp.Compare(a.Path, b.Path) })
	manifest := model.H("rows-manifest")
	snap := model.Snapshot{ID: model.NewSnapshotID(repo, "", model.H("policy"), manifest), RepositoryID: repo,
		CaptureConsistency: model.CaptureValidated, SourcePolicyHash: model.H("policy"),
		FileCount: uint64(len(files)), ManifestHash: manifest, SourceBytes: uint64(len(content) * len(files)),
		CreatedAt: time.Now().UTC()}
	err = st.PutSnapshot(ctx, snap, func(yield func(model.FileVersion) error) error {
		for _, fv := range files {
			if err := yield(fv); err != nil {
				return err
			}
		}
		return nil
	})
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
		path := cmp.Or(d.path, rowsPath)
		fileID := model.NewFileID(repo, path)
		key := model.CanonicalNodeKey(rowsPath, d.key)
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
// Mutation: decide the repeat per page (drop tierRule.ownedBefore's tier reads and
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

// filteredDecls is one node, "Thing", that two units disagree about: the
// unverified unit declares it under vendor/ with the qualified name "Thing"
// (exact_qualified_name for the query "Thing"), the verified unit under src/
// with the qualified name "pkg.Thing" and the name "Thing" (exact_name, whose
// row the verified unit wins). With the path filter src/, only exact_name can
// serve it. body, when set, gives the src/ unit a search document matching the
// query.
func filteredDecls(body string) []rowDecl {
	return []rowDecl{
		{scope: "thing-vendor", binding: model.SourceBindingUnverified, key: "Thing", name: "Thing",
			qualifiedName: "Thing", path: "vendor/thing.go"},
		{scope: "thing-src", binding: model.SourceBindingVerified, key: "Thing", name: "Thing",
			qualifiedName: "pkg.Thing", path: "src/thing.go", body: body},
	}
}

// TestAFilteredOutTierDoesNotOwnTheNode proves the tier rule reads the request's
// filters: a node whose exact_qualified_name row lies outside the path filter
// and whose exact_name row lies inside it is served once, at exact_name, inside
// the filter. With a matching search document the lexical fold must agree, so
// the node is served at exact_name and not lexically; without one, nothing but
// the exact tiers can serve it.
//
// Mutation: decide the tier without the request's filters, and the node is
// served lexically (with a document) or not at all (without one).
func TestAFilteredOutTierDoesNotOwnTheNode(t *testing.T) {
	for _, body := range []string{"func Thing() { thing }", ""} {
		f := newRowsFixture(t, filteredDecls(body))
		s := newService(t, f.opts)
		req := model.SearchRequest{Query: "Thing", Paths: []string{"src"}, Page: model.PageRequest{Limit: 1}}
		var tiers []model.SearchTier
		for page := 1; ; page++ {
			if page > 20 {
				t.Fatalf("document %q: the walk did not end", body)
			}
			got, err := s.Search(f.ctx, req)
			if err != nil {
				t.Fatalf("document %q: Search(page %d): %v", body, page, err)
			}
			for _, h := range got.Items {
				if h.NodeID == f.nodes["Thing"] {
					tiers = append(tiers, h.Tier)
					if h.Path != "src/thing.go" {
						t.Errorf("document %q: Thing is served at %q, outside the path filter", body, h.Path)
					}
				}
			}
			if got.Meta.NextCursor == "" {
				break
			}
			req.Page.Cursor = got.Meta.NextCursor
		}
		if len(tiers) != 1 || tiers[0] != model.TierExactName {
			t.Fatalf("document %q: Thing is served at %v, want once at %q", body, tiers, model.TierExactName)
		}
	}
}

// pagingDocs is a corpus of 24 files, one function each. Every body holds
// "omega" and "zeta", in four repeat counts, so the scores fall into tied runs
// that page boundaries split. Every third function is named "zeta": for the
// query "zeta" the exact_name tier serves those, and the exact fold drops their
// lexical results, which the service filters after the index has answered.
func pagingDocs() []doc {
	var out []doc
	for i := range 24 {
		name := fmt.Sprintf("Fn%02d", i)
		if i%3 == 0 {
			name = "zeta"
		}
		body := "func " + name + "() { " + strings.Repeat("omega zeta ", 1+i%4) + "}"
		out = append(out, doc{path: fmt.Sprintf("pkg/f%02d.go", i), name: name,
			qualifiedName: fmt.Sprintf("pkg.F%02d.%s", i, name), signature: "func " + name + "()",
			body: body, occurrences: 1})
	}
	return out
}

// pagedRead is one page of a walk: the hits it served and every request
// lexicalPhase sent the index for it.
type pagedRead struct {
	items []model.SearchHit
	reqs  []textindex.Request
}

// pagedReads walks query at limit to its end and answers the hits served and
// each page's read.
func pagedReads(t *testing.T, ctx context.Context, s *Service, query string, limit int) ([]model.SearchHit, []pagedRead) {
	t.Helper()
	var current []textindex.Request
	prev := observeIndexRequest
	observeIndexRequest = func(r textindex.Request) { current = append(current, r) }
	defer func() { observeIndexRequest = prev }()
	req := model.SearchRequest{Query: query, Page: model.PageRequest{Limit: limit}}
	var hits []model.SearchHit
	var pages []pagedRead
	for page := 1; ; page++ {
		if page > 100 {
			t.Fatalf("%q: the walk did not end", query)
		}
		current = nil
		got, err := s.Search(ctx, req)
		if err != nil {
			t.Fatalf("%q: Search(page %d): %v", query, page, err)
		}
		hits = append(hits, got.Items...)
		pages = append(pages, pagedRead{items: got.Items, reqs: current})
		if got.Meta.NextCursor == "" {
			return hits, pages
		}
		req.Page.Cursor = got.Meta.NextCursor
	}
}

// TestLexicalPagesResumeByKey proves a lexical page reads only what follows the
// previous page: every page's first index request asks for at most limit+1
// results after the cursor's key, a request repeats only while the service's
// own filters leave the page short (then resuming after what the last request
// returned, at twice its depth), and the pages together are exactly the answer
// one deep read serves.
//
// Mutation: page by depth (ask for served+limit+1 from the start of the
// ranking) and the second page's request asks for 2·limit+1 with no position.
// Mutation: resume after the last result read instead of the last one served,
// and the result past each page is lost from the walk.
//
// A page's first request carries no resume position exactly when no lexical
// result was served before the page: the page that starts the lexical tier, and
// a page the exact tiers filled exactly (it asks the index for one result only
// to learn whether a lexical page follows) together with the page after it.
// "zeta" at limits 2 and 4 puts that edge on a page boundary, at 3 and 5 inside
// a page.
func TestLexicalPagesResumeByKey(t *testing.T) {
	f := newFixtureOf(t, pagingDocs())
	s := newService(t, f.opts)
	for _, c := range []struct {
		query string
		limit int
		// filtered says the exact fold drops lexical results of this query.
		filtered bool
	}{{"omega", 4, false}, {"omega", 1, false}, {"zeta", 3, true}, {"zeta", 5, true},
		{"zeta", 2, true}, {"zeta", 4, true}} {
		deep, err := s.Search(f.ctx, model.SearchRequest{Query: c.query, Page: model.PageRequest{Limit: 200}})
		if err != nil {
			t.Fatalf("%q: Search(deep): %v", c.query, err)
		}
		if deep.Meta.NextCursor != "" || len(deep.Items) != 24 {
			t.Fatalf("%q: the deep read served %d hits (continued %v), want all 24 nodes on one page",
				c.query, len(deep.Items), deep.Meta.NextCursor != "")
		}
		hits, pages := pagedReads(t, f.ctx, s, c.query, c.limit)
		if !reflect.DeepEqual(hits, deep.Items) {
			for i := range min(len(hits), len(deep.Items)) {
				if !reflect.DeepEqual(hits[i], deep.Items[i]) {
					t.Fatalf("%q limit %d: paged hit %d = %+v, want %+v", c.query, c.limit, i, hits[i], deep.Items[i])
				}
			}
			t.Fatalf("%q limit %d: the pages served %d hits, the deep read %d", c.query, c.limit, len(hits), len(deep.Items))
		}
		lexicalPages := 0
		servedLexical := false
		for p, pr := range pages {
			reqs := pr.reqs
			before := servedLexical
			for _, h := range pr.items {
				if h.Tier == model.TierLexical {
					servedLexical = true
				}
			}
			if len(reqs) == 0 {
				continue
			}
			lexicalPages++
			if reqs[0].Depth > c.limit+1 {
				t.Fatalf("%q limit %d page %d: the first index request asks for %d results, over limit+1",
					c.query, c.limit, p+1, reqs[0].Depth)
			}
			if before != (reqs[0].After != nil) {
				t.Fatalf("%q limit %d page %d: the first request resumes after %v with lexical results "+
					"served before the page = %v; a page resumes exactly when it follows a served lexical result",
					c.query, c.limit, p+1, reqs[0].After, before)
			}
			if !c.filtered && (len(reqs) != 1 || reqs[0].Depth != c.limit+1) {
				t.Fatalf("%q limit %d page %d: with nothing filtered a page is one request of limit+1, got %+v",
					c.query, c.limit, p+1, reqs)
			}
			for i := 1; i < len(reqs); i++ {
				if reqs[i].Depth != 2*reqs[i-1].Depth || reqs[i].After == nil {
					t.Fatalf("%q limit %d page %d: request %d asks for %d after %v; a repeat resumes after the "+
						"previous request at twice its depth %d", c.query, c.limit, p+1, i, reqs[i].Depth,
						reqs[i].After, reqs[i-1].Depth)
				}
			}
		}
		if lexicalPages < 2 {
			t.Fatalf("%q limit %d: only %d pages read the lexical tier; the walk proves nothing about resuming",
				c.query, c.limit, lexicalPages)
		}
	}
}
