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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

// The Section 14 ranking fixture. Three small lexical documents in one
// activated generation are the smallest corpus that can separate the exact
// tiers from the lexical tier, exercise an indexed prefix range, carry a
// Unicode identifier and a literal-punctuation body through the query parser,
// and fold two documents of one node into one hit. The scenario's legs assert
// against this one corpus; a leg that needs another shape builds its own.
const (
	fixtureProviderID      = "treesitter"
	fixtureProviderVersion = "1.0.0"
	fixtureConfigHash      = "cfg"
)

// doc describes one fixture document: its file, its node identity and the
// indexed text the lexical tier scores. Occurrences > 1 publishes that many search
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

// storeOptions opens a fixture store, read-write or read-only, with its search
// index sized for one test: one thread of each kind and one query slot.
func storeOptions(readOnly bool) sqlite.Options {
	return sqlite.Options{ReadOnly: readOnly, ReadConnections: config.ReadConnections(config.Defaults()),
		TextIndex: textindex.Options{WriterThreads: 1, WriterMemoryBytes: textindex.WriterMemoryFloor(1),
			MergeThreads: 1, SearchThreads: 1, QuerySlots: 1, ViewCacheBytes: 1 << 20}}
}

// newFixture builds and activates the ranking corpus.
func newFixture(t *testing.T) *fixture { return newFixtureOf(t, fixtureDocs) }

// newFixtureOf builds and activates one corpus, one document set per file, so a
// corpus that no longer publishes is a failure here rather than in a later leg.
func newFixtureOf(t *testing.T, docs []doc) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "codectx.db")
	st, err := sqlite.Open(ctx, dbPath, storeOptions(false))
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
	hashes := make(map[string]string, len(docs))
	f.hashes = hashes
	files := make([]model.FileVersion, 0, len(docs))
	for _, d := range docs {
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
	for _, d := range docs {
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
	f.opts = Options{Store: st, Repo: f.repo, Signer: signer,
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
			Bytes: model.ByteRange{Start: 0, End: end}, Body: d.body,
		})
	}
	if err := w.PutSearchUnits(f.ctx, units); err != nil {
		f.t.Fatalf("PutSearchUnits(%s): %v", d.path, err)
	}
	if err := f.store.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", d.path, err)
	}
}

