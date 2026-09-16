package search

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

// The Section 14 ranking fixture. Three small lexical documents in one
// activated generation are the smallest corpus that can separate the exact
// tiers from lexical BM25, exercise an indexed prefix range, carry a Unicode
// identifier and a literal-punctuation body through the FTS encoder, and fold
// two occurrences of one node into one hit. Every Task 13 lane asserts against
// this one corpus so a ranking change cannot be hidden behind a private
// fixture.
const (
	fixtureProviderID      = "treesitter"
	fixtureProviderVersion = "1.0.0"
	fixtureConfigHash      = "cfg"
)

// doc describes one fixture document: its file, its node identity and the
// indexed text BM25 scores. Occurrences > 1 publishes that many search
// documents for the same node, which is what the deduplication leg folds.
type doc struct {
	path          string
	name          string
	qualifiedName string
	signature     string
	body          string
	occurrences   int
}

// fixtureDocs is the corpus. "Handle" and "HandleRequest" share the "pkg.Beta"
// qualified-name prefix; the third document carries a Unicode identifier and a
// literal-punctuation call in its body.
var fixtureDocs = []doc{
	{path: "pkg/alpha.go", name: "Handle", qualifiedName: "pkg.Alpha.Handle",
		signature: "func Handle(ctx context.Context) error",
		body:      "func Handle(ctx context.Context) error { return dispatch(ctx) }", occurrences: 2},
	{path: "pkg/beta.go", name: "HandleRequest", qualifiedName: "pkg.Beta.HandleRequest",
		signature: "func HandleRequest(r *Request) error",
		body:      "func HandleRequest(r *Request) error { return handle(r) }", occurrences: 1},
	{path: "pkg/café.go", name: "café", qualifiedName: "pkg.Uni.café",
		signature: "func café() string",
		body:      "func café() string { return foo(bar) }", occurrences: 1},
}

// fixture is one activated generation over fixtureDocs, plus the pagination
// machinery a Service needs.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	store   *sqlite.Store
	dbPath  string
	repo    model.RepositoryID
	gen     model.GenerationID
	binding model.Binding
	opts    Options
	nodes   map[string]model.NodeID
	files   map[string]model.FileID
	hashes  map[string]string
}

// newFixture builds and activates the corpus. It runs at every lane's entry so
// a corpus that no longer publishes is a failure here, not five lanes later.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "codectx.db")
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, ctx: ctx, store: st, dbPath: dbPath, repo: model.RepositoryID(model.H("search-fixture", "1")),
		nodes: map[string]model.NodeID{}, files: map[string]model.FileID{}}
	if err := st.EnsureRepository(ctx, f.repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}

	// The bodies live in a real content store: SearchHit.Range is hydrated
	// from the CAS through source.Cursor, so a fixture that only recorded blob
	// metadata would prove a nil Range rather than a served source position.
	cas, err := snapshot.OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	hashes := make(map[string]string, len(fixtureDocs))
	f.hashes = hashes
	files := make([]model.FileVersion, 0, len(fixtureDocs))
	for _, d := range fixtureDocs {
		rec, err := cas.Put(ctx, strings.NewReader(d.body))
		if err != nil {
			t.Fatalf("CAS.Put(%s): %v", d.path, err)
		}
		hash := rec.Hash
		hashes[d.path] = hash
		id := model.NewFileID(f.repo, d.path)
		f.files[d.path] = id
		f.nodes[d.path] = model.NewNodeID(f.repo, model.NodeFunction, model.CanonicalNodeKey(d.path, d.name))
		if err := st.PutBlob(ctx, rec); err != nil {
			t.Fatalf("PutBlob(%s): %v", d.path, err)
		}
		files = append(files, model.FileVersion{ID: id, Path: d.path, Status: model.FileTracked,
			Size: int64(len(d.body)), ContentHash: hash, Language: "go"})
	}

	manifest := model.H("search-fixture-manifest")
	snap := model.Snapshot{
		ID:                 model.NewSnapshotID(f.repo, "", model.H("policy"), manifest),
		RepositoryID:       f.repo,
		CaptureConsistency: model.CaptureValidated,
		SourcePolicyHash:   model.H("policy"),
		FileCount:          uint64(len(files)),
		ManifestHash:       manifest,
		CreatedAt:          time.Now().UTC(),
	}
	for _, fv := range files {
		snap.SourceBytes += uint64(fv.Size)
	}
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

	f.gen, err = st.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := st.BeginProviderRun(ctx, f.gen, fixtureProviderID, fixtureProviderVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	for _, d := range fixtureDocs {
		f.publish(f.gen, run, d, hashes[d.path])
	}
	f.binding, err = st.Activate(ctx, f.gen, 0, model.HealthFresh,
		[]model.CapabilityState{{ProviderID: fixtureProviderID, Capability: "structure", Scope: "workspace", State: model.CapabilityFresh}},
		"norm-v1")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"), 1<<20, st)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	f.opts = Options{Store: st, Repo: f.repo, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(st, pagination.DefaultCursorTTL), Content: cas,
		Resources: config.Defaults().Resources, CursorTTL: pagination.DefaultCursorTTL,
		Now: func() time.Time { return time.Now().UTC() }}
	return f
}

// publish seals one file-scoped unit carrying d's node fact and its lexical
// documents into the staging generation gen.
func (f *fixture) publish(gen model.GenerationID, run model.ProviderRunID, d doc, hash string) {
	f.t.Helper()
	fileID := f.files[d.path]
	nodeID := f.nodes[d.path]
	input := model.UnitInput{FileID: fileID, ContentHash: hash}
	h := model.NewUnitInputHasher()
	if err := h.Add(input); err != nil {
		f.t.Fatalf("UnitInputHasher.Add(%s): %v", d.path, err)
	}
	spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		ScopeKey: d.path, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, fixtureConfigHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: fixtureConfigHash, OriginRunID: run,
		SourceBinding: model.SourceBindingVerified}
	w, err := f.store.BeginUnit(f.ctx, gen, build,
		func(yield func(model.UnitInput) error) error { return yield(input) })
	if err != nil {
		f.t.Fatalf("BeginUnit(%s): %v", d.path, err)
	}

	end := uint64(len(d.body))
	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1},
		End: model.Position{Byte: end, Line: 1, Column: uint32(end)}}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		OriginRunID: run, NodeID: nodeID, Precision: model.PrecisionSyntax, FileID: fileID,
		ContentHash: hash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: d.name,
		QualifiedName: d.qualifiedName, Signature: d.signature, FileID: fileID, ContentHash: hash, Range: rng}
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node,
		CanonicalKey: model.CanonicalNodeKey(d.path, d.name), Evidence: []model.Evidence{ev}}}); err != nil {
		f.t.Fatalf("PutNodes(%s): %v", d.path, err)
	}

	units := make([]model.SearchUnit, 0, d.occurrences)
	for i := range d.occurrences {
		units = append(units, model.SearchUnit{
			ID: model.H("search-fixture-doc", d.path, hash, string(rune('a'+i))), NodeID: nodeID,
			FileID: fileID, Path: d.path, Kind: model.NodeFunction, Name: d.name,
			QualifiedName: d.qualifiedName, Signature: d.signature,
			Bytes: model.ByteRange{Start: 0, End: end}, Body: d.body, TokenCount: 1,
		})
	}
	if err := w.PutSearchUnits(f.ctx, units); err != nil {
		f.t.Fatalf("PutSearchUnits(%s): %v", d.path, err)
	}
	if err := f.store.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", d.path, err)
	}
}

