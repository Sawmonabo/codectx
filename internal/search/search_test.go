package search

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"slices"
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
	repo    model.RepositoryID
	gen     model.GenerationID
	binding model.Binding
	opts    Options
	nodes   map[string]model.NodeID
	files   map[string]model.FileID
}

// newFixture builds and activates the corpus. It runs at every lane's entry so
// a corpus that no longer publishes is a failure here, not five lanes later.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := sqlite.Open(ctx, filepath.Join(dir, "codectx.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	f := &fixture{t: t, ctx: ctx, store: st, repo: model.RepositoryID(model.H("search-fixture", "1")),
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
		f.publish(run, d, hashes[d.path])
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
// documents into the staging generation.
func (f *fixture) publish(run model.ProviderRunID, d doc, hash string) {
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
	w, err := f.store.BeginUnit(f.ctx, f.gen, build,
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
		{"rank/ranked_set_bound_truncates_loudly", legRankedSetBound},
		{"rank/reasons_stay_within_their_bounds", legReasonBounds},
		{"rank/range_hydration_reads_a_bounded_window", legRangeHydration},
		{"cursor/query_hash_binds_the_query_and_its_filters", legQueryHash},
		{"cursor/tampered_expired_and_foreign_cursors_are_rejected", legCursorRejections},
		{"cursor/resolve_pages_by_a_bounded_keyset", legResolveKeyset},
		{"cursor/search_pages_through_a_spool", legSpooledPage},
		{"scenario/search_serves_the_section_14_2_order", legEndToEndRanking},

		// FX-C13b rows
		{"fix/path_tier_announces_its_bound_and_filters_the_keyset", legPathTierBound},
		{"fix/a_continuation_carries_the_answer_level_truncation", legContinuationTruncation},
		{"fix/symbol_resolves_a_canonical_node_id", legSymbolByCanonicalID},
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

// legLessChain proves the full Section 14.2 chain, one key at a time. A
// dropped or reordered key would silently reorder results for identical
// inputs, which is the determinism guarantee Section 14.2 sells.
func legLessChain(t *testing.T, _ *fixture) {
	base := rankedOf(model.TierExactName, 100, "b", 10, "n2", "k2")
	cases := []struct {
		key   string
		lower ranked
	}{
		{"tier", rankedOf(model.TierExactPath, 0, "b", 10, "n2", "k2")},
		{"score", rankedOf(model.TierExactName, 101, "b", 10, "n2", "k2")},
		{"path", rankedOf(model.TierExactName, 100, "a", 10, "n2", "k2")},
		{"start_byte", rankedOf(model.TierExactName, 100, "b", 9, "n2", "k2")},
		{"node_id", rankedOf(model.TierExactName, 100, "b", 10, "n1", "k2")},
		{"search_key", rankedOf(model.TierExactName, 100, "b", 10, "n2", "k1")},
	}
	for _, c := range cases {
		if !c.lower.less(base) {
			t.Errorf("%s: want the %s-lower candidate to sort first", c.key, c.key)
		}
		if base.less(c.lower) {
			t.Errorf("%s: the order is not antisymmetric", c.key)
		}
	}
	// A tier with a worse score still outranks a better-scoring later tier:
	// tier is the FIRST key, not a tiebreak after score.
	exact := rankedOf(model.TierExactQualifiedName, 0, "z", 99, "n9", "k9")
	lexical := rankedOf(model.TierLexicalFTS, 9_999_999, "a", 0, "n0", "k0")
	if !exact.less(lexical) {
		t.Error("a zero-scored exact hit must outrank a high-scoring lexical hit")
	}
	if base.less(base) {
		t.Error("less must be irreflexive")
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
	c := newCollector()
	c.add(rankedOf(model.TierLexicalFTS, 4_200_000, "pkg/alpha.go", 0, "node-a", "doc-a1"), "lexical_fts match")
	c.add(rankedOf(model.TierExactName, 0, "pkg/alpha.go", 0, "node-a", "doc-a2"), "exact_name match")
	c.add(rankedOf(model.TierLexicalFTS, 1_000_000, "pkg/beta.go", 0, "node-b", "doc-b1"), "lexical_fts match")
	got := c.results()
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
	c := newCollector()
	c.add(rankedOf(model.TierLexicalFTS, 3, "pkg/alpha.go", 0, "", "doc-1"))
	c.add(rankedOf(model.TierLexicalFTS, 5, "pkg/alpha.go", 0, "", "doc-2"))
	c.add(rankedOf(model.TierLexicalFTS, 7, "pkg/alpha.go", 64, "", "doc-3"))
	c.add(rankedOf(model.TierLexicalFTS, 9, "pkg/beta.go", 0, "", "doc-4"))
	got := c.results()
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

// legRankedSetBound proves that overflowing the candidate bound is announced.
// A silent drop would serve a caller a confidently incomplete answer with no
// way to detect it (digest §6).
func legRankedSetBound(t *testing.T, _ *fixture) {
	c := newCollector()
	for i := range maxRankedHits {
		c.add(rankedOf(model.TierLexicalFTS, int64(i), "p", 0, model.NodeID("n"+strconv.Itoa(i)), "k"+strconv.Itoa(i)))
	}
	if truncated, _ := c.truncation(); truncated {
		t.Fatalf("the set is truncated at exactly %d distinct results", maxRankedHits)
	}
	c.add(rankedOf(model.TierExactPath, 0, "p", 0, "overflow", "k-overflow"))
	truncated, reason := c.truncation()
	if !truncated {
		t.Error("the candidate bound was exceeded without setting Truncated")
	}
	if reason == "" || len(reason) > model.MaxReasonBytes {
		t.Errorf("truncation reason = %q, want a non-empty reason within %d bytes", reason, model.MaxReasonBytes)
	}
	got := c.results()
	if len(got) != maxRankedHits {
		t.Fatalf("results = %d, want the bound %d", len(got), maxRankedHits)
	}
	for _, r := range got {
		if r.NodeID == "overflow" {
			t.Fatal("the refused candidate entered the set anyway")
		}
	}
	// An already-present key still folds past the bound: dropping the fold
	// would understate an occurrence count that is already being served.
	c.add(rankedOf(model.TierLexicalFTS, 0, "p", 0, "n0", "k0"))
	if got := c.results(); got[len(got)-1].Occurrences != 2 {
		t.Errorf("a fold into an existing key was refused past the bound")
	}
}

// legReasonBounds proves the Section 14.3 explanation bounds survive folding.
// A folded hit with nine reasons, or one reason of a megabyte, fails
// model.SearchHit.Validate and takes the whole page down.
func legReasonBounds(t *testing.T, _ *fixture) {
	c := newCollector()
	long := strings.Repeat("x", model.MaxReasonBytes+64)
	for i := range model.MaxReasonsPerEntry + 4 {
		c.add(rankedOf(model.TierLexicalFTS, 1, "p", 0, "n", "k"), "reason "+strconv.Itoa(i), long, "reason 0")
	}
	got := c.results()[0]
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

// legSpooledPage proves the Search continuation. Ranking is global, so page 2
// comes out of a spool; a spool that replayed out of order, or that a released
// lease still served, would hand the caller a differently ranked page under
// the same query.
func legSpooledPage(t *testing.T, f *fixture) {
	leases := pagination.NewLeases(f.store, time.Minute)
	lease, err := leases.Acquire(f.ctx, f.gen, "", model.LeaseCursor)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	now := time.Now().UTC()
	qh := searchQueryHash(model.SearchRequest{Query: "handle"})
	c := newCursor(endpointSearch, f.binding, lease.ID, qh, now.Add(time.Minute))

	want := []model.SearchHit{
		{FileID: f.files["pkg/alpha.go"], Path: "pkg/alpha.go", Kind: model.NodeFunction, Tier: model.TierExactName,
			Name: "Handle", ScoreMicros: 4_200_000, OccurrenceCount: 2},
		{FileID: f.files["pkg/beta.go"], Path: "pkg/beta.go", Kind: model.NodeFunction, Tier: model.TierLexicalFTS,
			Name: "HandleRequest", ScoreMicros: 1_000_000, OccurrenceCount: 1},
	}
	// The answer-level metadata the first page computed travels with the
	// remainder: a continuation reads its hits from the spool and has nothing
	// of its own to recompute truncation from.
	answer := spoolMeta{Truncated: true, TruncationReason: truncationExactTierFull}
	id, err := spoolHits(f.opts.Spools, c, answer, want)
	if err != nil {
		t.Fatalf("spoolHits: %v", err)
	}
	if !model.ValidHexID(id) {
		t.Fatalf("spool id %q is not a well-formed id", id)
	}
	if got, err := spoolHits(f.opts.Spools, c, answer, nil); err != nil || got != "" {
		t.Fatalf("spoolHits(nothing left) = (%q, %v), want (\"\", nil)", got, err)
	}

	next := c
	next.SpoolID = id
	if err := next.Validate(); err != nil {
		t.Fatalf("a spooled cursor does not validate: %v", err)
	}
	meta, got, err := readSpool(f.ctx, f.opts.Spools, next, now)
	if err != nil {
		t.Fatalf("readSpool: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("readSpool replayed %+v, want %+v", got, want)
	}
	if !meta.Truncated || meta.TruncationReason != truncationExactTierFull {
		t.Fatalf("readSpool replayed truncation (%t, %q), want the first page's (true, %q)",
			meta.Truncated, meta.TruncationReason, truncationExactTierFull)
	}

	// A spool bound to another query is not this cursor's continuation.
	foreign := next
	foreign.QueryHash = searchQueryHash(model.SearchRequest{Query: "other"})
	_, _, err = readSpool(f.ctx, f.opts.Spools, foreign, now)
	assertCode(t, "foreign spool", err, model.CodeCursorInvalid)

	// Releasing the lease ends the continuation rather than serving a page
	// from a generation retention is free to collect.
	if err := leases.Release(f.ctx, lease.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, _, err = readSpool(f.ctx, f.opts.Spools, next, now)
	assertCode(t, "released lease", err, model.CodeCursorInvalid)
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
}

func (c *countingCAS) ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error) {
	c.reads++
	c.bytes += r.End - r.Start
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

func (s *stubExact) Nodes(context.Context, sqlite.NodeFilter, model.NodeID, int) ([]sqlite.StoredNode, error) {
	return nil, nil
}

// legPathTierBound proves the exact_path tier both announces its overflow and
// applies the kind filter across the whole keyset rather than to one bounded
// read. Serving five of eight candidates as a complete answer, or answering
// "nothing here" for a file whose three functions sit past the bound, are both
// the silent drop Section 14.3 forbids.
func legPathTierBound(t *testing.T, _ *fixture) {
	const limit = 5
	stub := &stubExact{file: model.FileID(model.H("stub-file")), path: "pkg/wide.go"}
	for i := range 8 {
		kind := model.NodeVariable
		if i >= limit {
			kind = model.NodeFunction
		}
		id := model.NodeID(model.H("stub-node", strconv.Itoa(i)))
		stub.nodes = append(stub.nodes, sqlite.StoredNode{
			Node:  model.Node{ID: id, Kind: kind, Name: "n" + strconv.Itoa(i)},
			Bytes: &model.ByteRange{Start: uint64(i * 10), End: uint64(i*10 + 1)},
		})
	}

	out, full, err := pathCandidates(context.Background(), stub, stub.path, nil, limit)
	if err != nil {
		t.Fatalf("pathCandidates(unfiltered): %v", err)
	}
	if len(out) != limit || !full {
		t.Fatalf("pathCandidates(unfiltered) served %d candidates, truncated=%t; want %d and true: three of the eight were dropped",
			len(out), full, limit)
	}

	out, full, err = pathCandidates(context.Background(), stub, stub.path, []model.NodeKind{model.NodeFunction}, limit)
	if err != nil {
		t.Fatalf("pathCandidates(kind filter): %v", err)
	}
	if len(out) != 3 || full {
		t.Fatalf("pathCandidates(kind filter) served %d candidates, truncated=%t; want 3 and false: the filter runs over the keyset, not over one bounded read",
			len(out), full)
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

	leases := pagination.NewLeases(f.store, time.Minute)
	lease, err := leases.Acquire(f.ctx, f.gen, "", model.LeaseCursor)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	now := time.Now().UTC()
	c := newCursor(endpointSearch, f.binding, lease.ID, searchQueryHash(req), now.Add(time.Minute))
	id, err := spoolHits(f.opts.Spools, c, spoolMeta{Truncated: true, TruncationReason: truncationExactTierFull}, second.Items)
	if err != nil {
		t.Fatalf("spoolHits: %v", err)
	}
	c.SpoolID = id
	token, err := f.opts.Signer.EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	continued.Page.Cursor = token
	page, err := s.Search(f.ctx, continued)
	if err != nil {
		t.Fatalf("Search(truncated continuation): %v", err)
	}
	if !page.Meta.Truncated || page.Meta.TruncationReason != truncationExactTierFull {
		t.Fatalf("the continuation reports (%t, %q), want the first page's (true, %q)",
			page.Meta.Truncated, page.Meta.TruncationReason, truncationExactTierFull)
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