// newService builds the service under test over the fixture and closes it
// when the test ends.
func newService(t *testing.T, o Options) *Service {
	t.Helper()
	s, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// leg is one scenario leg, named by the component it exercises.
type leg struct {
	name string
	run  func(t *testing.T, f *fixture)
}

// TestSearchRankingScenario is the package's single scenario: one corpus, one
// activated generation, every leg that only reads it.
func TestSearchRankingScenario(t *testing.T) {
	legs := []leg{
		{"rank/range_hydration_reads_a_bounded_window", legRangeHydration},
		{"cursor/query_hash_binds_the_query_its_filters_and_its_scoring", legQueryHash},
		{"cursor/tampered_expired_and_foreign_cursors_are_rejected", legCursorRejections},
		{"cursor/resolve_pages_by_a_bounded_keyset", legResolveKeyset},
		{"cursor/search_positions_round_trip_and_refuse_foreign_keys", legSearchPositionKeys},
		{"scenario/search_serves_the_section_14_2_order", legEndToEndRanking},
		{"exact/path_tier_walks_the_whole_keyset_and_filters_it", legPathTierLossless},
		{"exact/exact_tiers_hold_one_read_page", legExactTiersHoldOnePage},
		{"exact/exact_path_tier_owns_its_files_nodes", legExactPathTierOwnsItsFile},
		{"page/a_writerless_search_pages_in_full", legWriterlessSearchPagesInFull},
		{"page/a_writerless_symbol_query_pages_in_full", legWriterlessSymbolPagesInFull},
		{"resolve/symbol_resolves_a_canonical_node_id", legSymbolByCanonicalID},
		{"page/a_clamped_page_bound_is_reported_on_the_answer", legPageClampIsReported},
		{"page/the_query_deadline_ends_a_page_not_the_answer", legDeadlineEndsThePage},
		{"page/an_unbounded_query_answers_in_full", legUnboundedQueryAnswersInFull},
		{"resolve/the_path_resolver_holds_one_read_page", legPathResolverHoldsOnePage},
		{"exact/a_two_offset_node_is_served_at_the_precedence_winner",
			legTwoOffsetNodeServesThePrecedenceWinner},
	}
	f := newFixture(t)
	for _, l := range legs {
		t.Run(l.name, func(t *testing.T) { l.run(t, f) })
	}
}

// ---------------------------------------------------------------------------
// Cursor legs. A continuation must name the generation, query, filters and
// scoring it was minted for, or the caller silently reads page 2 of a
// different question.
// ---------------------------------------------------------------------------

// legQueryHash proves the cursor's query binding. A hash that ignored a filter
// or the length rule would let a cursor minted for one answer resume against
// another, serving page 2 of a different question under the
// same token.
func legQueryHash(t *testing.T, _ *fixture) {
	base := model.SearchRequest{Query: "handle request",
		Kinds:     []model.NodeKind{model.NodeFunction, model.NodeClass},
		Languages: []string{"go", "rust"}, Paths: []string{"pkg/", "cmd/"}}
	reordered := model.SearchRequest{Query: " handle   request ",
		Kinds:     []model.NodeKind{model.NodeClass, model.NodeFunction},
		Languages: []string{"rust", "go"}, Paths: []string{"cmd/", "pkg/"}}
	h := searchQueryHash(base, textindex.LengthTotal)
	if !model.ValidHexID(h) {
		t.Fatalf("query hash %q is not a well-formed id; Cursor.Validate rejects it", h)
	}
	if got := searchQueryHash(reordered, textindex.LengthTotal); got != h {
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
		if searchQueryHash(c.req, textindex.LengthTotal) == h {
			t.Errorf("changing %s did not change the query hash", c.name)
		}
	}
	if searchQueryHash(base, textindex.LengthWeighted) == h {
		t.Error("changing the length rule did not change the query hash")
	}
	// A filter group is separated, so "a\x00b" and "ab" are different queries.
	split := model.SearchRequest{Query: base.Query, Kinds: base.Kinds, Languages: base.Languages, Paths: []string{"pk", "g/cmd/"}}
	if searchQueryHash(split, textindex.LengthTotal) == searchQueryHash(model.SearchRequest{Query: base.Query, Kinds: base.Kinds,
		Languages: base.Languages, Paths: []string{"pkg/cmd/"}}, textindex.LengthTotal) {
		t.Error("filter values are concatenated without a separator")
	}
	// The two endpoints never collide on one preimage.
	if symbolQueryHash(model.SymbolRequest{Query: base.Query}) == searchQueryHash(model.SearchRequest{Query: base.Query}, textindex.LengthTotal) {
		t.Error("the search and symbol endpoints hash one query to one value")
	}
}

// legCursorRejections proves cursor integrity: a tampered, expired, cross-endpoint,
// repinned or requeried cursor is CTX_CURSOR_INVALID. Accepting any of them
// serves a page from a generation or question the caller never asked about.
func legCursorRejections(t *testing.T, f *fixture) {
	signer := f.opts.Signer
	now := time.Now().UTC()
	qh := searchQueryHash(model.SearchRequest{Query: "handle"}, textindex.LengthTotal)
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
		searchQueryHash(model.SearchRequest{Query: "handle", Languages: []string{"go"}}, textindex.LengthTotal)), model.CodeCursorInvalid)
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

// stubFiles and stubBlobs stand in for the pinned reader and the store behind
// the hydrator's fileReader and blobReader, so the leg decides exactly which
// file and blob a hit resolves to. The CAS itself is real: the bytes, the block digests and the line
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

// legRangeHydration proves that a served hit carries a real source
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
	hits := []model.SearchHit{{FileID: file, Path: "pkg/café.go", Kind: model.NodeFunction, Tier: model.TierLexical}}
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
	more := []model.SearchHit{{FileID: file, Path: "pkg/café.go", Kind: model.NodeFunction, Tier: model.TierLexical}}
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

	// (a) A blob whose line checkpoints do NOT reach the hit. On a real
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
	sparseHits := []model.SearchHit{{FileID: sparseFile, Path: "pkg/sparse.go", Kind: model.NodeFunction, Tier: model.TierLexical}}
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
	longHits := []model.SearchHit{{FileID: file, Path: "web/bundle.min.js", Kind: model.NodeFunction, Tier: model.TierLexical}}
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

	// Cancellation and deadline are different answers (Section 22).
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

