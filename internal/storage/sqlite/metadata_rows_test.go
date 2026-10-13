package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/graph/graphtest"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestMetadataRowsReadBackInSourceOrderAcrossPages writes, through the batch
// sink over the real unit writer, a function whose 450 decorators and an open
// import site whose 900 fragments both pass the node metadata bound, so each
// is stored as metadata rows on its node, and a second site whose one
// fragment is a 5 MiB string literal -- longer than the whole storage batch
// (4 MiB) -- and reads them back through the store's graph reader. Each list
// spans several evidence pages (model.MaxPageItems rows a page), and every
// element must come back once, whole, in source order, with no row of another
// field mixed in.
//
// Mutations it catches: MetadataRows stopping after the first evidence page
// (200 of 450 rows), serving rows in evidence-id order rather than by
// ordinal, not filtering on the field, or the writer dropping a row's detail;
// for the long fragment, storing it as one row (the batch is refused and the
// unit fails), the sink handing one fact past the batch on whole (refused
// again), or the reader serving its pieces instead of the element.
func TestMetadataRowsReadBackInSourceOrderAcrossPages(t *testing.T) {
	ctx := context.Background()
	repo := graphtest.FixtureRepository
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "codectx.db"), store.WithDerivedReaders(store.Options{}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureRepository(ctx, repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	const src = "package a\n\nfunc Handler() {}\n"
	ff := putFixtureFile(t, s, repo, graphtest.FixtureFilePath, src)
	snap := putFixtureSnapshot(t, s, repo, ff, len(src))
	gen, err := s.BeginGeneration(ctx, repo, snap.ID, model.H("metadata-rows"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run, err := s.BeginProviderRun(ctx, gen, providerID, providerVersion)
	if err != nil {
		t.Fatalf("BeginProviderRun: %v", err)
	}
	w := beginFixtureUnit(t, s, gen, run, ff, "rows")

	var decorators []model.Decorator
	for i := range 450 {
		decorators = append(decorators, model.Decorator{Name: fmt.Sprintf("registered_decorator_%04d", i),
			Bytes: model.ByteRange{Start: 0, End: 9}})
	}
	var fragments []string
	for i := range 900 {
		fragments = append(fragments, fmt.Sprintf("part%04d/", i))
	}
	// The literal mixes quotes, backslashes and a two-byte rune so the piece
	// cuts land inside escapes and inside the rune's encoding.
	literal := strings.Repeat(`a"b\\é`, (5<<20)/7)
	located := &model.SourceRange{Start: model.Position{Byte: 11, Line: 1}, End: model.Position{Byte: 29, Line: 1}}
	type subject struct {
		id    model.NodeID
		key   string
		node  model.Node
		field string
		want  any
	}
	fnKey, siteKey := graphtest.NodeKey("pkg/a#Handler"), graphtest.NodeKey("pkg/a#openimport")
	subjects := []subject{
		{key: fnKey, field: model.MetaDecorators, want: decorators,
			node: model.Node{Kind: model.NodeFunction, Name: "Handler"}},
		{key: siteKey, field: model.MetaFragments, want: fragments,
			node: model.Node{Kind: model.NodeModule, Name: "import"}},
		{key: graphtest.NodeKey("pkg/a#bigimport"), field: model.MetaFragments, want: []string{"head/", literal, "tail"},
			node: model.Node{Kind: model.NodeModule, Name: "bigimport"}},
	}
	var facts []model.NodeFact
	for i := range subjects {
		sub := &subjects[i]
		sub.id = model.NewNodeID(repo, sub.node.Kind, sub.key)
		n := sub.node
		n.ID, n.FileID, n.ContentHash, n.Range = sub.id, ff.id, ff.hash, located
		meta, rows, err := model.SplitMetadata(map[string]any{"exported": true, sub.field: sub.want})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatalf("%s fits the metadata bound inline; the fixture does not exercise rows", sub.field)
		}
		n.Metadata = meta
		evidence := func(detail string) model.Evidence {
			ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
				OriginRunID: run, NodeID: n.ID, Precision: model.PrecisionSyntax, FileID: ff.id,
				ContentHash: ff.hash, Detail: detail}
			ev.ID = model.NewEvidenceID(ev)
			return ev
		}
		evs := []model.Evidence{evidence("")}
		for _, r := range rows {
			details, err := r.Details()
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range details {
				evs = append(evs, evidence(d))
			}
		}
		facts = append(facts, model.NodeFact{Node: n, CanonicalKey: sub.key, Evidence: evs})
	}
	limits := provider.Limits{BatchRecords: 1000, BatchBytes: 4 << 20}
	pool, err := provider.NewPool(4 * limits.BatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sink, err := provider.NewBatchSink(runCtx, w, limits, pool, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Discard()
	if err := sink.PutNodes(ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.SealUnit(ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	if _, err := s.Activate(ctx, gen, 0, model.HealthFresh, fixtureCaps, "norm-v1"); err != nil {
		t.Fatalf("Activate: %v", err)
	}

	g := openGraphReader(t, s, repo)
	rr, ok := g.(graph.MetadataRowReader)
	if !ok {
		t.Fatal("the store's graph reader does not read metadata rows")
	}
	for _, sub := range subjects {
		rows, err := rr.MetadataRows(ctx, sub.id, sub.field)
		if err != nil {
			t.Fatalf("MetadataRows(%s): %v", sub.field, err)
		}
		if !slices.IsSortedFunc(rows, func(a, b model.MetadataRow) int { return int(a.Ordinal - b.Ordinal) }) {
			t.Errorf("%s rows are not in ordinal order", sub.field)
		}
		for _, r := range rows {
			if r.Parts != 0 {
				t.Fatalf("%s row %d is served as piece %d of %d, want the whole element", sub.field, r.Ordinal, r.Part, r.Parts)
			}
		}
		got, err := model.JoinMetadataRows(rows)
		if err != nil {
			t.Fatalf("join %d %s rows: %v", len(rows), sub.field, err)
		}
		want, err := model.EncodeMetadataValue(sub.want)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s: %d rows rebuild %d bytes, want every element in source order (%d bytes)",
				sub.field, len(rows), len(got), len(want))
		}
	}
}