// newService builds the service under test over the fixture. The fill-in lanes
// call it; at the skeleton commit New is not implemented yet.
func newService(t *testing.T, o Options) *Service {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// leg is one scenario leg. Each Task 13 lane appends its legs under its own
// marker below, so the inserts never touch the same lines.
type leg struct {
	name string
	run  func(t *testing.T, f *fixture)
}

// TestSearchRankingScenario is the single Task 13 scenario: one corpus, one
// activated generation, every leg a lane contributes. It guards determinism of
// the Section 14.2 ranking -- a silent reordering or score drift would serve
// different context for identical inputs.
func TestSearchRankingScenario(t *testing.T) {
	legs := []leg{

		// L1 STORAGE rows

		// L2 EXACT rows

		// L3 LEXICAL rows

		// L4 RANK rows
		{"rank/less_orders_the_full_tie_break_chain", legLessChain},
		{"rank/quantization_is_the_only_float_boundary", legQuantization},
		{"rank/dedup_folds_occurrences_and_keeps_the_lowest_tier", legDedupFolds},
		{"rank/dedup_falls_back_to_path_and_start_byte", legDedupFileKey},
		{"rank/the_ranked_set_keeps_every_distinct_hit", legRankedSetLossless},
		{"rank/the_answer_does_not_depend_on_the_order_candidates_arrive", legFoldOrderIndependent},
		{"rank/the_ranked_set_is_bounded_by_its_run_budget", legRankedSetIsBoundedByItsRunBudget},
		{"cursor/a_streamed_continuation_walk_reproduces_the_whole_answer", legStreamedWalkParity},
		{"rank/reasons_stay_within_their_bounds", legReasonBounds},
		{"rank/range_hydration_reads_a_bounded_window", legRangeHydration},
		{"cursor/query_hash_binds_the_query_and_its_filters", legQueryHash},
		{"cursor/tampered_expired_and_foreign_cursors_are_rejected", legCursorRejections},
		{"cursor/resolve_pages_by_a_bounded_keyset", legResolveKeyset},
		{"scenario/search_serves_the_section_14_2_order", legEndToEndRanking},

		// FX-C13b rows
		{"fix/path_tier_walks_the_whole_keyset_and_filters_it", legPathTierLossless},
		{"fix/exact_tiers_hold_one_read_page", legExactTiersHoldOnePage},
		{"fix/exact_path_tier_owns_its_files_nodes", legExactPathTierOwnsItsFile},
		{"fix/a_continuation_carries_the_answer_level_truncation", legContinuationTruncation},
		{"fix/a_full_spool_ends_the_page_not_the_query", legSpoolExhaustionEndsThePage},
		{"fix/a_writerless_process_serves_one_page_and_mints_no_cursor", legReadOnlyServesOnePage},
		{"fix/a_writerless_symbol_query_pages_in_full", legWriterlessSymbolPagesInFull},
		{"fix/symbol_resolves_a_canonical_node_id", legSymbolByCanonicalID},

		// FX-H-G rows
		{"fix/a_clamped_page_bound_is_reported_on_the_answer", legPageClampIsReported},
		{"fix/the_query_deadline_ends_a_page_not_the_answer", legDeadlineEndsThePage},
		{"fix/an_unbounded_query_answers_in_full", legUnboundedQueryAnswersInFull},

		// FX-H-U rows
		{"fix/the_path_resolver_holds_one_read_page", legPathResolverHoldsOnePage},

		// FX-H-X1 rows
		{"fix/a_two_offset_node_is_served_at_the_precedence_winner",
			legTwoOffsetNodeServesThePrecedenceWinner},
	}
	f := newFixture(t)
	for _, l := range legs {
		t.Run(l.name, func(t *testing.T) { l.run(t, f) })
	}
}

// ---------------------------------------------------------------------------
// L4 RANK + CURSORS legs.
//
// The legs below guard the two invariants this lane owns. Ranking determinism:
// identical inputs must produce byte-identical ordering and byte-identical
// ScoreMicros on every release target, which is why every assertion here is an
// exact int64 and never "a is before b". Cursor integrity: a continuation must
// name the generation, query and endpoint it was minted for, or the caller
// silently reads page 2 of a different question.
// ---------------------------------------------------------------------------

// rankedOf builds a candidate with the sort tuple spelled out, so a leg that
// isolates one tie-break key differs from its partner in exactly that key.
func rankedOf(tier model.SearchTier, score int64, path string, start uint64, node model.NodeID, key string) ranked {
	return ranked{Tier: tier, ScoreMicros: score, Path: path, StartByte: start,
		NodeID: node, SearchKey: key, Occurrences: 1}
}

// scoredOf wraps a sort tuple as the record the served order compares, with
// the served facts a real candidate carries.
func scoredOf(r ranked, kind model.NodeKind, name string, end uint64) scored {
	return scored{ranked: r, Folded: r.ScoreMicros, Span: &model.ByteRange{Start: r.StartByte, End: end},
		Hit: model.SearchHit{NodeID: r.NodeID, Path: r.Path, Kind: kind, Name: name}}
}

// legLessChain proves the full Section 14.2 chain, one key at a time. A
// dropped or reordered key would silently reorder results for identical
// inputs, which is the determinism guarantee Section 14.2 sells. The keys
// after start byte are the content keys that make the order independent of
// WHERE the tree was indexed: end byte, kind, name, qualified name and
// signature all precede the root-dependent NodeID and SearchKey.
func legLessChain(t *testing.T, _ *fixture) {
	baseR := rankedOf(model.TierExactName, 100, "b", 10, "n2", "k2")
	base := scoredOf(baseR, model.NodeFunction, "m2", 20)
	cases := []struct {
		key   string
		lower scored
	}{
		{"tier", scoredOf(rankedOf(model.TierExactPath, 0, "b", 10, "n2", "k2"), model.NodeFunction, "m2", 20)},
		{"score", scoredOf(rankedOf(model.TierExactName, 101, "b", 10, "n2", "k2"), model.NodeFunction, "m2", 20)},
		{"path", scoredOf(rankedOf(model.TierExactName, 100, "a", 10, "n2", "k2"), model.NodeFunction, "m2", 20)},
		{"start_byte", scoredOf(rankedOf(model.TierExactName, 100, "b", 9, "n2", "k2"), model.NodeFunction, "m2", 20)},
		{"end_byte", scoredOf(baseR, model.NodeFunction, "m2", 19)},
		{"kind", scoredOf(baseR, model.NodeDocument, "m2", 20)},
		{"name", scoredOf(baseR, model.NodeFunction, "m1", 20)},
		{"qualified_name", withQName(scoredOf(baseR, model.NodeFunction, "m2", 20), "q1")},
		{"signature", withSignature(scoredOf(baseR, model.NodeFunction, "m2", 20), "s1")},
		{"node_id", scoredOf(rankedOf(model.TierExactName, 100, "b", 10, "n1", "k2"), model.NodeFunction, "m2", 20)},
		{"search_key", scoredOf(rankedOf(model.TierExactName, 100, "b", 10, "n2", "k1"), model.NodeFunction, "m2", 20)},
	}
	// The base carries a qualified name and a signature so the two cases that
	// isolate those keys have something to sort below.
	base = withSignature(withQName(base, "q2"), "s2")
	for i := range cases {
		if cases[i].key != "qualified_name" && cases[i].key != "signature" {
			cases[i].lower = withSignature(withQName(cases[i].lower, "q2"), "s2")
		}
		if cases[i].key == "qualified_name" {
			cases[i].lower = withSignature(cases[i].lower, "s2")
		}
		if cases[i].key == "signature" {
			cases[i].lower = withQName(cases[i].lower, "q2")
		}
	}
	for _, c := range cases {
		if cmpScored(c.lower, base) >= 0 {
			t.Errorf("%s: want the %s-lower candidate to sort first", c.key, c.key)
		}
		if cmpScored(base, c.lower) <= 0 {
			t.Errorf("%s: the order is not antisymmetric", c.key)
		}
	}
	// A tier with a worse score still outranks a better-scoring later tier:
	// tier is the FIRST key, not a tiebreak after score.
	exact := scoredOf(rankedOf(model.TierExactQualifiedName, 0, "z", 99, "n9", "k9"), model.NodeFunction, "m9", 100)
	lexical := scoredOf(rankedOf(model.TierLexicalFTS, 9_999_999, "a", 0, "n0", "k0"), model.NodeFunction, "m0", 1)
	if cmpScored(exact, lexical) >= 0 {
		t.Error("a zero-scored exact hit must outrank a high-scoring lexical hit")
	}
	if cmpScored(base, base) != 0 {
		t.Error("the order must be reflexive-equal on one record")
	}
	// A candidate with no span sorts below one that has one, and the order
	// stays total either way.
	noSpan := scoredOf(baseR, model.NodeFunction, "m2", 20)
	noSpan.Span = nil
	if cmpScored(noSpan, base) >= 0 {
		t.Error("a candidate without a span must sort before one that has an end byte")
	}
}

// withQName and withSignature set the served fields the tail of the order
// compares, which scoredOf leaves empty.
func withQName(s scored, q string) scored     { s.Hit.QualifiedName = q; return s }
func withSignature(s scored, v string) scored { s.Hit.Signature = v; return s }

// legFoldOrderIndependent is the certification F9 invariant in one package:
// the served answer must be a pure function of the candidates, never of the
// order they arrive in. A delta re-index walks the lexical tier in a different
// search_units.doc_id order than a fresh index of the same tree does -- a
// delta carries doc ids forward, a fresh index assigns them anew -- so any
// arrival-ordered choice makes the two indexes of one tree answer differently.
//
// The shape is the general one: several candidates share a deduplication key
// while differing in name, range and score, and one group spans two tiers --
// the case where folding the ACCUMULATED score makes the survivor
// arrival-ordered.
//
// Mutation: restore `keep.Hit, keep.Span = a.Hit, a.Span` in fold, or compare
// Folded instead of ScoreMicros in it, and this fails.
func legFoldOrderIndependent(t *testing.T, _ *fixture) {
	// Three candidates sharing one document node as their deduplication key,
	// plus a two-tier group: an exact-name hit and a lexical hit of one node.
	group := []scored{
		scoredOf(rankedOf(model.TierLexicalFTS, 900, "docs/skill.md", 0, "n-doc", "k-h1"), model.NodeDocument, "Skill: Reverse-Proxy Config Hunt", 34),
		scoredOf(rankedOf(model.TierLexicalFTS, 700, "docs/skill.md", 5474, "n-doc", "k-h2"), model.NodeDocument, "1. Map the config surface", 5504),
		scoredOf(rankedOf(model.TierLexicalFTS, 900, "docs/skill.md", 5771, "n-doc", "k-h3"), model.NodeDocument, "2. Config failure patterns", 5802),
		scoredOf(rankedOf(model.TierExactName, 0, "pkg/cfg.go", 12, "n-fn", "k-fn-x"), model.NodeFunction, "Config", 40),
		scoredOf(rankedOf(model.TierLexicalFTS, 5000, "pkg/cfg.go", 12, "n-fn", "k-fn-y"), model.NodeFunction, "Config", 40),
		scoredOf(rankedOf(model.TierLexicalFTS, 100, "pkg/other.go", 3, "n-o", "k-o"), model.NodeFunction, "configure", 20),
	}
	// Every permutation of six candidates is 720 orders. These five are chosen
	// so that EVERY member of each deduplication group leads its group in at
	// least one of them: an order that always offers the eventual survivor
	// first would pass even with the arrival-ordered fold restored.
	orders := [][]int{{0, 1, 2, 3, 4, 5}, {5, 4, 3, 2, 1, 0}, {2, 0, 4, 1, 5, 3}, {1, 3, 5, 0, 2, 4}, {4, 5, 0, 2, 3, 1}}
	var want []string
	for _, order := range orders {
		c := collectorFor(t)
		for _, i := range order {
			v := group[i]
			if err := c.add(v.ranked, hitFacts{hit: v.Hit, span: v.Span}, "matched the "+string(v.Tier)+" tier"); err != nil {
				t.Fatalf("order %v: adding a candidate: %v", order, err)
			}
		}
		var got []string
		for _, r := range resultsOf(t, c) {
			got = append(got, fmt.Sprintf("%s|%d|%s|%s|%d|%v", r.Tier, r.ScoreMicros, r.Hit.Path, r.Hit.Name, r.Occurrences, r.Reasons))
		}
		if want == nil {
			want = got
			if len(want) != 3 {
				t.Fatalf("the three deduplication keys folded to %d results: %v", len(want), want)
			}
			continue
		}
		if !slices.Equal(got, want) {
			t.Fatalf("arrival order %v served\n  %v\nbut the first order served\n  %v", order, got, want)
		}
	}
	// The two-tier group ranks first -- it keeps the LOWER tier rank -- and it
	// is served at the higher score, carrying the reasons of both sides in a
	// fixed order.
	if want[0] != "exact_name|5000|pkg/cfg.go|Config|2|[matched the exact_name tier matched the lexical_fts tier]" {
		t.Errorf("the two-tier group served %q, want the exact tier at the lexical score", want[0])
	}
	// The survivor of the Markdown group is decided by content, not by which
	// heading arrived first: highest score, then the earliest start byte.
	if want[1] != "lexical_fts|900|docs/skill.md|Skill: Reverse-Proxy Config Hunt|3|[matched the lexical_fts tier]" {
		t.Errorf("the Markdown group served %q, want the highest-scoring, earliest heading", want[1])
	}
}

// legQuantization proves the one float boundary. Ranking compares int64 only;
// a drifting or truncating quantizer would make two releases disagree on the
// order of two hits whose scores differ in the seventh decimal.
func legQuantization(t *testing.T, _ *fixture) {
	cases := []struct {
		score float64
		want  int64
	}{
		{0, 0},
		{1, 1_000_000},
		{0.0000005, 1},   // rounds half away from zero, never truncates to 0
		{0.00000049, 0},  // and the value just below it does not
		{-0.0000005, -1}, // symmetric, so a negative idf cannot round to +0
		{2.7182818284, 2_718_282},
	}
	for _, c := range cases {
		if got := quantizeScore(c.score); got != c.want {
			t.Errorf("quantizeScore(%v) = %d, want %d", c.score, got, c.want)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := quantizeScore(bad); got != 0 {
			t.Errorf("quantizeScore(%v) = %d, want 0: a non-finite score must not become an arbitrary ranking value", bad, got)
		}
	}
}

// legDedupFolds proves digest §4 deduplication: one entity, one hit, the
// lowest tier seen, the lexical score kept, occurrences summed. Folding after
// paging (or not at all) would serve one node twice inside one page and
// understate its occurrence count.
func legDedupFolds(t *testing.T, _ *fixture) {
	c := collectorFor(t)
	add(t, c, rankedOf(model.TierLexicalFTS, 4_200_000, "pkg/alpha.go", 0, "node-a", "doc-a1"), "lexical_fts match")
	add(t, c, rankedOf(model.TierExactName, 0, "pkg/alpha.go", 0, "node-a", "doc-a2"), "exact_name match")
	add(t, c, rankedOf(model.TierLexicalFTS, 1_000_000, "pkg/beta.go", 0, "node-b", "doc-b1"), "lexical_fts match")
	got := resultsOf(t, c)
	if len(got) != 2 {
		t.Fatalf("results = %d hits, want 2 after deduplication", len(got))
	}
	a := got[0]
	if a.NodeID != "node-a" {
		t.Fatalf("first hit is %q, want node-a", a.NodeID)
	}
	if a.Tier != model.TierExactName {
		t.Errorf("folded tier = %q, want %q: the survivor keeps the lowest tier rank seen", a.Tier, model.TierExactName)
	}
	if a.ScoreMicros != 4_200_000 {
		t.Errorf("folded ScoreMicros = %d, want 4200000: an exact hit that also matched lexically keeps that score", a.ScoreMicros)
	}
	if a.Occurrences != 2 {
		t.Errorf("folded Occurrences = %d, want 2", a.Occurrences)
	}
	if a.SearchKey != "doc-a2" {
		t.Errorf("folded SearchKey = %q, want doc-a2: the survivor is the candidate that sorts first", a.SearchKey)
	}
	if len(a.Reasons) != 2 {
		t.Errorf("folded Reasons = %v, want both sides' reasons", a.Reasons)
	}
	if got[1].ScoreMicros != 1_000_000 || got[1].Occurrences != 1 {
		t.Errorf("second hit = %+v, want the untouched beta candidate", got[1].ranked)
	}
}

// legDedupFileKey proves the fallback key. A file chunk carries no node id, so
// keying only on NodeID would fold every bodiless chunk of every file into one
// hit.
func legDedupFileKey(t *testing.T, _ *fixture) {
	c := collectorFor(t)
	add(t, c, rankedOf(model.TierLexicalFTS, 3, "pkg/alpha.go", 0, "", "doc-1"))
	add(t, c, rankedOf(model.TierLexicalFTS, 5, "pkg/alpha.go", 0, "", "doc-2"))
	add(t, c, rankedOf(model.TierLexicalFTS, 7, "pkg/alpha.go", 64, "", "doc-3"))
	add(t, c, rankedOf(model.TierLexicalFTS, 9, "pkg/beta.go", 0, "", "doc-4"))
	got := resultsOf(t, c)
	if len(got) != 3 {
		t.Fatalf("results = %d hits, want 3: (path,start) separates chunks, and folds only the pair that shares one", len(got))
	}
	want := []int64{9, 7, 5}
	for i, w := range want {
		if got[i].ScoreMicros != w {
			t.Errorf("hit %d ScoreMicros = %d, want %d", i, got[i].ScoreMicros, w)
		}
	}
	if got[2].Occurrences != 2 {
		t.Errorf("the two documents at pkg/alpha.go:0 folded to Occurrences = %d, want 2", got[2].Occurrences)
	}
}

// legRankedSetLossless proves the ranked set is lossless: the former bound of
// 10*model.MaxPageItems distinct results refused every new key past 2000 and
// discarded the tail of the corpus. 5000 distinct candidates, offered in an
// order unrelated to their rank, must ALL survive, in exactly the order an
// in-memory oracle sorted by the Section 14.2 chain produces, and the answer
// must not claim to be truncated.
//
// Mutation: restore the refusal in collector.add and this fails on the count.
func legRankedSetLossless(t *testing.T, _ *fixture) {
	const distinct = 5000
	oracle := make([]ranked, 0, distinct)
	c := collectorFor(t)
	// A stride coprime with distinct offers the candidates in an order that
	// shares no prefix with the ranked order, so a collector that silently
	// kept only a prefix of what it was offered cannot pass.
	for i := range distinct {
		n := (i * 2237) % distinct
		r := rankedOf(model.TierLexicalFTS, int64(n), "pkg/f"+strconv.Itoa(n%97)+".go", uint64(n),
			model.NodeID("n"+strconv.Itoa(n)), "k"+strconv.Itoa(n))
		oracle = append(oracle, r)
		add(t, c, r, "lexical")
	}
	if truncated, reason := c.truncation(); truncated {
		t.Fatalf("a %d-result answer reported itself truncated: %s", distinct, reason)
	}
	got := resultsOf(t, c)
	if len(got) != distinct {
		t.Fatalf("results = %d, want every one of the %d distinct hits", len(got), distinct)
	}
	sort.Slice(oracle, func(i, j int) bool {
		return cmpScored(scored{ranked: oracle[i]}, scored{ranked: oracle[j]}) < 0
	})
	for i := range oracle {
		if got[i].ranked != oracle[i] {
			t.Fatalf("hit %d = %+v, want %+v (rank order differs from the oracle)", i, got[i].ranked, oracle[i])
		}
	}
	// A repeat of a key still folds rather than entering twice, at any size.
	again := collectorFor(t)
	for i := range distinct {
		n := (i * 2237) % distinct
		add(t, again, rankedOf(model.TierLexicalFTS, int64(n), "pkg/f"+strconv.Itoa(n%97)+".go", uint64(n),
			model.NodeID("n"+strconv.Itoa(n)), "k"+strconv.Itoa(n)), "lexical")
	}
	add(t, again, oracle[0], "exact")
	if got := resultsOf(t, again); len(got) != distinct {
		t.Fatalf("a repeated key grew the set to %d, want %d", len(got), distinct)
	}
}

// legReasonBounds proves the Section 14.3 explanation bounds survive folding.
// A folded hit with nine reasons, or one reason of a megabyte, fails
// model.SearchHit.Validate and takes the whole page down.
func legReasonBounds(t *testing.T, _ *fixture) {
	c := collectorFor(t)
	long := strings.Repeat("x", model.MaxReasonBytes+64)
	for i := range model.MaxReasonsPerEntry + 4 {
		add(t, c, rankedOf(model.TierLexicalFTS, 1, "p", 0, "n", "k"), "reason "+strconv.Itoa(i), long, "reason 0")
	}
	got := resultsOf(t, c)[0]
	if len(got.Reasons) != model.MaxReasonsPerEntry {
		t.Fatalf("Reasons = %d entries, want the bound %d", len(got.Reasons), model.MaxReasonsPerEntry)
	}
	for i, r := range got.Reasons {
		if len(r) > model.MaxReasonBytes {
			t.Errorf("reason %d is %d bytes, over the %d-byte bound", i, len(r), model.MaxReasonBytes)
		}
	}
	if n := len(slices.Compact(slices.Clone(got.Reasons))); n != len(got.Reasons) {
		t.Errorf("Reasons repeats an entry: %v", got.Reasons)
	}
	hit := model.SearchHit{FileID: model.NewFileID("r", "p"), Path: "p", Kind: model.NodeFunction,
		Tier: model.TierLexicalFTS, Reasons: got.Reasons}
	if err := hit.Validate(); err != nil {
		t.Errorf("a folded hit does not validate: %v", err)
	}
}

// legQueryHash proves the cursor's query binding. A hash that ignored a filter
// would let a cursor minted for one filter set resume against another, serving
// page 2 of a different question under the same token.
func legQueryHash(t *testing.T, _ *fixture) {
	base := model.SearchRequest{Query: "handle request",
		Kinds:     []model.NodeKind{model.NodeFunction, model.NodeClass},
		Languages: []string{"go", "rust"}, Paths: []string{"pkg/", "cmd/"}}
	reordered := model.SearchRequest{Query: " handle   request ",
		Kinds:     []model.NodeKind{model.NodeClass, model.NodeFunction},
		Languages: []string{"rust", "go"}, Paths: []string{"cmd/", "pkg/"}}
	h := searchQueryHash(base)
	if !model.ValidHexID(h) {
		t.Fatalf("query hash %q is not a well-formed id; Cursor.Validate rejects it", h)
	}
	if got := searchQueryHash(reordered); got != h {
		t.Error("filter order or interior whitespace changed the query hash; one query must have one cursor identity")
	}
	changed := []struct {
		name string
		req  model.SearchRequest
	}{
		{"query", model.SearchRequest{Query: "handle requests", Kinds: base.Kinds, Languages: base.Languages, Paths: base.Paths}},
		{"kinds", model.SearchRequest{Query: base.Query, Kinds: []model.NodeKind{model.NodeFunction}, Languages: base.Languages, Paths: base.Paths}},
		{"languages", model.SearchRequest{Query: base.Query, Kinds: base.Kinds, Languages: []string{"go"}, Paths: base.Paths}},
		{"paths", model.SearchRequest{Query: base.Query, Kinds: base.Kinds, Languages: base.Languages, Paths: []string{"pkg/"}}},
	}
	for _, c := range changed {
		if searchQueryHash(c.req) == h {
			t.Errorf("changing %s did not change the query hash", c.name)
		}
	}
	// A filter group is separated, so "a\x00b" and "ab" are different queries.
	split := model.SearchRequest{Query: base.Query, Kinds: base.Kinds, Languages: base.Languages, Paths: []string{"pk", "g/cmd/"}}
	if searchQueryHash(split) == searchQueryHash(model.SearchRequest{Query: base.Query, Kinds: base.Kinds,
		Languages: base.Languages, Paths: []string{"pkg/cmd/"}}) {
		t.Error("filter values are concatenated without a separator")
	}
	// The two endpoints never collide on one preimage.
	if symbolQueryHash(model.SymbolRequest{Query: base.Query}) == searchQueryHash(model.SearchRequest{Query: base.Query}) {
		t.Error("the search and symbol endpoints hash one query to one value")
	}
}

// legCursorRejections proves digest §5: a tampered, expired, cross-endpoint,
// repinned or requeried cursor is CTX_CURSOR_INVALID. Accepting any of them
// serves a page from a generation or question the caller never asked about.
func legCursorRejections(t *testing.T, f *fixture) {
	signer := f.opts.Signer
	now := time.Now().UTC()
	qh := searchQueryHash(model.SearchRequest{Query: "handle"})
	c := newCursor(endpointSearch, f.binding, model.H("lease", "1"), qh, now.Add(15*time.Minute))
	token, err := signer.EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	back, err := decodeCursor(signer, token, endpointSearch, now)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if back.GenerationID != f.binding.GenerationID || back.AnalysisKey != f.binding.AnalysisKey || back.QueryHash != qh {
		t.Fatalf("round trip lost the binding: %+v", back)
	}
	if err := verifyCursor(back, f.binding, qh); err != nil {
		t.Fatalf("verifyCursor on its own cursor: %v", err)
	}

	// Tamper inside the signed payload, not in the base64 framing, so the
	// rejection comes from the MAC check and not from a decode error.
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	raw[len(raw)/2] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(raw)
	if tampered == token {
		t.Fatal("the tamper did not change the token")
	}

	expiredToken, err := signer.EncodeCursor(newCursor(endpointSearch, f.binding, model.H("lease", "1"), qh, now.Add(-2*time.Second)))
	if err != nil {
		t.Fatalf("EncodeCursor(expired): %v", err)
	}
	symbolToken, err := signer.EncodeCursor(newCursor(endpointSymbol, f.binding, model.H("lease", "1"), qh, now.Add(15*time.Minute)))
	if err != nil {
		t.Fatalf("EncodeCursor(symbol): %v", err)
	}

	for _, c := range []struct{ name, token string }{
		{"tampered", tampered},
		{"expired", expiredToken},
		{"cross_endpoint", symbolToken},
		{"malformed", "not-a-token"},
	} {
		_, err := decodeCursor(signer, c.token, endpointSearch, now)
		assertCode(t, c.name, err, model.CodeCursorInvalid)
	}

	other := f.binding
	other.GenerationID++
	assertCode(t, "repinned", verifyCursor(back, other, qh), model.CodeCursorInvalid)
	other = f.binding
	other.AnalysisKey = model.AnalysisKey(model.H("other-analysis"))
	assertCode(t, "reanalyzed", verifyCursor(back, other, qh), model.CodeCursorInvalid)
	assertCode(t, "requeried", verifyCursor(back, f.binding,
		searchQueryHash(model.SearchRequest{Query: "handle", Languages: []string{"go"}})), model.CodeCursorInvalid)
}

// legResolveKeyset proves the Resolve continuation fits its bound and round
// trips. A key that overflowed Cursor.LastKey would make Resolve unable to
// page at all; one that parsed loosely would resume at the wrong tier.
func legResolveKeyset(t *testing.T, f *fixture) {
	node := f.nodes["pkg/alpha.go"]
	key := resolveKey(model.TierQualifiedNamePrefix, string(node))
	if len(key) > 1024 {
		t.Fatalf("the keyset key is %d bytes, over the Cursor.LastKey bound", len(key))
	}
	rank, got, err := parseResolveKey(key)
	if err != nil {
		t.Fatalf("parseResolveKey: %v", err)
	}
	if rank != model.TierQualifiedNamePrefix.Rank() || got != string(node) {
		t.Fatalf("round trip = (%d, %q), want (%d, %q)", rank, got, model.TierQualifiedNamePrefix.Rank(), node)
	}
	c := newCursor(endpointSymbol, f.binding, model.H("lease", "2"), symbolQueryHash(model.SymbolRequest{Query: "Handle"}), time.Now().Add(time.Minute))
	c.LastKey = key
	if err := c.Validate(); err != nil {
		t.Fatalf("a keyset cursor does not validate: %v", err)
	}
	c.SpoolID = model.H("spool")
	if err := c.Validate(); err == nil {
		t.Error("a cursor naming both a sort key and a spool was accepted")
	}
	for _, bad := range []string{"", "no-separator", "x\x00" + string(node), "-1\x00" + string(node), "0\x00not-an-id"} {
		if _, _, err := parseResolveKey(bad); err == nil {
			t.Errorf("parseResolveKey(%q) was accepted", bad)
		} else {
			assertCode(t, "key "+bad, err, model.CodeCursorInvalid)
		}
	}
}

// stubFiles and stubBlobs stand in for the pinned reader and the store while
// the integration lane decides how the CAS reaches this package (see the
// report). The CAS itself is real: the bytes, the block digests and the line
// checkpoints under test all come from snapshot.CAS.
type stubFiles map[model.FileID]model.FileVersion

func (s stubFiles) File(_ context.Context, id model.FileID) (model.FileVersion, error) {
	fv, ok := s[id]
	if !ok {
		return model.FileVersion{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "unknown file"}
	}
	return fv, nil
}

type stubBlobs map[string]model.BlobRecord

func (s stubBlobs) Blob(_ context.Context, hash string) (model.BlobRecord, error) {
	rec, ok := s[hash]
	if !ok {
		return model.BlobRecord{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "unknown blob"}
	}
	return rec, nil
}

// countingCAS records how many bytes hydration actually pulled out of the CAS,
// so the bounded-window promise is measured and not assumed.
type countingCAS struct {
	cas   *snapshot.CAS
	reads int
	bytes uint64
	// maxRead is the largest single call. The CAS refuses a call over
	// model.MaxRawChunkBytes, so this is the invariant that keeps hydration
	// off that ceiling however long the span it resolves.
	maxRead uint64
}

func (c *countingCAS) ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error) {
	c.reads++
	c.bytes += r.End - r.Start
	c.maxRead = max(c.maxRead, r.End-r.Start)
	return c.cas.ReadRange(ctx, rec, r)
}

// legRangeHydration proves digest §4/Q10: a served hit carries a real source
// range, resolved from the nearest line checkpoint through the one position
// implementation, reading a bounded window. A nil Range is a silent capability
// reduction, and a hand-rolled line count would disagree with source.Cursor at
// the first CRLF or multi-byte rune.
func legRangeHydration(t *testing.T, _ *fixture) {
	ctx := context.Background()
	cas, err := snapshot.OpenCAS(t.TempDir())
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	// A file long enough to carry more than one line checkpoint, so the hit
	// below is resolved from a checkpoint deep in the file rather than from
	// byte zero. The target line holds a multi-byte identifier.
	filler := strings.Repeat("// padding padding padding padding\r\n", 4096)
	target := "func café() string { return foo(bar) }"
	body := filler + target + "\n"
	rec, err := cas.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(rec.LineCheckpoints) < 2 {
		t.Fatalf("the fixture file carries %d checkpoints, want more than one", len(rec.LineCheckpoints))
	}
	file := model.NewFileID("repo", "pkg/café.go")
	h := newHydrator(
		stubFiles{file: {ID: file, Path: "pkg/café.go", ContentHash: rec.Hash, Size: rec.Size}},
		stubBlobs{rec.Hash: rec},
		&countingCAS{cas: cas})
	counter := h.content.(*countingCAS)

	start := uint64(len(filler))
	span := model.ByteRange{Start: start, End: start + uint64(len(target))}
	hits := []model.SearchHit{{FileID: file, Path: "pkg/café.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	if err := h.hydratePage(ctx, hits, []model.ByteRange{span}); err != nil {
		t.Fatalf("hydratePage: %v", err)
	}
	got := hits[0].Range
	if got == nil {
		t.Fatal("Range is nil; a served hit must carry its source position")
	}
	// CRLF is two bytes and one line break, so the target is on line 4097.
	wantLine := uint32(strings.Count(filler, "\n") + 1)
	want := model.SourceRange{
		Start: model.Position{Byte: span.Start, Line: wantLine, Column: 0},
		End:   model.Position{Byte: span.End, Line: wantLine, Column: uint32(len(target))},
	}
	if *got != want {
		t.Fatalf("Range = %+v, want %+v", *got, want)
	}
	if counter.bytes > 2*maxRangeWindowBytes {
		t.Errorf("hydration read %d bytes in %d reads, over the bounded window", counter.bytes, counter.reads)
	}
	if counter.bytes >= uint64(rec.Size) {
		t.Errorf("hydration read %d of %d bytes: it is scanning the file, not a window", counter.bytes, rec.Size)
	}

	// A second hit in the same file reuses the cached blob record.
	before := counter.reads
	more := []model.SearchHit{{FileID: file, Path: "pkg/café.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	if err := h.hydratePage(ctx, more, []model.ByteRange{{Start: 0, End: 0}}); err != nil {
		t.Fatalf("hydratePage(empty span): %v", err)
	}
	if more[0].Range == nil || more[0].Range.Start.Line != 1 {
		t.Errorf("the empty span at byte 0 resolved to %+v, want line 1", more[0].Range)
	}
	if counter.reads != before+1 {
		t.Errorf("a zero-length span cost %d reads, want 1", counter.reads-before)
	}

	// A document that claims bytes past its file is a corrupt index, not a
	// silently clamped range.
	assertCode(t, "past end of file",
		h.hydratePage(ctx, []model.SearchHit{{FileID: file}}, []model.ByteRange{{Start: 0, End: uint64(rec.Size) + 1}}),
		model.CodeArgumentInvalid)
	if err := h.hydratePage(ctx, hits, nil); err == nil {
		t.Error("hydration accepted a span count that does not match its hits")
	}

	// VF4(a): a blob whose line checkpoints do NOT reach the hit. On a real
	// repository this failed the WHOLE answer with CTX_RESOURCE_LIMIT ("would
	// read past the bounded window") because one file's checkpoints stopped
	// short of byte 1.7 M. The gap is walked a window at a time instead, so
	// the position is still exact and the peak is still one window.
	sparseFile := model.NewFileID("repo", "pkg/sparse.go")
	sparseRec := rec
	sparseRec.LineCheckpoints = nil // not one checkpoint past byte 0
	sh := newHydrator(
		stubFiles{sparseFile: {ID: sparseFile, Path: "pkg/sparse.go", ContentHash: sparseRec.Hash, Size: sparseRec.Size}},
		stubBlobs{sparseRec.Hash: sparseRec},
		&countingCAS{cas: cas})
	sparseCounter := sh.content.(*countingCAS)
	sparseHits := []model.SearchHit{{FileID: sparseFile, Path: "pkg/sparse.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	if err := sh.hydratePage(ctx, sparseHits, []model.ByteRange{span}); err != nil {
		t.Fatalf("hydratePage over a file with no checkpoints: %v: a sparse checkpoint index must "+
			"degrade to more reads, never to a failed answer", err)
	}
	if sparseHits[0].Range == nil || *sparseHits[0].Range != want {
		t.Fatalf("the walked position is %+v, want the checkpointed answer %+v", sparseHits[0].Range, want)
	}
	if sparseCounter.bytes < uint64(span.Start) {
		t.Fatalf("the walk read %d bytes to reach byte %d; it cannot have covered the gap",
			sparseCounter.bytes, span.Start)
	}
	if sparseCounter.reads < 2 {
		t.Fatalf("the walk took %d reads over a %d-byte gap with a %d-byte window; it is not walking it",
			sparseCounter.reads, span.Start, maxRangeWindowBytes)
	}

	if counter.maxRead > maxRangeWindowBytes {
		t.Errorf("one hydration read pulled %d bytes, over the %d-byte window", counter.maxRead, maxRangeWindowBytes)
	}

	// SV2: a file that is ONE LINE of megabytes -- a minified bundle, a
	// generated data file. No line checkpoint can exist inside it, so
	// resolving a hit 4.5 MiB in used to ask the CAS for that whole span in
	// one call and failed the WHOLE answer with CTX_RESOURCE_LIMIT ("byte
	// range spans 4292608 bytes, over the 1048576-byte read ceiling").
	head := "// generated -- do not edit\n"
	longTarget := "func café() string { return foo(bar) }"
	lead := 4608000 - len(head) // the hit sits 4.5 MiB into the single line
	longLine := head + strings.Repeat("x", lead) + longTarget + strings.Repeat("y", 1<<19) + "\n"
	longRec, err := cas.Put(ctx, strings.NewReader(longLine))
	if err != nil {
		t.Fatalf("Put(long line): %v", err)
	}
	longSpan := model.ByteRange{Start: uint64(len(head) + lead), End: uint64(len(head) + lead + len(longTarget))}
	if gap := longSpan.Start - checkpointIndex(longRec).CheckpointFor(longSpan.Start).Byte; gap <= model.MaxRawChunkBytes {
		t.Fatalf("the nearest checkpoint is %d bytes back; the fixture no longer exercises a span over the read ceiling", gap)
	}
	lh := newHydrator(
		stubFiles{file: {ID: file, Path: "web/bundle.min.js", ContentHash: longRec.Hash, Size: longRec.Size}},
		stubBlobs{longRec.Hash: longRec},
		&countingCAS{cas: cas})
	longCounter := lh.content.(*countingCAS)
	longHits := []model.SearchHit{{FileID: file, Path: "web/bundle.min.js", Kind: model.NodeFunction, Tier: model.TierLexicalFTS}}
	if err := lh.hydratePage(ctx, longHits, []model.ByteRange{longSpan}); err != nil {
		t.Fatalf("hydratePage over a %d-byte line: %v: a long line must cost more reads, never a failed answer",
			len(longLine), err)
	}
	wantLong := model.SourceRange{
		Start: model.Position{Byte: longSpan.Start, Line: 2, Column: uint32(lead)},
		End:   model.Position{Byte: longSpan.End, Line: 2, Column: uint32(lead + len(longTarget))},
	}
	if longHits[0].Range == nil || *longHits[0].Range != wantLong {
		t.Fatalf("the streamed position is %+v, want %+v", longHits[0].Range, wantLong)
	}
	if longCounter.maxRead > maxRangeWindowBytes {
		t.Errorf("one read pulled %d bytes over a %d-byte span: the span is not being streamed",
			longCounter.maxRead, longSpan.End-longSpan.Start)
	}
	// The end endpoint continues the walk that resolved the start: the whole
	// resolution reads the span once, not once per endpoint.
	if longCounter.bytes > longSpan.End+2*maxRangeWindowBytes {
		t.Errorf("resolving both endpoints read %d bytes to reach byte %d: the walk is restarting",
			longCounter.bytes, longSpan.End)
	}

	// Cancellation and deadline are different answers (digest §6).
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assertCode(t, "canceled", h.hydratePage(canceled, hits, []model.ByteRange{span}), model.CodeCanceled)
	expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer stop()
	assertCode(t, "deadline", h.hydratePage(expired, hits, []model.ByteRange{span}), model.CodeQueryDeadline)
}

// assertCode fails unless err is a typed model.Error carrying want.
func assertCode(t *testing.T, what string, err error, want string) {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Errorf("%s: err = %v, want a typed %s", what, err, want)
		return
	}
	if typed.Code != want {
		t.Errorf("%s: err code = %s, want %s", what, typed.Code, want)
	}
}

// legEndToEndRanking is the digest §9 retrieval leg: the whole Section 14.2
// order over the real corpus, through the real service. It is the only leg
// here that needs the retrieval lanes (L2 exact, L3 lexical) and the
// integration lane (L6 search.go orchestration); until those land it fails at
// New with "search.New is not implemented", which is the intended state.
//
// It asserts exact int64 ScoreMicros for the exact tiers -- digest §4 fixes
// them at 0 for a document that did not also match lexically, and a non-zero
// value there would mean tier ordering had quietly become score ordering.
func legEndToEndRanking(t *testing.T, f *fixture) {
	s := newService(t, f.opts)
	page, err := s.Search(f.ctx, model.SearchRequest{Query: "pkg.Alpha.Handle", Page: model.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("Search(exact qualified name): %v", err)
	}
	if len(page.Items) == 0 {
		t.Fatal("the exact qualified name matched nothing")
	}
	top := page.Items[0]
	if top.Tier != model.TierExactQualifiedName {
		t.Errorf("top tier = %q, want %q", top.Tier, model.TierExactQualifiedName)
	}
	if top.NodeID != f.nodes["pkg/alpha.go"] {
		t.Errorf("top hit is %q, want the alpha node", top.NodeID)
	}
	if top.OccurrenceCount != 2 {
		t.Errorf("OccurrenceCount = %d, want 2: the two documents of one node fold to one hit", top.OccurrenceCount)
	}
	if top.Range == nil {
		t.Error("the served hit carries no source range")
	}
	// The BM25F constants themselves. Section 9 requires exact int64
	// ScoreMicros, not orderings: with only an ordering assertion, bm25K1,
	// bm25B and every columnWeight entry can be changed to any other value and
	// the suite stays green, so the scoring formula would be unprotected. Two
	// scores are pinned because they load different parts of it: the top hit
	// folds a name-column match into an exact-tier candidate, and the second
	// is a pure body/qualified-name lexical hit. Recompute these deliberately
	// -- a changed value here is a changed ranking for every caller.
	//
	// The other half of Section 9's ranking claim, that a lower-scoring exact
	// hit still outranks a higher-scoring lexical one, is legLessChain's
	// (:327); asserting it again here would be a duplicate.
	const (
		wantTopMicros     int64 = 2_197_205
		wantLexicalMicros int64 = 554_547
	)
	if top.ScoreMicros != wantTopMicros {
		t.Errorf("the alpha hit scores %d, want the BM25F value %d", top.ScoreMicros, wantTopMicros)
	}
	lexicalHit := false
	for _, hit := range page.Items {
		if hit.NodeID != f.nodes["pkg/beta.go"] {
			continue
		}
		lexicalHit = true
		if hit.Tier != model.TierLexicalFTS || hit.ScoreMicros != wantLexicalMicros {
			t.Errorf("the beta hit is tier %q at %d, want %q at the BM25F value %d",
				hit.Tier, hit.ScoreMicros, model.TierLexicalFTS, wantLexicalMicros)
		}
	}
	if !lexicalHit {
		t.Error("the query returned no lexical hit to score")
	}
	for i, hit := range page.Items {
		// Digest §4 zeroes the exact and prefix tiers "unless the document
		// also matched lexically, keeping that score", which is exactly what
		// the top hit here does: its node's two lexical documents fold into
		// the exact candidate. The exemption is read off the hit's own
		// reasons, with the production spelling, so it can only apply to a
		// hit the lexical tier really contributed to.
		lexical := hit.Tier == model.TierLexicalFTS || slices.Contains(hit.Reasons, reasonFor(model.TierLexicalFTS))
		if !lexical && hit.ScoreMicros != 0 {
			t.Errorf("hit %d is tier %q with ScoreMicros = %d, want 0", i, hit.Tier, hit.ScoreMicros)
		}
		if i > 0 && page.Items[i-1].Tier.Rank() > hit.Tier.Rank() {
			t.Errorf("hit %d (%q) sorts after a later tier", i, hit.Tier)
		}
	}

	// The prefix tier serves both pkg.Beta* and pkg.Alpha* documents through
	// the indexed range, never a leading wildcard.
	prefix, err := s.Search(f.ctx, model.SearchRequest{Query: "pkg.", Page: model.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("Search(prefix): %v", err)
	}
	if len(prefix.Items) < 3 {
		t.Errorf("the prefix query returned %d hits, want every fixture document", len(prefix.Items))
	}

	// A Unicode identifier and a literal-punctuation query both survive the
	// FTS encoder rather than being rejected as malformed MATCH syntax.
	for _, q := range []string{"café", "foo(bar)"} {
		got, err := s.Search(f.ctx, model.SearchRequest{Query: q, Page: model.PageRequest{Limit: 10}})
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if len(got.Items) == 0 {
			t.Errorf("Search(%q) matched nothing", q)
		}
	}
}

// ---------------------------------------------------------------------------
// FX-C13b regression legs. Each guards one behavioural fix that the review
// showed no test could see: mutating the fixed line left the suite green.
// ---------------------------------------------------------------------------

// stubExact is a pinned read surface holding one file whose declarations are
// ordered so that the first `limit` of them are of a kind the caller filters
// out. Storage applies the page bound BEFORE the kind filter (NodesInFile takes
// no kinds), so this is the shape that turns a bounded single read into "0
// hits, not truncated" for a file that has three matching declarations. The
// fixture corpus publishes one node per file and cannot express it.
type stubExact struct {
	file  model.FileID
	path  string
	nodes []sqlite.StoredNode
}

func (s *stubExact) Node(context.Context, model.NodeID) (sqlite.StoredNode, error) {
	return sqlite.StoredNode{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "unknown node"}
}

func (s *stubExact) FileByPath(_ context.Context, p string) (model.FileID, error) {
	if p != s.path {
		return "", &model.Error{Code: model.CodeArgumentInvalid, Message: "unknown path"}
	}
	return s.file, nil
}

func (s *stubExact) File(_ context.Context, id model.FileID) (model.FileVersion, error) {
	return model.FileVersion{ID: id, Path: s.path}, nil
}

// NodesInFile pages the declarations on the (start_byte, node_id) keyset, which
// is the contract exact.go walks; it applies no kind filter, exactly as storage
// does not.
func (s *stubExact) NodesInFile(_ context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	if file != s.file {
		return nil, nil
	}
	var out []sqlite.StoredNode
	for _, n := range s.nodes {
		start := nodeStartByte(n)
		if start < afterStart || (start == afterStart && n.Node.ID <= after) {
			continue
		}
		out = append(out, n)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// DistinctNodesInFile serves the same rows: every synthetic declaration here
// has its own node id, so the one-row-per-node reading and the every-offset
// reading coincide. The distinction is storage's, and the leg that proves it
// lives beside the SQL.
func (s *stubExact) DistinctNodesInFile(ctx context.Context, file model.FileID, afterStart int64,
	after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	return s.NodesInFile(ctx, file, afterStart, after, limit)
}

func (s *stubExact) Nodes(context.Context, sqlite.NodeFilter, model.NodeID, int) ([]sqlite.StoredNode, error) {
	return nil, nil
}

// testTruncationReason is an arbitrary answer-level truncation reason used to
// prove a spool meta record round-trips it; the reason's text is not the
// invariant under test.
const testTruncationReason = "a traversal budget was exhausted"

// legPathTierLossless proves the exact_path tier drops nothing: it walks the
// whole keyset and emits every kept candidate, with the read page size bounding
// only one round trip. Serving five of eight candidates -- or answering
// "nothing here" for a file whose three functions sit past one page -- are both
// the silent drop Section 14.3 forbids.
//
// Mutation: restore the per-tier bound (stop emitting once page candidates have
// been kept) and the unfiltered case comes back with 5 instead of 8.
func legPathTierLossless(t *testing.T, _ *fixture) {
	const page = 5
	stub := &stubExact{file: model.FileID(model.H("stub-file")), path: "pkg/wide.go"}
	for i := range 8 {
		kind := model.NodeVariable
		if i >= page {
			kind = model.NodeFunction
		}
		id := model.NodeID(model.H("stub-node", strconv.Itoa(i)))
		stub.nodes = append(stub.nodes, sqlite.StoredNode{
			Node:  model.Node{ID: id, Kind: kind, Name: "n" + strconv.Itoa(i)},
			Bytes: &model.ByteRange{Start: uint64(i * 10), End: uint64(i*10 + 1)},
		})
	}
	collect := func(kinds []model.NodeKind) []exactHit {
		t.Helper()
		var out []exactHit
		if _, err := pathCandidates(context.Background(), stub, stub.path, kinds, page,
			func(h exactHit) error { out = append(out, h); return nil }); err != nil {
			t.Fatalf("pathCandidates: %v", err)
		}
		return out
	}
	if out := collect(nil); len(out) != 8 {
		t.Fatalf("pathCandidates(unfiltered) served %d of the file's 8 candidates; the tier walks the whole keyset", len(out))
	}
	if out := collect([]model.NodeKind{model.NodeFunction}); len(out) != 3 {
		t.Fatalf("pathCandidates(kind filter) served %d candidates; want 3: the filter runs over the keyset, not over one bounded read",
			len(out))
	}
}

// stubWideExact serves the three symbol tiers of one query over a synthetic
// corpus of n declarations, honouring the NodeFilter exactly as storage does so
// the tier predicates exact.go relies on hold. The nodes are SYNTHESIZED per
// page from the keyset rather than held in a slice: a stub that materialized
// 200 000 of them would itself be the heap the leg below measures.
//
// Every node is named "Widget" and qualified "Widget.mN", so the
// exact_qualified_name tier matches none, qualified_name_prefix matches all of
// them, and exact_name matches all of them but is superseded by the prefix
// tier -- the cross-tier rule under test.
type stubWideExact struct {
	n    int
	file model.FileID
	path string
}

func (s *stubWideExact) node(i int) sqlite.StoredNode {
	return sqlite.StoredNode{
		Node: model.Node{ID: model.NodeID(fmt.Sprintf("n%08d", i)), Kind: model.NodeFunction,
			Name: "Widget", QualifiedName: "Widget.m" + strconv.Itoa(i), FileID: s.file},
		Bytes: &model.ByteRange{Start: uint64(i), End: uint64(i) + 1},
	}
}

func (s *stubWideExact) Node(context.Context, model.NodeID) (sqlite.StoredNode, error) {
	return sqlite.StoredNode{}, errors.New("not used")
}

func (s *stubWideExact) FileByPath(_ context.Context, path string) (model.FileID, error) {
	if path != s.path {
		return "", &model.Error{Code: model.CodeArgumentInvalid, Message: "no such file"}
	}
	return s.file, nil
}

func (s *stubWideExact) File(_ context.Context, id model.FileID) (model.FileVersion, error) {
	return model.FileVersion{ID: id, Path: s.path}, nil
}

// NodesInFile pages the same synthetic declarations on the (start_byte,
// node_id) keyset, so the exact_path tier walks the file the symbol tiers also
// match -- the overlap the tier-0 predicate decides.
func (s *stubWideExact) NodesInFile(_ context.Context, file model.FileID, afterStart int64,
	after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	if file != s.file {
		return nil, nil
	}
	var out []sqlite.StoredNode
	for i := range s.n {
		n := s.node(i)
		start := nodeStartByte(n)
		if start < afterStart || (start == afterStart && n.Node.ID <= after) {
			continue
		}
		out = append(out, n)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// DistinctNodesInFile coincides with NodesInFile for the same reason as
// stubExact's: one synthetic declaration per node id.
func (s *stubWideExact) DistinctNodesInFile(ctx context.Context, file model.FileID, afterStart int64,
	after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	return s.NodesInFile(ctx, file, afterStart, after, limit)
}

func (s *stubWideExact) Nodes(_ context.Context, f sqlite.NodeFilter, after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	match := func(n sqlite.StoredNode) bool {
		switch {
		case f.QualifiedName != "":
			return n.Node.QualifiedName == f.QualifiedName
		case f.QualifiedPrefix != "":
			return strings.HasPrefix(n.Node.QualifiedName, f.QualifiedPrefix)
		default:
			return n.Node.Name == f.Name
		}
	}
	// The keyset resumes at the successor of `after`, which the fixed-width
	// id encodes; rescanning from zero on every page would make the leg's cost
	// quadratic in a corpus it walks 200 000 nodes of.
	first := 0
	if after != "" {
		i, err := strconv.Atoi(strings.TrimPrefix(string(after), "n"))
		if err != nil {
			return nil, err
		}
		first = i + 1
	}
	var out []sqlite.StoredNode
	for i := first; i < s.n; i++ {
		n := s.node(i)
		if !match(n) {
			continue
		}
		out = append(out, n)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// legExactTiersHoldOnePage proves the exact tiers carry no candidate-sized heap
// structure: the `seen` set that decided the cross-tier "most specific tier
// wins" rule held one model.NodeID per distinct candidate, so a wide
// qualified_name_prefix scan cost heap proportional to the match count. The
// rule is now two predicates over the candidate in hand, and the working set is
// one read page however many candidates the tier yields.
//
// The heap is sampled at the LAST candidate, while anything the walk accumulated
// is still reachable; the answer itself is counted and discarded, so what the
// sample sees is the walk's own working set.
//
// Mutation: restore the map (`seen[n.Node.ID] = struct{}{}` beside a duplicate
// test) and the ten-fold input moves the live set by ~19 MiB, failing the bound.
func legExactTiersHoldOnePage(t *testing.T, _ *fixture) {
	walk := func(n int) (int, uint64) {
		t.Helper()
		stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "pkg/wide.go"}
		var held uint64
		count := 0
		if err := exactCandidates(context.Background(), stub, "Widget", nil, 1000,
			func(exactHit) error {
				count++
				if count == n {
					var m runtime.MemStats
					runtime.GC()
					runtime.ReadMemStats(&m)
					held = m.HeapAlloc
				}
				return nil
			}); err != nil {
			t.Fatalf("exactCandidates(%d): %v", n, err)
		}
		return count, held
	}
	// Correctness first: every node matches both the prefix tier and the
	// exact_name tier, and each must be emitted ONCE -- supersededByLowerTier
	// suppresses the duplicate, with no candidate-sized seen set.
	small, smallHeap := walk(20_000)
	if small != 20_000 {
		t.Fatalf("a 20000-declaration prefix tier emitted %d candidates, want each exactly once", small)
	}
	large, largeHeap := walk(200_000)
	if large != 200_000 {
		t.Fatalf("a 200000-declaration prefix tier emitted %d candidates, want each exactly once", large)
	}
	// A ten-fold input must not move the live set: the walk holds one read
	// page, not one entry per candidate.
	if largeHeap > smallHeap+(1<<20) {
		t.Fatalf("the exact tiers held %d heap bytes over 20000 candidates and %d over 200000; "+
			"the working set of the tiers is one read page, not the candidate count", smallHeap, largeHeap)
	}
	t.Logf("exact-tier live set: 20000 candidates %d bytes, 200000 candidates %d bytes", smallHeap, largeHeap)
}

// legExactPathTierOwnsItsFile proves the predicate that replaced the tier-0
// half of the removed `seen` set: when the query is BOTH a path and a symbol
// name, every declaration of that file is emitted once, by the exact_path tier
// that outranks the symbol tiers, and the symbol tiers do not emit it again.
//
// Mutation: drop the `n.Node.FileID == pathFile` clause from exactCandidates
// and the prefix tier re-emits all 500, doubling the count.
func legExactPathTierOwnsItsFile(t *testing.T, _ *fixture) {
	const n = 500
	// The query resolves as a path AND matches the symbol tiers' filters.
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "Widget"}
	byTier := map[model.SearchTier]int{}
	if err := exactCandidates(context.Background(), stub, stub.path, nil, 100,
		func(h exactHit) error { byTier[h.Tier]++; return nil }); err != nil {
		t.Fatalf("exactCandidates: %v", err)
	}
	if got := byTier[model.TierExactPath]; got != n {
		t.Fatalf("the exact_path tier emitted %d of the file's %d declarations, want all of them", got, n)
	}
	if total := byTier[model.TierExactPath] + byTier[model.TierQualifiedNamePrefix] +
		byTier[model.TierExactName] + byTier[model.TierExactQualifiedName]; total != n {
		t.Fatalf("the exact tiers emitted %d candidates for %d declarations: a node of the walked file "+
			"must not be emitted again by a symbol tier (%v)", total, n, byTier)
	}
}

// legSpoolExhaustionEndsThePage proves the class-D refusal is gone: when the
// shared spool byte budget cannot hold the tail of an answer, the hits already
// in hand are still served. Before, spoolNext's error propagated out of Search
// and page 1 failed because the page after it could not be written.
//
// The budget is set to one byte, so Create's header alone exhausts it and no
// continuation can be minted for a corpus that certainly has one.
//
// Mutation: restore `return empty, err` in the spoolNext branch and Search
// fails here instead of answering.
func legSpoolExhaustionEndsThePage(t *testing.T, f *fixture) {
	full, err := pagination.NewSpools(filepath.Join(t.TempDir(), "spools"), 1, f.store)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	opts := f.opts
	opts.Spools = full
	s := newService(t, opts)
	req := model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}}
	page, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("a full spool failed page 1 instead of ending it: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("page 1 served %d hits, want the 1 it had in hand", len(page.Items))
	}
	if page.Meta.NextCursor != "" {
		t.Error("an answer whose tail could not be spooled still minted a continuation")
	}
	if !page.Meta.Truncated {
		t.Fatal("an answer that dropped its tail did not report itself truncated")
	}
	if !strings.Contains(page.Meta.TruncationReason, "spool") {
		t.Errorf("truncation reason = %q, want the dropped tail named", page.Meta.TruncationReason)
	}
}

// legReadOnlyServesOnePage proves the rule a continuation cannot break: a
// process that opened the store read-only -- which is how `codectx search`
// answers while another process is indexing -- can write neither the cursor
// lease nor the spool a continuation is made of, so it must serve the page it
// has and say the answer stops there. Minting a token it could not honour would
// hand the caller a continuation that fails on use, which is worse than an
// answer that admits it is partial.
//
// Mutation: make PinnedReader.Continuable report true unconditionally, and this
// leg fails with CTX_INTERNAL out of the lease the read-only store refuses.
func legReadOnlyServesOnePage(t *testing.T, f *fixture) {
	ro, err := sqlite.Open(f.ctx, f.dbPath, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	opts := f.opts
	opts.Store = ro
	opts.Leases = pagination.NewLeases(ro, pagination.DefaultCursorTTL)
	s := newService(t, opts)
	req := model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}}
	page, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("a writerless process failed the query instead of answering it: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("page 1 served %d hits, want the 1 it had in hand", len(page.Items))
	}
	if page.Meta.NextCursor != "" {
		t.Error("a process that cannot write a spool still minted a continuation")
	}
	if !page.Meta.Truncated {
		t.Fatal("an answer with an unreachable tail did not report itself truncated")
	}
	if !strings.Contains(page.Meta.TruncationReason, "writes nothing") {
		t.Errorf("truncation reason = %q, want the cause named", page.Meta.TruncationReason)
	}
}

// legContinuationTruncation proves that Service.Search's continuation branch
// reports the answer-level truncation the FIRST page computed. A continuation
// reads its hits from the spool and has nothing of its own to recompute
// truncation from, so a page that dropped the metadata would present the tail
// of an admittedly incomplete answer as complete.
//
// The spool round-trip itself is legSpooledPage's; what is asserted here is
// only what comes out of Service.Search. The truncated continuation is minted
// rather than produced by a first page because neither answer-level bound --
// 200 exact candidates, 512 term offsets -- is reachable on a three-document
// corpus.
func legContinuationTruncation(t *testing.T, f *fixture) {
	s := newService(t, f.opts)
	req := model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}}
	first, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("Search(page 1): %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("a bounded page over three matching documents minted no continuation")
	}
	if first.Meta.Truncated {
		t.Fatalf("the control page reports truncation (%q); the assertion below would be vacuous", first.Meta.TruncationReason)
	}
	continued := req
	continued.Page.Cursor = first.Meta.NextCursor
	second, err := s.Search(f.ctx, continued)
	if err != nil {
		t.Fatalf("Search(page 2): %v", err)
	}
	if len(second.Items) == 0 || second.Items[0].NodeID == first.Items[0].NodeID {
		t.Fatalf("page 2 served %d items starting at the page-1 hit; the continuation did not advance", len(second.Items))
	}
	if second.Meta.Truncated {
		t.Error("a continuation of a complete answer reports truncation")
	}

	// The answer-level truncation rides INSIDE the signed continuation, so a
	// truncated walk is minted by re-signing a live one -- over a fresh first
	// page, whose spool the assertion below is the only reader of -- rather
	// than by a fixture that cannot reach either answer-level bound.
	fresh, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("Search(fresh page 1): %v", err)
	}
	c, err := s.resumeSearch(fresh.Meta.NextCursor, searchQueryHash(req), time.Now().UTC())
	if err != nil {
		t.Fatalf("resumeSearch: %v", err)
	}
	c.Truncated, c.TruncationReason = true, testTruncationReason
	token, err := s.signSearchCursor(c)
	if err != nil {
		t.Fatalf("signSearchCursor: %v", err)
	}
	continued.Page.Cursor = token
	page, err := s.Search(f.ctx, continued)
	if err != nil {
		t.Fatalf("Search(truncated continuation): %v", err)
	}
	if !page.Meta.Truncated || page.Meta.TruncationReason != testTruncationReason {
		t.Fatalf("the continuation reports (%t, %q), want the first page's (true, %q)",
			page.Meta.Truncated, page.Meta.TruncationReason, testTruncationReason)
	}
}

// legSymbolByCanonicalID proves the canonical-id tier of `codectx symbol`. The
// command's Long text promises a canonical node ID resolves, and the name tiers
// index names: without this tier a query spelled as an id falls through them
// and the documented call answers an empty page.
func legSymbolByCanonicalID(t *testing.T, f *fixture) {
	s := newService(t, f.opts)
	want := f.nodes["pkg/beta.go"]
	page, err := s.Resolve(f.ctx, model.SymbolRequest{Query: string(want),
		Operation: model.SymbolResolve, SemanticSource: model.SemanticCanonical})
	if err != nil {
		t.Fatalf("Resolve(canonical id): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != want {
		t.Fatalf("Resolve(%q) served %d items, want the one node it names", want, len(page.Items))
	}
	if page.Meta.NextCursor != "" {
		t.Error("the canonical-id tier minted a continuation for a single-node answer")
	}
}

// collectorFor opens a ranking collector whose external sort spills under the
// test's own directory. The run budget is the floor, so these legs exercise
// the spilling path rather than the buffer-only one.
func collectorFor(t *testing.T) *collector {
	t.Helper()
	c, err := newCollector(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("opening a collector: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// add offers one candidate with no servable facts, which the ranking legs do
// not read.
func add(t *testing.T, c *collector, r ranked, reasons ...string) {
	t.Helper()
	if err := c.add(r, hitFacts{}, reasons...); err != nil {
		t.Fatalf("adding a candidate: %v", err)
	}
}

// resultsOf drains the ordered run into a slice so a leg can index it. The
// production path streams it instead; nothing outside a test holds it whole.
func resultsOf(t *testing.T, c *collector) []scored {
	t.Helper()
	run, err := c.deduped()
	if err != nil {
		t.Fatalf("ordering the candidate set: %v", err)
	}
	t.Cleanup(func() { run.Close() })
	out, err := rankOrder(t, run)
	if err != nil {
		t.Fatalf("reading the ordered set: %v", err)
	}
	return out
}

// rankOrder is the WHOLE answer in served order: the deduplicated candidates
// promoted and passed through the same external sort under cmpScored that a
// continuation runs over the raw candidate spool. It is the reference the
// bounded heap's first page is compared against.
func rankOrder(t *testing.T, run *pagination.SortedRun[scored]) ([]scored, error) {
	t.Helper()
	sorter, err := newScoredSort(t.TempDir(), 0, cmpScored)
	if err != nil {
		return nil, err
	}
	defer sorter.Close()
	if err := run.Each(func(v scored) error {
		v.ScoreMicros = v.Folded
		return sorter.Add(v)
	}); err != nil {
		return nil, err
	}
	sorted, err := sorter.Sorted()
	if err != nil {
		return nil, err
	}
	defer sorted.Close()
	var out []scored
	err = sorted.Each(func(v scored) error { out = append(out, v); return nil })
	return out, err
}

// legRankedSetIsBoundedByItsRunBudget proves the ranked set is sized by the
// query memory admission and not by the number of matches. A one-character
// qualified_name_prefix range-scans a corpus-sized slice of node_ids and every
// match enters this set (H-L5b concern 2); at 200 000 matches the former
// map-plus-slice collector held 200 000 entries plus 200 000 servable hits.
//
// The assertion is the sort's live-record high-water mark, which is
// deterministic and input-independent: it must be IDENTICAL at 20 000 and at
// 200 000 candidates and within the envelope the run budget and the merge
// fan-in define.
//
// Mutation: drop the byte budget so a run is unbounded (WithRunBytes's sizeOf
// nil-guard, or a runBytes larger than the input) and the high-water mark
// becomes the match count at both sizes.
func legRankedSetIsBoundedByItsRunBudget(t *testing.T, f *fixture) {
	peakAt := func(n int) int {
		c := collectorFor(t)
		for i := range n {
			// A stride coprime with n offers the candidates in an order that
			// shares no prefix with either sort order, so every run spills a
			// scattered key range and the merge is a real one.
			k := (i * 2237) % n
			add(t, c, rankedOf(model.TierLexicalFTS, int64(k), "pkg/f"+strconv.Itoa(k%97)+".go", uint64(k),
				model.NodeID("n"+strconv.Itoa(k)), "k"+strconv.Itoa(k)), "lexical")
		}
		got := resultsOf(t, c)
		if len(got) != n {
			t.Fatalf("%d candidates ranked to %d results, want all of them", n, len(got))
		}
		return c.peakLiveRecords()
	}
	small, large := peakAt(20_000), peakAt(200_000)
	// A ten-fold input must not move the high-water mark at all beyond the
	// merge fan-in: the run budget is a BYTE budget, so the record count one
	// run holds varies by a few with the length of the keys that happen to
	// fill it, but it cannot track the input.
	if large > small+pagination.MaxSortFanIn {
		t.Fatalf("live record high-water was %d at 20000 candidates and %d at 200000; the working set must not grow with the match count",
			small, large)
	}
	// The envelope: one run buffer of the floored budget, whose records this
	// leg makes about 300 bytes each, plus one record per open run in the
	// merge. A generous ceiling still fails instantly on an unbounded run.
	const envelope = 4096
	if large > envelope {
		t.Fatalf("live record high-water was %d at 200000 candidates, over the %d-record envelope", large, envelope)
	}

}

// legDeadlineEndsThePage is ruling Q4 for search: resources.query_timeout ends
// a PAGE, not an answer. On a real repository this endpoint answered
// CTX_QUERY_DEADLINE with no page and no cursor, so the widest queries -- the
// only ones that ever reach the deadline -- were unanswerable rather than
// partially answered.
//
// The ranking phase runs under a budget that has already expired here, so
// every tier stops at its first read; what the collector holds is served as a
// page that says it is truncated and why. Nothing else in the answer may
// degrade: the binding, the completeness report and the page's own validation
// are the same as any other answer's.
//
// Mutation: return the tier error from Service.rank instead of converting a
// deadline into a truncated page (drop the isQueryDeadline arms) and this leg
// fails with CTX_QUERY_DEADLINE where it wants a page.
func legDeadlineEndsThePage(t *testing.T, f *fixture) {
	opts := f.opts
	opts.Resources.QueryTimeout = config.Duration(time.Nanosecond)
	s := newService(t, opts)
	got, err := s.Search(f.ctx, model.SearchRequest{Query: "handle"})
	if err != nil {
		t.Fatalf("Search under an expired budget: %v: the time budget ends a page, not the answer", err)
	}
	if !got.Meta.Truncated {
		t.Fatal("the answer is not marked truncated; a page that stopped at the deadline must say so")
	}
	if !strings.Contains(got.Meta.TruncationReason, "resources.query_timeout") {
		t.Fatalf("the truncation reason is %q, want it to name resources.query_timeout", got.Meta.TruncationReason)
	}
	if got.Meta.Binding.GenerationID != f.binding.GenerationID {
		t.Fatalf("the deadline page is bound to generation %d, want %d",
			got.Meta.Binding.GenerationID, f.binding.GenerationID)
	}
	// Whatever was ranked before the deadline is served, and a page that
	// leaves hits unserved carries the continuation to reach them.
	if len(got.Items) > 0 && len(got.Items) < len(fixtureDocs) && got.Meta.NextCursor == "" {
		t.Fatalf("a deadline page served %d of at most %d hits with no continuation",
			len(got.Items), len(fixtureDocs))
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the deadline page does not validate: %v", err)
	}
}

// legUnboundedQueryAnswersInFull is the other half of ruling Q4, and the half a
// shipped default now depends on: resources.query_timeout is unlimited by
// DEFAULT, and it is a default and never a ceiling.
//
// Both halves were broken here. search.New refused a non-positive timeout, so
// the shipped configuration could not compose a search service at all; and
// every request was wrapped in context.WithTimeout unconditionally, so a zero
// timeout minted `now + 0` -- an instant already past, which would refuse every
// query -- while a caller who had set a LONGER deadline of their own had it
// silently cut back to the configured default and was told the query ran out
// of time at a fraction of the budget they granted.
//
// The fixture needs no wall clock: a one-nanosecond configured timeout is
// already spent by the time the tiers read, so "the deadline applied" is
// observable as a truncated page (legDeadlineEndsThePage asserts that leg), and
// its absence is observable as a complete one.
//
// Mutation: restore context.WithTimeout(ctx, s.timeout) in place of
// model.QueryDeadline at either site and both sub-legs below fail on a page
// that came back truncated.
func legUnboundedQueryAnswersInFull(t *testing.T, f *fixture) {
	// Zero is the shipped default and means NO deadline: the service composes,
	// and the query is ranked to a complete answer.
	opts := f.opts
	opts.Resources.QueryTimeout = 0
	got, err := newService(t, opts).Search(f.ctx, model.SearchRequest{Query: "handle"})
	if err != nil {
		t.Fatalf("an unbounded search failed: %v: query_timeout 0 means no deadline, not one already spent", err)
	}
	if got.Meta.Truncated {
		t.Fatalf("an unbounded search answered truncated (%q); `now + 0` is being applied as a deadline",
			got.Meta.TruncationReason)
	}
	if len(got.Items) == 0 {
		t.Fatal("the unbounded search answered nothing: this leg cannot tell a complete answer from an empty fixture")
	}
	full := len(got.Items)

	// A caller's own deadline is the request's, above the configured value as
	// readily as below it. The configured one here is already spent, so a
	// service that took the smaller of the two truncates.
	opts.Resources.QueryTimeout = config.Duration(time.Nanosecond)
	ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
	defer cancel()
	if got, err = newService(t, opts).Search(ctx, model.SearchRequest{Query: "handle"}); err != nil {
		t.Fatalf("a search under the caller's own minute-long deadline failed: %v", err)
	}
	if got.Meta.Truncated || len(got.Items) != full {
		t.Fatalf("the caller's deadline is a minute away, yet the search answered %d of %d hit(s) truncated=%v: "+
			"resources.query_timeout is being applied as a ceiling", len(got.Items), full, got.Meta.Truncated)
	}
}

// legPageClampIsReported protects the page-bound notice the answer carries.
// model.PageRequest refuses a limit above model.MaxPageItems, so the clamp a
// caller can actually reach is the one against a LOWERED
// resources.max_page_items: the request is served short, and without the
// notice the caller cannot tell a clamped page from the end of the answer.
//
// Mutation: drop the notice (return "" from Service.pageLimit, or stop
// appending it to meta.Notices) and this leg fails on the notice count.
func legPageClampIsReported(t *testing.T, f *fixture) {
	opts := f.opts
	opts.Resources.MaxPageItems = 1
	s := newService(t, opts)
	got, err := s.Search(f.ctx, model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 50}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("a page of 50 against max_page_items=1 served %d hits, want 1", len(got.Items))
	}
	if len(got.Meta.Notices) != 1 {
		t.Fatalf("the answer carries %d notices (%v), want exactly one naming the clamp",
			len(got.Meta.Notices), got.Meta.Notices)
	}
	if n := got.Meta.Notices[0]; !strings.Contains(n, "requested 50") || !strings.Contains(n, "effective 1") {
		t.Fatalf("the clamp notice is %q, want it to name requested 50 and effective 1", n)
	}
	// An unclamped request carries none: a notice on every answer is noise,
	// and the caller would stop reading them.
	plain, err := s.Search(f.ctx, model.SearchRequest{Query: "handle"})
	if err != nil {
		t.Fatalf("Search(no limit): %v", err)
	}
	if len(plain.Meta.Notices) != 0 {
		t.Fatalf("a request that named no page bound carries notices %v, want none", plain.Meta.Notices)
	}
}

// legStreamedWalkParity proves a paged walk is the same answer, in the same
// order, as the unpaginated one. The tail is never materialised: every
// candidate is laid into the raw spool as it passes, the first continuation
// sorts that spool and writes the ordered remainder skipping the records page
// one already served, and every later page seeks to its own byte offset. A
// record that was skipped, repeated or reordered -- an off-by-one in that skip
// against the served count, or a codec that did not round trip -- would show
// up here and nowhere else.
//
// It asserts the WHOLE hit, field for field: Reasons, Kind, Name,
// QualifiedName and Signature survive the spool round trip or they are lost
// for every page but the first, and Range must come back identical although
// the continuation -- not the first page -- is what hydrates it.
//
// The corpus walk runs the whole continuation path on a small answer; the
// second half drives the page heap over a candidate set whose ties span the
// page boundary, which is the arm a small corpus can never reach.
func legStreamedWalkParity(t *testing.T, f *fixture) {
	s := newService(t, f.opts)
	whole, err := s.Search(f.ctx, model.SearchRequest{Query: "handle"})
	if err != nil {
		t.Fatalf("Search(one page): %v", err)
	}
	if len(whole.Items) < 2 {
		t.Fatalf("the corpus answers %d hits; the walk below needs at least 2", len(whole.Items))
	}
	req := model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}}
	var walked []model.SearchHit
	for page := 0; ; page++ {
		if page > len(whole.Items) {
			t.Fatalf("the continuation walk did not end after %d pages", page)
		}
		got, err := s.Search(f.ctx, req)
		if err != nil {
			t.Fatalf("Search(page %d): %v", page+1, err)
		}
		walked = append(walked, got.Items...)
		if got.Meta.NextCursor == "" {
			break
		}
		req.Page.Cursor = got.Meta.NextCursor
	}
	if len(walked) != len(whole.Items) {
		t.Fatalf("the walk served %d hits, want the %d the whole answer holds", len(walked), len(whole.Items))
	}
	if !reflect.DeepEqual(walked, whole.Items) {
		for i := range whole.Items {
			if !reflect.DeepEqual(walked[i], whole.Items[i]) {
				t.Fatalf("walked hit %d = %+v, want %+v: a spooled hit must survive the round trip whole",
					i, walked[i], whole.Items[i])
			}
		}
		t.Fatalf("the walked answer differs from the whole one")
	}

	// The wide arm, on a candidate set whose TIES SPAN THE PAGE BOUNDARY: every
	// candidate shares one tier, one score and one path, so the first page is
	// decided entirely by the tie-break keys after them. The heap's page must
	// be the fully sorted answer's first page, record for record.
	//
	// Mutation: drop any key of cmpScored's tie-break chain and the two
	// disagree at the boundary, because the heap evicts a different entry than
	// the sort keeps.
	c := collectorFor(t)
	const wide, page = model.MaxPageItems*2 + model.MaxPageItems/4, 7
	for i := range wide {
		k := (i * 2237) % wide
		r := rankedOf(model.TierLexicalFTS, 1_000_000, "pkg/w.go", uint64(k%3),
			model.NodeID("w"+strconv.Itoa(k)), "wk"+strconv.Itoa(k))
		facts := hitFacts{hit: model.SearchHit{NodeID: r.NodeID, FileID: f.files["pkg/handler.go"],
			Path: r.Path, Kind: model.NodeFunction, Name: "w" + strconv.Itoa(k%5),
			QualifiedName: "pkg.W" + strconv.Itoa(k%5), Signature: "func w" + strconv.Itoa(k%5) + "() error"}}
		if err := c.add(r, facts, "matched the lexical tier"); err != nil {
			t.Fatalf("adding a wide candidate: %v", err)
		}
	}
	run, err := c.deduped()
	if err != nil {
		t.Fatalf("deduplicating the wide set: %v", err)
	}
	defer run.Close()
	want, err := rankOrder(t, run)
	if err != nil {
		t.Fatalf("ordering the wide set: %v", err)
	}
	h := &pageHeap{limit: page}
	if err := run.Each(func(v scored) error {
		v.ScoreMicros = v.Folded
		h.offer(v)
		return nil
	}); err != nil {
		t.Fatalf("feeding the heap: %v", err)
	}
	if got := h.page(); !reflect.DeepEqual(got, want[:page]) {
		for i := range got {
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("the heap's hit %d is %+v, the sorted run's is %+v: one comparator, two answers",
					i, got[i], want[i])
			}
		}
		t.Fatalf("the heap served %d hits and the sorted run's first page holds %d", len(got), page)
	}
}

// ---------------------------------------------------------------------------
// FX-H-U legs.
// ---------------------------------------------------------------------------

// stubFileReader answers File for any id by SYNTHESIZING the version, so a walk
// over 200 000 distinct files costs the stub nothing: a pre-built map of them
// would be the heap the leg below measures. Nothing else on the read surface is
// exercised, and the methods that are not say so rather than returning
// plausible empty answers.
type stubFileReader struct{ reads int }

func (s *stubFileReader) File(_ context.Context, id model.FileID) (model.FileVersion, error) {
	s.reads++
	return model.FileVersion{ID: id, Path: "pkg/" + string(id) + ".go"}, nil
}

func (s *stubFileReader) Node(context.Context, model.NodeID) (sqlite.StoredNode, error) {
	return sqlite.StoredNode{}, errors.New("not used")
}

func (s *stubFileReader) FileByPath(context.Context, string) (model.FileID, error) {
	return "", errors.New("not used")
}

func (s *stubFileReader) NodesInFile(context.Context, model.FileID, int64, model.NodeID, int) ([]sqlite.StoredNode, error) {
	return nil, errors.New("not used")
}

func (s *stubFileReader) DistinctNodesInFile(context.Context, model.FileID, int64, model.NodeID, int) ([]sqlite.StoredNode, error) {
	return nil, errors.New("not used")
}

func (s *stubFileReader) Nodes(context.Context, sqlite.NodeFilter, model.NodeID, int) ([]sqlite.StoredNode, error) {
	return nil, errors.New("not used")
}

// legPathResolverHoldsOnePage proves the exact tiers' path cache is bounded by
// the READ PAGE and not by the answer: the resolver used to memoize one entry
// per distinct file of the whole request, so a symbol query spanning a
// repository's files cost that much heap for the life of the request.
//
// Every path is still served -- the count below is the answer, and an evicted
// entry costs one more File read, which is the whole trade.
//
// Mutation: restore the unbounded map (keep every entry, never evict) and the
// ten-fold input moves the live set by ~24 MiB, failing the bound.
func legPathResolverHoldsOnePage(t *testing.T, _ *fixture) {
	const page = 1000
	resolve := func(n int) uint64 {
		t.Helper()
		p := newPathResolver(&stubFileReader{}, page)
		var held uint64
		for i := range n {
			got, err := p.path(context.Background(), model.FileID(fmt.Sprintf("f%08d", i)))
			if err != nil {
				t.Fatalf("path(%d): %v", i, err)
			}
			if got == "" {
				t.Fatalf("file %d resolved to no path", i)
			}
			if i == n-1 {
				var m runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&m)
				held = m.HeapAlloc
			}
		}
		return held
	}
	small := resolve(20_000)
	large := resolve(200_000)
	if large > small+(1<<20) {
		t.Fatalf("the path resolver held %d heap bytes over 20000 distinct files and %d over 200000; "+
			"its working set is one read page, not the answer's file count", small, large)
	}
	t.Logf("path-resolver live set: 20000 files %d bytes, 200000 files %d bytes", small, large)

	// The cache is a read-amplification knob, never a bound on the answer: a
	// repeat within the page is served from the cache, and one past it is
	// re-read rather than dropped.
	warm := &stubFileReader{}
	p := newPathResolver(warm, 2)
	for _, id := range []model.FileID{"a", "b", "a", "c", "b"} {
		if _, err := p.path(context.Background(), id); err != nil {
			t.Fatalf("path(%s): %v", id, err)
		}
	}
	if warm.reads != 4 {
		t.Fatalf("a 2-entry cache made %d File reads over a,b,a,c,b; want 4: the repeat inside the "+
			"cache is served from it and the evicted one is re-read", warm.reads)
	}
}

// ---------------------------------------------------------------------------
// FX-H-X1 legs.
// ---------------------------------------------------------------------------

// twoOffsetFixture is a second activated generation holding ONE file with ONE
// node that two units disagree about: the unverified unit declares it at byte
// 20, the verified one -- the Section 9.4 precedence winner -- at byte 100.
// node_facts is keyed (unit_id, node_id), so two units is the only way one file
// can carry a node at two offsets.
//
// It is built apart from the ranking corpus on purpose: fixtureDocs is the one
// corpus every Task 13 leg asserts against, and a second declaration of one of
// its nodes would move the answers those legs pin. No search units are
// published here, so no lexical tier answers and the hit count below is exactly
// what the exact_path tier emitted.
type twoOffsetFixture struct {
	ctx  context.Context
	opts Options
	path string
}

// newTwoOffsetFixture publishes that generation and returns the service options
// over it. winner is the offset of the precedence winner's declaration.
func newTwoOffsetFixture(t *testing.T, loserStart, winnerStart uint64) *twoOffsetFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	repo := model.RepositoryID(model.H("two-offset-fixture", "1"))
	if err := st.EnsureRepository(ctx, repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	cas, err := snapshot.OpenCAS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatalf("OpenCAS: %v", err)
	}
	// The body comfortably outruns the winner's offset: a span the blob cannot
	// hold is reported as an unresolved Range, which would fail this leg for a
	// reason that is not the one it guards.
	body := "package pkg\n" + strings.Repeat("// padding\n", 40)
	rec, err := cas.Put(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("CAS.Put: %v", err)
	}
	if err := st.PutBlob(ctx, rec); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	const path = "pkg/twice.go"
	fileID := model.NewFileID(repo, path)
	fv := model.FileVersion{ID: fileID, Path: path, Status: model.FileTracked,
		Size: int64(len(body)), ContentHash: rec.Hash, Language: "go"}
	manifest := model.H("two-offset-manifest")
	snap := model.Snapshot{ID: model.NewSnapshotID(repo, "", model.H("policy"), manifest), RepositoryID: repo,
		CaptureConsistency: model.CaptureValidated, SourcePolicyHash: model.H("policy"), FileCount: 1,
		ManifestHash: manifest, SourceBytes: uint64(len(body)), CreatedAt: time.Now().UTC()}
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

	const name = "Twice"
	key := model.CanonicalNodeKey(path, name)
	nodeID := model.NewNodeID(repo, model.NodeFunction, key)
	declare := func(scope string, binding model.SourceBinding, start uint64) {
		t.Helper()
		input := model.UnitInput{FileID: fileID, ContentHash: rec.Hash}
		h := model.NewUnitInputHasher()
		if err := h.Add(input); err != nil {
			t.Fatalf("UnitInputHasher.Add: %v", err)
		}
		spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
			ScopeKey: scope, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
		spec.ID = model.NewUnitID(spec, fixtureConfigHash)
		build := model.UnitBuild{Spec: spec, AnalysisConfigHash: fixtureConfigHash, OriginRunID: run,
			SourceBinding: binding}
		w, err := st.BeginUnit(ctx, gen, build,
			func(yield func(model.UnitInput) error) error { return yield(input) })
		if err != nil {
			t.Fatalf("BeginUnit(%s): %v", scope, err)
		}
		rng := &model.SourceRange{Start: model.Position{Byte: start, Line: 1},
			End: model.Position{Byte: start + 10, Line: 1, Column: 10}}
		ev := model.Evidence{UnitID: w.UnitID(), ProviderID: fixtureProviderID,
			ProviderVersion: fixtureProviderVersion, OriginRunID: run, NodeID: nodeID,
			Precision: model.PrecisionSyntax, FileID: fileID, ContentHash: rec.Hash, Range: rng}
		ev.ID = model.NewEvidenceID(ev)
		node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: name,
			QualifiedName: "pkg." + name, FileID: fileID, ContentHash: rec.Hash, Range: rng}
		if err := w.PutNodes(ctx, []model.NodeFact{{Node: node, CanonicalKey: key,
			Evidence: []model.Evidence{ev}}}); err != nil {
			t.Fatalf("PutNodes(%s): %v", scope, err)
		}
		if err := st.SealUnit(ctx, w); err != nil {
			t.Fatalf("SealUnit(%s): %v", scope, err)
		}
	}
	declare("scope-loose", model.SourceBindingUnverified, loserStart)
	declare("scope-strict", model.SourceBindingVerified, winnerStart)

	if _, err := st.Activate(ctx, gen, 0, model.HealthFresh,
		[]model.CapabilityState{{ProviderID: fixtureProviderID, Capability: "structure", Scope: "workspace",
			State: model.CapabilityFresh}}, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "spools"), 1<<20, st)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	return &twoOffsetFixture{ctx: ctx, path: path, opts: Options{Store: st, Repo: repo, Signer: signer,
		Spools: spools, Leases: pagination.NewLeases(st, pagination.DefaultCursorTTL), Content: cas,
		Resources: config.Defaults().Resources, CursorTTL: pagination.DefaultCursorTTL,
		Now: func() time.Time { return time.Now().UTC() }}}
}