// legEndToEndRanking is the end-to-end retrieval leg: the Section 14.2 order
// over the real corpus, through the real service and the real search index,
// exact and lexical retrieval together.
//
// The exact tiers are not ranked: every exact hit scores 0 and is one
// occurrence, and it sorts above every lexical hit. A lexical result whose node
// an exact tier served is not served again, so the node the query names
// exactly appears once, at its exact tier. The two documents of one node fold
// into one lexical result counting both.
//
// Mutation: serve the lexical sequence without the exact fold (drop the
// exact fold (exactFold.served) in lexicalPhase) and the alpha node is served twice.
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
	if top.Tier != model.TierExactQualifiedName || top.NodeID != f.nodes["pkg/alpha.go"] {
		t.Fatalf("top hit is %q at tier %q, want the alpha node at %q", top.NodeID, top.Tier, model.TierExactQualifiedName)
	}
	if top.ScoreMicros != 0 || top.OccurrenceCount != 1 {
		t.Errorf("the exact hit scores %d with %d occurrences, want 0 and 1: the exact tiers are not ranked",
			top.ScoreMicros, top.OccurrenceCount)
	}
	if top.Range == nil {
		t.Error("the served hit carries no source range")
	}
	alpha, lexical := 0, 0
	for i, hit := range page.Items {
		if hit.NodeID == f.nodes["pkg/alpha.go"] {
			alpha++
		}
		if hit.Tier == model.TierLexical {
			lexical++
			if hit.ScoreMicros <= 0 {
				t.Errorf("lexical hit %d scores %d, want a positive score", i, hit.ScoreMicros)
			}
		} else if hit.ScoreMicros != 0 {
			t.Errorf("hit %d is tier %q with ScoreMicros = %d, want 0", i, hit.Tier, hit.ScoreMicros)
		}
		if i > 0 && page.Items[i-1].Tier.Rank() > hit.Tier.Rank() {
			t.Errorf("hit %d (%q) sorts after a later tier", i, hit.Tier)
		}
		if i > 0 && hit.Tier == model.TierLexical && page.Items[i-1].Tier == model.TierLexical &&
			page.Items[i-1].ScoreMicros < hit.ScoreMicros {
			t.Errorf("lexical hit %d scores %d above its predecessor's %d", i, hit.ScoreMicros, page.Items[i-1].ScoreMicros)
		}
	}
	if alpha != 1 {
		t.Errorf("the alpha node was served %d times, want once, at its exact tier", alpha)
	}
	if lexical == 0 {
		t.Error("the query returned no lexical hit: the beta and café documents match its terms")
	}

	// Two documents of one node fold into one lexical result counting both.
	handle, err := s.Search(f.ctx, model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("Search(handle): %v", err)
	}
	folded := 0
	for _, hit := range handle.Items {
		if hit.NodeID == f.nodes["pkg/alpha.go"] {
			folded++
			if hit.Tier != model.TierLexical || hit.OccurrenceCount != 2 {
				t.Errorf("the alpha node is served at %q with %d occurrences, want lexical with its 2 documents",
					hit.Tier, hit.OccurrenceCount)
			}
		}
	}
	if folded != 1 {
		t.Errorf("the alpha node's two documents were served as %d results, want one", folded)
	}

	// The prefix tier serves both pkg.Beta* and pkg.Alpha* documents through
	// the indexed range, never a leading wildcard.
	prefix, err := s.Search(f.ctx, model.SearchRequest{Query: "pkg.", Page: model.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatalf("Search(prefix): %v", err)
	}
	prefixed := 0
	for _, hit := range prefix.Items {
		if hit.Tier == model.TierQualifiedNamePrefix {
			prefixed++
		}
	}
	if prefixed != len(fixtureDocs) {
		t.Errorf("the prefix query served %d prefix-tier hits, want every fixture node", prefixed)
	}

	// A Unicode identifier and a literal-punctuation query are text to the
	// lexical tier, never syntax.
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

// legSearchPositionKeys proves a continuation's position survives its token
// and that a key this endpoint did not write is refused rather than read as a
// restarted answer. A Resolve key, a spool cursor's empty key, a negative score
// and a second spelling of one lexical position are all CTX_CURSOR_INVALID.
func legSearchPositionKeys(t *testing.T, f *fixture) {
	for _, want := range []searchPosition{
		{phase: phaseExact, rank: model.TierExactName.Rank(), node: f.nodes["pkg/alpha.go"]},
		{phase: phaseLexical},
		{phase: phaseLexical, after: &textindex.Position{ScoreMicros: 4_120_000, DocID: 77}},
		{phase: phaseLexical, after: &textindex.Position{ScoreMicros: math.MaxInt64, DocID: math.MaxUint64}},
	} {
		key := want.lastKey()
		if len(key) > 1024 {
			t.Fatalf("the position key is %d bytes, over the Cursor.LastKey bound", len(key))
		}
		got, err := parseSearchPosition(key)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("parseSearchPosition(%q) = %+v, %v; want %+v", key, got, err, want)
		}
	}
	node := string(f.nodes["pkg/alpha.go"])
	for _, bad := range []string{"", "l", "l\x00-1\x007", "l\x00x\x007", "l\x0012", "l\x0012\x00", "l\x00012\x007",
		"l\x00+12\x007", "l\x0012\x0007", "l\x0012\x00-7", "l\x0012\x007\x00", "x\x004\x00" + node,
		"x\x00-1\x00" + node, "x\x001\x00not-an-id", resolveKey(model.TierExactName, node)} {
		_, err := parseSearchPosition(bad)
		assertCode(t, "key "+strconv.Quote(bad), err, model.CodeCursorInvalid)
	}
}

// ---------------------------------------------------------------------------
// Exact-tier, paging and resolve legs. Each guards one behaviour no other leg
// sees: mutating the guarded line leaves every other leg green.
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
		if err := pathCandidates(context.Background(), stub, stub.path, stub.file, kinds, page,
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
		Node: model.Node{ID: model.NodeID(fmt.Sprintf("%0*x", model.IDHexLen, i)), Kind: model.NodeFunction,
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
	// hex id encodes; rescanning from zero on every page would make the leg's
	// cost quadratic in a corpus it walks 200 000 nodes of.
	first := 0
	if after != "" {
		i, err := strconv.ParseUint(string(after), 16, 64)
		if err != nil {
			return nil, err
		}
		first = int(i) + 1
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
// structure: a `seen` set deciding the cross-tier "most specific tier wins"
// rule would hold one model.NodeID per distinct candidate, so a wide
// qualified_name_prefix scan would cost heap proportional to the match count.
// The rule is decided over the candidate in hand and the earlier tiers' own
// keysets (tierRule), and the working set is one read page per tier however
// many candidates the tier yields.
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
		if err := exactCandidates(context.Background(), stub, "Widget", nil, hitFilter{}, 1000, allExactTiers,
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
	// exact_name tier, and each must be emitted ONCE -- tierRule assigns each
	// node its one tier, with no candidate-sized seen set.
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

// legExactPathTierOwnsItsFile proves the tier-0 half of tierRule: when the
// query is BOTH a path and a symbol name, every declaration of that file is
// emitted once, by the exact_path tier that outranks the symbol tiers, and the
// symbol tiers do not emit it again.
//
// Mutation: drop the pathFile block from tierRule.earlier and the prefix tier
// re-emits all 500, doubling the count.
func legExactPathTierOwnsItsFile(t *testing.T, _ *fixture) {
	const n = 500
	// The query resolves as a path AND matches the symbol tiers' filters.
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "Widget"}
	byTier := map[model.SearchTier]int{}
	if err := exactCandidates(context.Background(), stub, stub.path, nil, hitFilter{}, 100, allExactTiers,
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

// legWriterlessSearchPagesInFull proves that a search answered by a process
// that opened the store read-only -- which is how `codectx search` answers
// while another process is indexing -- pages through its WHOLE answer, across
// separate calls, and reaches the same hits the unpaged answer holds.
//
// Failure mode: such a process cannot record a retention lease, and a search
// that required one would serve page one and stop, so a person searching
// during an index silently sees a prefix of the ranking with no way to reach
// the rest. The continuation carries its whole position in the token, so it
// needs no lease: the read of the call that presents it retains the generation.
//
// Mutation: drop the reader.Continuable() branch in keysetNext so the lease is
// always acquired, and the read-only store refuses page one's continuation.
func legWriterlessSearchPagesInFull(t *testing.T, f *fixture) {
	ro, err := sqlite.Open(f.ctx, f.dbPath, storeOptions(true))
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	t.Cleanup(func() { ro.Close() })
	opts := f.opts
	opts.Store = ro
	opts.Leases = pagination.NewLeases(ro, pagination.DefaultCursorTTL)
	s := newService(t, opts)
	req := model.SearchRequest{Query: "handle", Page: model.PageRequest{Limit: 1}}
	first, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("a writerless process failed the query instead of answering it: %v", err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("page 1 served %d hits, want the 1 it asked for", len(first.Items))
	}
	if first.Meta.NextCursor == "" {
		t.Fatal("a writerless search stopped after one page of a longer answer")
	}
	seen := len(first.Items)
	// The SECOND call is the point: the token was minted with no lease row, and
	// the process that presents it back resumes from the position it carries.
	req.Page.Cursor = first.Meta.NextCursor
	second, err := s.Search(f.ctx, req)
	if err != nil {
		t.Fatalf("a writerless continuation failed instead of serving page two: %v", err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("page 2 served %d hits, want 1", len(second.Items))
	}
	if second.Items[0].NodeID == first.Items[0].NodeID {
		t.Fatal("page 2 served page 1's hit again")
	}
	seen += len(second.Items)
	for cursor := second.Meta.NextCursor; cursor != ""; {
		req.Page.Cursor = cursor
		page, err := s.Search(f.ctx, req)
		if err != nil {
			t.Fatalf("a writerless continuation failed: %v", err)
		}
		seen += len(page.Items)
		cursor = page.Meta.NextCursor
	}
	whole, err := s.Search(f.ctx, model.SearchRequest{Query: "handle"})
	if err != nil {
		t.Fatalf("Search(unpaged): %v", err)
	}
	if seen != len(whole.Items) {
		t.Fatalf("paging a writerless search reached %d hits, want the %d the one-shot answer has", seen, len(whole.Items))
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

// legDeadlineEndsThePage is the deadline rule for search: resources.query_timeout ends
// a PAGE, not an answer. Answering CTX_QUERY_DEADLINE with no page would leave
// the widest queries -- the only ones that ever reach the deadline --
// unanswerable rather than partially answered.
//
// The candidate search runs under a budget that has already expired here, so
// every tier stops at its first read; what was found is served as a page that
// says it is truncated and why. Nothing else in the answer may
// degrade: the binding, the completeness report and the page's own validation
// are the same as any other answer's.
//
// Mutation: return the error from collect instead of converting a deadline into
// a truncated page (drop the deadline arm of ended) and this leg fails with a
// typed deadline or cancel error where it wants a page.
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
	// What was found before the deadline is served, and a page the deadline
	// ended carries no continuation: its hits are not a prefix of the answer's
	// order, so no position after them names the rest of it.
	if got.Meta.NextCursor != "" {
		t.Fatal("a page the deadline ended minted a continuation")
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the deadline page does not validate: %v", err)
	}
}

// legUnboundedQueryAnswersInFull is the other half of the deadline rule, and the half a
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
// The capability-state fix legs.
// ---------------------------------------------------------------------------

// twoOffsetFixture is a second activated generation holding ONE file with ONE
// node that two units disagree about: the unverified unit declares it at byte
// 20, the verified one -- the Section 9.4 precedence winner -- at byte 100.
// node_facts is keyed (unit_id, node_id), so two units is the only way one file
// can carry a node at two offsets.
//
// It is built apart from the ranking corpus on purpose: fixtureDocs is the one
// corpus every other leg asserts against, and a second declaration of one of
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
	st, err := sqlite.Open(ctx, filepath.Join(dir, "codectx.db"), storeOptions(false))
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
	return &twoOffsetFixture{ctx: ctx, path: path, opts: Options{Store: st, Repo: repo, Signer: signer,
		Leases: pagination.NewLeases(st, pagination.DefaultCursorTTL), Content: cas,
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
// The continuation writes nothing: it carries its whole position in the token,
// so the only thing it ever needed a lease for was to retain the generation it
// names. The read snapshot of the
// call that presents it does that, and a generation collected in between is
// answered in the cursor's family. Requiring the lease anyway made an answer
// larger than one page unreachable for the whole length of an index -- a
// symbol query that silently stops after its first page is the failure this
// guards.
//
// Mutation: drop the reader.Continuable() branch in keysetNext so the lease is
// always acquired. The leg then fails with the read-only store's refusal.
func legWriterlessSymbolPagesInFull(t *testing.T, f *fixture) {
	ro, err := sqlite.Open(f.ctx, f.dbPath, storeOptions(true))
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

// identifierDocs is a code corpus for the identifier and paging tests: one
// identifier whose camelCase parts include a word no other text of the corpus
// spells on its own, decoys that share some of its parts, and two qualified
// names one query names exactly and by prefix.
var identifierDocs = []doc{
	{path: "pkg/pod.go", name: "Pod", qualifiedName: "pkg.Pod",
		signature: "type Pod struct", body: "type Pod struct { Spec PodSpec }", occurrences: 1},
	{path: "pkg/podctl.go", name: "PodController", qualifiedName: "pkg.PodController",
		signature: "type PodController struct", body: "type PodController struct { queue WorkQueue }", occurrences: 1},
	{path: "pkg/newpod.go", name: "NewPodController", qualifiedName: "pkg.NewPodController",
		signature: "func NewPodController() *PodController",
		body:      "func NewPodController() *PodController { return &PodController{} }", occurrences: 1},
	{path: "pkg/manager.go", name: "NewNodeManager", qualifiedName: "pkg.NewNodeManager",
		signature: "func NewNodeManager() *NodeManager", body: "func NewNodeManager() *NodeManager { return nil }", occurrences: 1},
	{path: "pkg/loop.go", name: "ControlLoop", qualifiedName: "pkg.ControlLoop",
		signature: "func ControlLoop()", body: "func ControlLoop() { for { tick() } }", occurrences: 1},
	{path: "pkg/run.go", name: "RunCtl", qualifiedName: "pkg.RunCtl",
		signature: "func RunCtl() error", body: "func RunCtl() error { return run() }", occurrences: 1},
}

// TestMidIdentifierWordFindsTheIdentifier proves the code-aware tokenizer
// reaches the search page: the word `controller` appears in this corpus only
// inside identifiers (NewPodController, PodController), so the query finds the
// identifier on page 1 only because each camelCase part is indexed at the
// identifier's position. It also proves the exact tiers keep their place ahead
// of that: a query that names one qualified name exactly and prefixes another
// serves those two first, at their tiers, and every node once.
//
// Mutation: index only the whole identifier (no camelCase parts at its
// position) and `controller` finds nothing.
func TestMidIdentifierWordFindsTheIdentifier(t *testing.T) {
	f := newFixtureOf(t, identifierDocs)
	s := newService(t, f.opts)
	page, err := s.Search(f.ctx, model.SearchRequest{Query: "controller"})
	if err != nil {
		t.Fatalf("Search(controller): %v", err)
	}
	found := false
	for _, hit := range page.Items {
		if hit.NodeID == f.nodes["pkg/newpod.go"] {
			found = true
			if hit.Tier != model.TierLexical || hit.ScoreMicros <= 0 {
				t.Errorf("NewPodController is served at %q scoring %d, want a scored lexical hit", hit.Tier, hit.ScoreMicros)
			}
		}
	}
	if !found {
		t.Fatalf("the mid-identifier word `controller` did not find NewPodController on page 1 (%d hits)", len(page.Items))
	}

	tiers, err := s.Search(f.ctx, model.SearchRequest{Query: "pkg.Pod"})
	if err != nil {
		t.Fatalf("Search(pkg.Pod): %v", err)
	}
	if len(tiers.Items) < 3 {
		t.Fatalf("pkg.Pod served %d hits, want the two exact-tier hits and lexical hits after them", len(tiers.Items))
	}
	if h := tiers.Items[0]; h.Tier != model.TierExactQualifiedName || h.NodeID != f.nodes["pkg/pod.go"] {
		t.Errorf("hit 0 is %q at %q, want Pod at %q", h.Name, h.Tier, model.TierExactQualifiedName)
	}
	if h := tiers.Items[1]; h.Tier != model.TierQualifiedNamePrefix || h.NodeID != f.nodes["pkg/podctl.go"] {
		t.Errorf("hit 1 is %q at %q, want PodController at %q", h.Name, h.Tier, model.TierQualifiedNamePrefix)
	}
	seen := map[model.NodeID]int{}
	for i, h := range tiers.Items {
		seen[h.NodeID]++
		if i >= 2 && h.Tier != model.TierLexical {
			t.Errorf("hit %d is at %q, want every hit after the two exact ones lexical", i, h.Tier)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("node %s was served %d times, want once", id, n)
		}
	}
}

// TestSearchPagingWalksTheWholeAnswerOnce proves a paged walk is the deep
// page: walking every page by cursor -- one hit at a time, and in pages that
// end inside the exact tiers and inside the lexical tier -- serves each result
// once and in the order one page of the whole answer serves it. The query's
// answer crosses from the exact tiers into the lexical tier, where the results
// the exact tiers served are dropped from the lexical sequence. A filter change
// refuses the cursor.
//
// Mutation: slice the lexical page without the exact fold and the exact hits
// reappear in the lexical sequence, served twice.
func TestSearchPagingWalksTheWholeAnswerOnce(t *testing.T) {
	f := newFixtureOf(t, identifierDocs)
	s := newService(t, f.opts)
	const query = "pkg.Pod"
	whole, err := s.Search(f.ctx, model.SearchRequest{Query: query})
	if err != nil {
		t.Fatalf("Search(one page): %v", err)
	}
	if whole.Meta.NextCursor != "" {
		t.Fatal("the whole answer does not fit one page; the walk below compares against it")
	}
	if len(whole.Items) != len(identifierDocs) {
		t.Fatalf("the whole answer holds %d hits, want each of the %d nodes once", len(whole.Items), len(identifierDocs))
	}
	for _, limit := range []int{1, 3} {
		req := model.SearchRequest{Query: query, Page: model.PageRequest{Limit: limit}}
		var walked []model.SearchHit
		for page := 0; ; page++ {
			if page > len(whole.Items) {
				t.Fatalf("limit %d: the walk did not end after %d pages", limit, page)
			}
			got, err := s.Search(f.ctx, req)
			if err != nil {
				t.Fatalf("limit %d: Search(page %d): %v", limit, page+1, err)
			}
			walked = append(walked, got.Items...)
			if got.Meta.NextCursor == "" {
				break
			}
			if len(got.Items) != limit {
				t.Fatalf("limit %d: page %d served %d hits and a continuation", limit, page+1, len(got.Items))
			}
			req.Page.Cursor = got.Meta.NextCursor
		}
		if !reflect.DeepEqual(walked, whole.Items) {
			for i := range min(len(walked), len(whole.Items)) {
				if !reflect.DeepEqual(walked[i], whole.Items[i]) {
					t.Fatalf("limit %d: walked hit %d = %+v, want %+v", limit, i, walked[i], whole.Items[i])
				}
			}
			t.Fatalf("limit %d: the walk served %d hits, the whole answer %d", limit, len(walked), len(whole.Items))
		}
	}

	first, err := s.Search(f.ctx, model.SearchRequest{Query: query, Page: model.PageRequest{Limit: 1}})
	if err != nil {
		t.Fatalf("Search(page 1): %v", err)
	}
	for _, changed := range []model.SearchRequest{
		{Query: query, Kinds: []model.NodeKind{model.NodeFunction}},
		{Query: query, Paths: []string{"pkg"}},
		{Query: "pkg.Po"},
	} {
		changed.Page = model.PageRequest{Limit: 1, Cursor: first.Meta.NextCursor}
		_, err := s.Search(f.ctx, changed)
		assertCode(t, "a continuation under a changed request", err, model.CodeCursorInvalid)
	}
}