// legTwoOffsetNodeServesThePrecedenceWinner proves the served answer, not just
// the storage read: a node two units declare at two offsets reaches search ONCE
// and at the offset the Section 9.4 precedence order picks -- the same range
// Nodes reports for that identity. The winner is the HIGHER offset here, so a
// rule that took the first declaration in document order, or that emitted both
// rows, fails rather than coincidentally agreeing.
//
// Mutation: order the distinct predicate in storage/sqlite/search.go by
// start_byte instead of the precedence keys, or drop the predicate entirely --
// either way the hit is served at byte 20. (Dropping it still serves ONE hit:
// the ranked set folds by node identity, which is why the count assertion
// alone would not have caught the wrong offset.)
func legTwoOffsetNodeServesThePrecedenceWinner(t *testing.T, _ *fixture) {
	const loser, winner = 20, 100
	f := newTwoOffsetFixture(t, loser, winner)
	s := newService(t, f.opts)
	page, err := s.Search(f.ctx, model.SearchRequest{Query: f.path, Page: model.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("Search(%s): %v", f.path, err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the file's one node, declared by two units, was served %d times: a retrieval tier "+
			"emits a node identity once (%+v)", len(page.Items), page.Items)
	}
	hit := page.Items[0]
	if hit.Range == nil {
		t.Fatalf("the served hit carries no source range: %+v", hit)
	}
	if hit.Range.Start.Byte != winner {
		t.Fatalf("the hit is served at byte %d, want the precedence winner's %d; the winner is the LATER "+
			"declaration, so serving %d is the first-declaration reading", hit.Range.Start.Byte, winner, loser)
	}
}

// TestSearchContinuationOverACollectedGeneration proves the across-call end of a pin
// that holds its generation by snapshot rather than by lease. Both endpoints
// here pin the generation the TOKEN names, so a continuation presented after
// the retention pass behind another process's activation has collected that
// generation finds no row at all.
//
// The answer owed there is the cursor's own family: the caller typed nothing,
// it presented a token this service minted, and what it must be told is to
// re-run from the first page. Reported as CTX_ARGUMENT_INVALID -- which is what
// the store raises, correctly, for an operator who named the generation by hand
// -- it reads as a command line typed wrong, and a caller paging through an
// answer while an index publishes has no way to tell that it should simply
// start again.
//
// Mutation: make resumePinFailure return err unchanged. The leg then fails with
// CTX_ARGUMENT_INVALID.
// It collects a generation, so it runs on a fixture of its own rather than as a
// leg of the shared-corpus scenario.
func TestSearchContinuationOverACollectedGeneration(t *testing.T) {
	f := newFixture(t)
	s := newService(t, f.opts)
	first, err := s.Search(f.ctx, model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}})
	if err != nil {
		t.Fatalf("Search(page 1): %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("a bounded page over three matching documents minted no continuation")
	}
	// The chain is walked to its end, which releases every cursor lease along
	// it: a continuation is single-use, so the token page 1 handed back is now
	// one a caller may still present and nothing retains its generation for.
	// That is the only state in which the generation a live token names can be
	// collected, and it is an ordinary one -- a caller that retries a page it
	// has already followed.
	cursor := first.Meta.NextCursor
	for cursor != "" {
		page, err := s.Search(f.ctx, model.SearchRequest{Query: "handle",
			Page: model.PageRequest{Limit: 1, Cursor: cursor}})
		if err != nil {
			t.Fatalf("Search(continuation): %v", err)
		}
		cursor = page.Meta.NextCursor
	}
	// Another process publishes the next generation, which supersedes the one
	// the cursor names, and collects it.
	st := f.opts.Store
	gen2, err := st.BeginGeneration(f.ctx, f.repo, f.binding.SnapshotID, model.H("semantic-next"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(2): %v", err)
	}
	// The sealed unit of the first generation is reused rather than rebuilt,
	// which is what an incremental run does: the second generation differs from
	// the first only in the membership it publishes.
	if err := st.AttachUnit(f.ctx, gen2, f.unitOf(fixtureDocs[0])); err != nil {
		t.Fatalf("AttachUnit: %v", err)
	}
	if _, err := st.Activate(f.ctx, gen2, f.gen, model.HealthFresh, nil, "norm-v1"); err != nil {
		t.Fatalf("Activate(2): %v", err)
	}
	if err := st.DeleteGeneration(f.ctx, f.gen); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	_, err = s.Search(f.ctx, model.SearchRequest{Query: "handle",
		Page: model.PageRequest{Limit: 1, Cursor: first.Meta.NextCursor}})
	if err == nil {
		t.Fatal("a continuation over a collected generation was served")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
		t.Fatalf("a continuation whose generation was collected failed as %v, want CTX_CURSOR_INVALID", err)
	}
	if !strings.Contains(typed.Message, "first page") {
		t.Errorf("the refusal does not tell the caller what to do: %q", typed.Message)
	}
}

// unitOf is the id of the file-scoped unit publish seals for d, recomputed from
// the same spec rather than remembered, so it cannot drift from what was built.
func (f *fixture) unitOf(d doc) model.UnitID {
	f.t.Helper()
	h := model.NewUnitInputHasher()
	if err := h.Add(model.UnitInput{FileID: f.files[d.path], ContentHash: f.hashes[d.path]}); err != nil {
		f.t.Fatalf("UnitInputHasher.Add(%s): %v", d.path, err)
	}
	spec := model.UnitSpec{ProviderID: fixtureProviderID, ProviderVersion: fixtureProviderVersion,
		ScopeKey: d.path, InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	return model.NewUnitID(spec, fixtureConfigHash)
}

// legWriterlessSymbolPagesInFull proves that `symbol` keeps PAGING from a
// process that opened the store read-only -- the composition that answers while
// another process is indexing.
//
// Unlike search's, this continuation writes nothing: it carries its whole
// position in the token and retains no spool, so the only thing it ever needed
// a lease for was to retain the generation it names. The read snapshot of the
// call that presents it does that, and a generation collected in between is
// answered in the cursor's family. Requiring the lease anyway made an answer
// larger than one page unreachable for the whole length of an index -- a
// symbol query that silently stops after its first page is the failure this
// guards.
//
// Mutation: drop the reader.Continuable() branch in keysetNext so the lease is
// always acquired. The leg then fails with the read-only store's refusal.
func legWriterlessSymbolPagesInFull(t *testing.T, f *fixture) {
	ro, err := sqlite.Open(f.ctx, f.dbPath, sqlite.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	opts := f.opts
	opts.Store = ro
	opts.Leases = pagination.NewLeases(ro, pagination.DefaultCursorTTL)
	s := newService(t, opts)

	req := model.SymbolRequest{Query: "Handle", Operation: model.SymbolResolve,
		SemanticSource: model.SemanticCanonical, Page: model.PageRequest{Limit: 1}}
	first, err := s.Resolve(f.ctx, req)
	if err != nil {
		t.Fatalf("a writerless symbol query failed instead of answering: %v", err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("page 1 served %d nodes, want 1", len(first.Items))
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("a writerless symbol query stopped after one page of a longer answer")
	}
	seen := len(first.Items)
	cursor := first.Meta.NextCursor
	for cursor != "" {
		req.Page.Cursor = cursor
		page, err := s.Resolve(f.ctx, req)
		if err != nil {
			t.Fatalf("a writerless symbol continuation failed: %v", err)
		}
		seen += len(page.Items)
		cursor = page.Meta.NextCursor
	}
	whole := model.SymbolRequest{Query: "Handle", Operation: model.SymbolResolve,
		SemanticSource: model.SemanticCanonical}
	all, err := s.Resolve(f.ctx, whole)
	if err != nil {
		t.Fatalf("Resolve(unpaged): %v", err)
	}
	if seen != len(all.Items) {
		t.Fatalf("paging a writerless symbol query reached %d nodes, want the %d the one-shot answer has", seen, len(all.Items))
	}
}
