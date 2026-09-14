package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
	_ "modernc.org/sqlite"
)

// The store satisfies the narrow lease contract pagination declares. Neither
// production package imports the other; this assertion is the only place the
// two meet, so a drift in either signature fails compilation here.
var _ pagination.LeaseStore = (*store.Store)(nil)

// fixture is the smallest repository that can exercise unit reuse: two files,
// one unit per file, one node with evidence and one search document per unit.
type fixture struct {
	t     *testing.T
	ctx   context.Context
	s     *store.Store
	repo  model.RepositoryID
	files map[string]fileFixture
}

type fileFixture struct {
	path    string
	id      model.FileID
	content []byte
	hash    string
}

const (
	providerID      = "treesitter"
	providerVersion = "1.0.0"
	configHash      = "cfg"
	actorID         = "agent-1"
)

func newFixture(t *testing.T, dbPath string) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fixture{t: t, ctx: ctx, s: s, repo: model.RepositoryID(model.H("test-repo", "1")), files: map[string]fileFixture{}}
	if err := s.EnsureRepository(ctx, f.repo, "/repo"); err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	return f
}

func (f *fixture) file(path string, content string) fileFixture {
	f.t.Helper()
	sum := sha256.Sum256([]byte(content))
	ff := fileFixture{path: path, id: model.NewFileID(f.repo, path), content: []byte(content), hash: hex.EncodeToString(sum[:])}
	rec := model.BlobRecord{Hash: ff.hash, Size: int64(len(content))}
	for off := 0; off < len(content); off += model.BlobBlockBytes {
		end := min(off+model.BlobBlockBytes, len(content))
		d := sha256.Sum256([]byte(content[off:end]))
		rec.BlockDigests = append(rec.BlockDigests, hex.EncodeToString(d[:]))
	}
	rec.LineCheckpoints = []model.LineCheckpoint{{ByteOffset: 0, LineNumber: 1, LineStartByte: 0}}
	if err := f.s.PutBlob(f.ctx, rec); err != nil {
		f.t.Fatalf("PutBlob(%s): %v", path, err)
	}
	f.files[path] = ff
	return ff
}

func (f *fixture) snapshot(tag string, files ...fileFixture) model.Snapshot {
	f.t.Helper()
	var bytes uint64
	for _, ff := range files {
		bytes += uint64(len(ff.content))
	}
	manifest := model.H("manifest-test", tag)
	snap := model.Snapshot{
		ID:                 model.NewSnapshotID(f.repo, "", model.H("policy"), manifest),
		RepositoryID:       f.repo,
		CaptureConsistency: model.CaptureValidated,
		SourcePolicyHash:   model.H("policy"),
		FileCount:          uint64(len(files)),
		SourceBytes:        bytes,
		ManifestHash:       manifest,
		CreatedAt:          time.Now().UTC(),
	}
	err := f.s.PutSnapshot(f.ctx, snap, func(yield func(model.FileVersion) error) error {
		for _, ff := range files {
			fv := model.FileVersion{ID: ff.id, Path: ff.path, Status: model.FileTracked, Size: int64(len(ff.content)), ContentHash: ff.hash, Language: "go"}
			if err := yield(fv); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("PutSnapshot(%s): %v", tag, err)
	}
	return snap
}

// providerTokenCount is the deliberately wrong token count every fixture
// document asserts; storage must compute its own from the indexed text.
const providerTokenCount = 3

// searchDoc is the lexical document fixture.unit publishes for ff.
func (f *fixture) searchDoc(ff fileFixture) model.SearchUnit {
	return model.SearchUnit{ID: model.H("search-test", ff.path, ff.hash), NodeID: f.nodeID(ff), FileID: ff.id, Path: ff.path,
		Kind: model.NodeFunction, Name: "F", QualifiedName: ff.path + ".F",
		Bytes: model.ByteRange{Start: 0, End: uint64(len(ff.content))}, Body: string(ff.content), TokenCount: providerTokenCount}
}

// begin opens a file-scoped unit for ff inside gen without sealing it.
func (f *fixture) begin(gen model.GenerationID, run model.ProviderRunID, ff fileFixture, deps ...model.UnitID) *store.UnitWriter {
	f.t.Helper()
	input := model.UnitInput{FileID: ff.id, ContentHash: ff.hash}
	h := model.NewUnitInputHasher()
	if err := h.Add(input); err != nil {
		f.t.Fatal(err)
	}
	spec := model.UnitSpec{ProviderID: providerID, ProviderVersion: providerVersion, ScopeKey: ff.path,
		InputHash: h.Sum(), DependencyHash: model.DependencyHash(deps)}
	spec.ID = model.NewUnitID(spec, configHash)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: configHash, OriginRunID: run,
		SourceBinding: model.SourceBindingVerified, Dependencies: deps}
	w, err := f.s.BeginUnit(f.ctx, gen, build, func(yield func(model.UnitInput) error) error { return yield(input) })
	if err != nil {
		f.t.Fatalf("BeginUnit(%s): %v", ff.path, err)
	}
	return w
}

// unit builds, fills and seals one file-scoped unit for ff inside gen. The
// node's canonical key and the search document are derived from the path so
// the same file content yields the same unit across snapshots.
func (f *fixture) unit(gen model.GenerationID, run model.ProviderRunID, ff fileFixture, deps ...model.UnitID) model.UnitID {
	f.t.Helper()
	w := f.begin(gen, run, ff, deps...)
	f.fill(w, run, ff)
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", ff.path, err)
	}
	return w.UnitID()
}

// fill writes one node fact, its alias and its search document into a building
// unit over ff.
func (f *fixture) fill(w *store.UnitWriter, run model.ProviderRunID, ff fileFixture) {
	f.t.Helper()
	key := model.CanonicalNodeKey(ff.path, "F")
	nodeID := model.NewNodeID(f.repo, model.NodeFunction, key)
	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(len(ff.content)), Line: 1, Column: uint32(len(ff.content))}}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
		NodeID: nodeID, Precision: model.PrecisionSyntax, FileID: ff.id, ContentHash: ff.hash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: "F", QualifiedName: ff.path + ".F",
		FileID: ff.id, ContentHash: ff.hash, Range: rng}
	// A fact whose ID does not derive from (repository, kind, canonical key) is
	// malformed provider output: accepting it would let two keys share one
	// identity row.
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node, CanonicalKey: "other-key", Evidence: []model.Evidence{ev}}}); err == nil {
		f.t.Fatal("PutNodes accepted a node whose ID does not derive from its canonical key")
	} else {
		wantCode(f.t, err, model.CodeProviderOutputInvalid)
	}
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node, CanonicalKey: key, Evidence: []model.Evidence{ev}}}); err != nil {
		f.t.Fatalf("PutNodes: %v", err)
	}
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: ff.path, NativeKey: "F", NodeID: nodeID}}); err != nil {
		f.t.Fatalf("PutAliases: %v", err)
	}
	if err := w.PutSearchUnits(f.ctx, []model.SearchUnit{f.searchDoc(ff)}); err != nil {
		f.t.Fatalf("PutSearchUnits: %v", err)
	}
}

func (f *fixture) nodeID(ff fileFixture) model.NodeID {
	return model.NewNodeID(f.repo, model.NodeFunction, model.CanonicalNodeKey(ff.path, "F"))
}

func (f *fixture) run(gen model.GenerationID) model.ProviderRunID {
	f.t.Helper()
	run, err := f.s.BeginProviderRun(f.ctx, gen, providerID, providerVersion)
	if err != nil {
		f.t.Fatalf("BeginProviderRun: %v", err)
	}
	return run
}

var fixtureCaps = []model.CapabilityState{{ProviderID: providerID, Capability: "structure", Scope: "workspace", State: model.CapabilityFresh}}

// activate publishes gen, asserting that expected is the active generation at
// the moment the pointer flips.
func (f *fixture) activate(gen, expected model.GenerationID) model.Binding {
	f.t.Helper()
	b, err := f.s.Activate(f.ctx, gen, expected, model.HealthFresh, fixtureCaps, "norm-v1")
	if err != nil {
		f.t.Fatalf("Activate(%d): %v", gen, err)
	}
	return b
}

func (f *fixture) stats() store.Stats {
	f.t.Helper()
	st, err := f.s.Stats(f.ctx)
	if err != nil {
		f.t.Fatalf("Stats: %v", err)
	}
	return st
}

// indexedTokens counts the tokens storage's own tokenizer yields for every
// column search_fts indexes, which is what token_count must equal.
func (f *fixture) indexedTokens(doc model.SearchUnit) int64 {
	f.t.Helper()
	var n int64
	for _, col := range []string{doc.Name, doc.QualifiedName, doc.Signature, doc.Path, doc.Body} {
		terms, err := f.s.Tokenize(f.ctx, col)
		if err != nil {
			f.t.Fatalf("Tokenize: %v", err)
		}
		n += int64(len(terms))
	}
	return n
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("got %v (%T), want *model.Error with code %s", err, err, code)
	}
	if typed.Code != code {
		t.Fatalf("got code %s (%s), want %s", typed.Code, typed.Message, code)
	}
}

// TestStorePublicationScenario is the one storage integration scenario of
// Section 25.1 ("storage and incremental publication"). Each phase names the
// silent failure it protects against: leaking unsealed facts, rewriting or
// wrongly reusing units, a half-applied activation, lost provenance, a fact
// bound to the wrong snapshot, an FTS index that disagrees with its content,
// GC collecting a pinned generation, and serving from a schema this binary
// does not understand.
func TestStorePublicationScenario(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "codectx.db")
	f := newFixture(t, dbPath)
	ctx := f.ctx

	a := f.file("pkg/a.go", "package pkg\nfunc A() {}\n")
	b1 := f.file("pkg/b.go", "package pkg\nfunc B() {}\n")
	// c.go exists only in the first snapshot, so its files row becomes
	// unreferenced once that snapshot is collected.
	c := f.file("pkg/c.go", "package pkg\n")
	snap1 := f.snapshot("one", a, b1, c)

	// Generation 1: build both units and publish.
	gen1, err := f.s.BeginGeneration(ctx, f.repo, snap1.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run1 := f.run(gen1)
	unitA := f.unit(gen1, run1, a)
	unitB1 := f.unit(gen1, run1, b1)
	if err := f.s.CompleteProviderRun(ctx, model.ProviderResult{RunID: run1, State: model.RunSucceeded, RecordsEmitted: 2, BytesProcessed: 40}, ""); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}

	// No active generation yet: a query must fail closed rather than read
	// staging facts.
	if _, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute); err == nil {
		t.Fatal("PinGeneration succeeded with only a staging generation; staging facts leaked")
	} else {
		wantCode(t, err, model.CodeNoActiveGeneration)
	}
	// Activation is a compare-and-swap on the pointer: a coordinator that
	// believes another generation is active must not publish over it.
	if _, err := f.s.Activate(ctx, gen1, 99, model.HealthFresh, fixtureCaps, "norm-v1"); err == nil {
		t.Fatal("Activate published with a wrong expected active generation")
	} else {
		wantCode(t, err, model.CodeVersionConflict)
	}
	bind1 := f.activate(gen1, 0)
	if bind1.GenerationID != gen1 || bind1.AnalysisKey == "" {
		t.Fatalf("binding after activation = %+v", bind1)
	}
	after1 := f.stats()

	// Phase: staging invisibility. Pin gen1, start gen2 over a changed b.go,
	// build its unit, and confirm the pinned reader never sees the new node
	// while the active pointer still resolves to gen1.
	b2 := f.file("pkg/b.go", "package pkg\nfunc B() { changed() }\n")
	// empty.go carries no bytes at all. It is in the snapshot, and later in a
	// manifest, so the coverage phase below can prove that a zero-byte required
	// file is not fully served by virtue of having nothing to read.
	empty := f.file("pkg/empty.go", "")
	snap2 := f.snapshot("two", a, b2, empty)
	reader1, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	gen2, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(2): %v", err)
	}
	// Phase: changed-input rejection. b1's unit was built over the old bytes;
	// attaching it to a generation whose snapshot has different bytes for that
	// file must fail.
	if err := f.s.AttachUnit(ctx, gen2, unitB1); err == nil {
		t.Fatal("AttachUnit accepted a unit whose input bytes changed; stale facts would be served as fresh")
	} else {
		wantCode(t, err, model.CodeSnapshotChanged)
	}
	run2 := f.run(gen2)
	unitB2 := f.unit(gen2, run2, b2)
	// The node ID is path-derived so gen2's staging fact shares it with gen1's;
	// the pinned reader must return the gen1 attributes, whose range covers
	// b1's shorter content, never the staging unit's.
	n, err := reader1.Node(ctx, f.nodeID(b1))
	if err != nil {
		t.Fatalf("pinned reader lost its own generation's node: %v", err)
	}
	if n.Bytes == nil || n.Bytes.End != uint64(len(b1.content)) || n.UnitID != unitB1 {
		t.Fatalf("pinned reader returned staging attributes %+v", n)
	}
	if got, _ := f.s.ActiveGeneration(ctx, f.repo); got != gen1 {
		t.Fatalf("active generation = %d during staging, want %d", got, gen1)
	}
	// Two exact predicates on one filter are contradictory; the reader must
	// refuse rather than silently apply one of them.
	if _, err := reader1.Nodes(ctx, store.NodeFilter{Name: "F", QualifiedName: "pkg/a.go.F"}, "", 10); err == nil {
		t.Fatal("Nodes accepted a filter with both name and qualified_name")
	} else {
		wantCode(t, err, model.CodeArgumentInvalid)
	}

	// Phase: unchanged reuse. Attaching unitA writes only a membership row: no
	// new node, evidence, alias or search rows.
	if err := f.s.AttachUnit(ctx, gen2, unitA); err != nil {
		t.Fatalf("AttachUnit(unitA): %v", err)
	}
	before := f.stats()
	if before.NodeFacts != after1.NodeFacts+1 || before.Evidence != after1.Evidence+1 || before.SearchUnits != after1.SearchUnits+1 {
		t.Fatalf("stats after building one new unit = %+v, want exactly one more fact/evidence/search row than %+v", before, after1)
	}

	// Phase: failed activation rolls back. A dependency that is not a member
	// makes validation fail after the status update was issued; both the
	// pointer and the status must be untouched afterwards.
	gen3, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(3): %v", err)
	}
	run3 := f.run(gen3)
	depUnit := f.unit(gen3, run3, a, unitA)
	// gen3 has depUnit but not its dependency unitA as a member.
	if _, err := f.s.Activate(ctx, gen3, gen1, model.HealthFresh, nil, "norm-v1"); err == nil {
		t.Fatal("Activate published a generation whose member depends on a non-member unit")
	}
	if got, _ := f.s.ActiveGeneration(ctx, f.repo); got != gen1 {
		t.Fatalf("active pointer moved to %d after a failed activation", got)
	}
	if st, err := f.s.GenerationStatus(ctx, gen3); err != nil || st != model.GenerationStaging {
		t.Fatalf("gen3 status after failed activation = %v %v, want staging", st, err)
	}
	if err := f.s.Abort(ctx, gen3); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Abort deletes only unsealed output; the sealed dependent unit keeps its
	// provenance and stays reusable.
	if origin, err := f.s.UnitOrigin(ctx, depUnit); err != nil || origin != run3 {
		t.Fatalf("sealed unit lost after Abort: %v %v", origin, err)
	}

	// Publish gen2; gen1 is superseded but still pinned by reader1.
	bind2 := f.activate(gen2, gen1)
	if bind2.AnalysisKey == bind1.AnalysisKey {
		t.Fatal("analysis key did not change although unit membership changed")
	}
	if st, _ := f.s.GenerationStatus(ctx, gen1); st != model.GenerationSuperseded {
		t.Fatalf("gen1 status = %s, want superseded", st)
	}
	// The pinned reader still serves gen1 and its lease blocks collection.
	if _, err := reader1.Node(ctx, f.nodeID(a)); err != nil {
		t.Fatalf("pinned reader lost access to its generation: %v", err)
	}

	// Phase: pin/GC race. Readers pin and read gen2 while a collector retries
	// DeleteGeneration(gen1) concurrently. Every attempt made before reader1
	// releases its lease must be refused as busy; the first attempt after the
	// release succeeds and no later attempt may run against a half-deleted row.
	var wg sync.WaitGroup
	var released atomic.Bool
	raceErr := make(chan error, 64)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				r, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
				if err != nil {
					raceErr <- err
					return
				}
				if _, err := r.Node(ctx, f.nodeID(a)); err != nil {
					raceErr <- err
				}
				if err := r.Close(); err != nil {
					raceErr <- err
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for attempt := 0; attempt < 10_000; attempt++ {
			err := f.s.DeleteGeneration(ctx, gen1)
			if err == nil {
				if !released.Load() {
					raceErr <- errors.New("DeleteGeneration succeeded while reader1 still held its lease")
				}
				return
			}
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy || !typed.Retryable {
				raceErr <- err
				return
			}
			if attempt == 20 {
				// Release only once the collector has been refused repeatedly,
				// so the busy path is exercised under real contention.
				released.Store(true)
				if err := reader1.Close(); err != nil {
					raceErr <- err
					return
				}
			}
		}
		raceErr <- errors.New("DeleteGeneration never succeeded after the lease was released")
	}()
	wg.Wait()
	close(raceErr)
	for err := range raceErr {
		t.Fatalf("pin/GC race: %v", err)
	}
	if _, err := f.s.GenerationStatus(ctx, gen1); err == nil {
		t.Fatal("gen1 still exists after DeleteGeneration succeeded")
	}

	// Phase: provenance retained. unitA still carries run1 as its origin even
	// though gen1, which created it, is gone; unitB1 was unreachable and is
	// collected together with its FTS rows; snap1 and c.go's file row, now
	// referenced by nothing, are collected too.
	if run, err := f.s.ProviderRun(ctx, run1); err != nil {
		t.Fatalf("run1 provenance lost after deleting its generation: %v", err)
	} else if run.GenerationID != 0 {
		t.Fatalf("run1 still names deleted generation %d", run.GenerationID)
	}
	if origin, err := f.s.UnitOrigin(ctx, unitA); err != nil || origin != run1 {
		t.Fatalf("unitA origin = %v %v, want %v", origin, err, run1)
	}
	if _, err := f.s.UnitOrigin(ctx, unitB1); err == nil {
		t.Fatal("unreachable unitB1 survived generation deletion")
	}
	if err := f.s.Check(ctx, true); err != nil {
		t.Fatalf("FTS/database integrity after FTS-aware deletion: %v", err)
	}
	afterGC := f.stats()
	if afterGC.Snapshots != 1 || afterGC.Files != 3 {
		t.Fatalf("after collecting gen1: %d snapshots %d files, want 1 snapshot and 3 files (c.go collected, snap2's a.go/b.go/empty.go retained)", afterGC.Snapshots, afterGC.Files)
	}
	reader2, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(gen2): %v", err)
	}
	if n, err := reader2.Node(ctx, f.nodeID(b2)); err != nil || n.UnitID != unitB2 {
		t.Fatalf("active reader node = %+v %v, want gen2's unit %s", n, err, unitB2)
	}
	if ids, err := reader2.Match(ctx, `"changed"`, 0, 10); err != nil || len(ids) != 1 {
		t.Fatalf("Match(changed) = %v %v, want exactly the gen2 b.go document", ids, err)
	}
	ids, err := reader2.Match(ctx, `"B"`, 0, 10)
	if err != nil || len(ids) != 1 {
		t.Fatalf("Match(B) = %v %v, want only the retained document; the deleted unit's FTS row must be gone", ids, err)
	}
	// Phase: length statistics agree with the index. token_count is computed
	// by storage with the index tokenizer; the provider's value is ignored.
	docB, err := reader2.SearchUnit(ctx, ids[0])
	if err != nil {
		t.Fatalf("SearchUnit: %v", err)
	}
	wantB := f.indexedTokens(f.searchDoc(b2))
	if docB.TokenCount != wantB || docB.TokenCount == providerTokenCount {
		t.Fatalf("stored token_count = %d, want %d indexed tokens (provider asserted %d)", docB.TokenCount, wantB, providerTokenCount)
	}
	docs, tokens, err := reader2.SearchStats(ctx)
	if wantTokens := wantB + f.indexedTokens(f.searchDoc(a)); err != nil || docs != 2 || tokens != wantTokens {
		t.Fatalf("SearchStats = %d docs %d tokens %v, want 2 docs %d tokens", docs, tokens, err, wantTokens)
	}
	if terms, err := f.s.Tokenize(ctx, "Foo_bar baz.Qux"); err != nil || len(terms) != 4 {
		t.Fatalf("Tokenize = %v %v, want 4 unicode61 tokens", terms, err)
	}

	// Phase: manifests and sessions. A manifest must carry the generation's
	// analysis key, and reading it back must reproduce the header written.
	manifest := model.ContextManifest{ID: model.ManifestID(model.H("manifest", "m1")), Binding: bind2, Phase: model.PhaseSweep,
		RequestHash: model.H("req"), PolicyVersion: "p1", CanonicalHash: model.H("canon"), EntryCount: 1, SliceCount: 1,
		Budget: model.Budget{MaxEstimatedTokens: 1000, MaxBytes: 4096, MaxFiles: 10, MaxSlices: 2}, Completeness: fixtureCaps, ScopeComplete: true,
		EstimateMethod: "test-estimator", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	manifest.EntryCount = 2
	entries := []model.ContextEntry{
		{Ordinal: 0, FileID: b2.id, Requirement: model.RequirementFull, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10, Reasons: []string{"seed"}},
		{Ordinal: 1, FileID: empty.id, Requirement: model.RequirementFull, EstimatedBytes: 0, EstimatedTokens: 0, Reasons: []string{"seed"}},
	}
	slices := []model.ContextSlice{{Index: 0, EntryOrdinals: []int{0, 1}, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10}}
	wrongKey := manifest
	wrongKey.Binding.AnalysisKey = model.AnalysisKey(model.H("not", "this"))
	if err := f.s.PutManifest(ctx, wrongKey, []byte(`{"task":"t"}`), entries, slices, nil); err == nil {
		t.Fatal("PutManifest accepted a binding whose analysis key is not the generation's")
	}
	if err := f.s.PutManifest(ctx, manifest, []byte(`{"task":"t"}`), entries, slices, nil); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if got, err := f.s.Manifest(ctx, manifest.ID); err != nil {
		t.Fatalf("Manifest: %v", err)
	} else if got.EstimateMethod != manifest.EstimateMethod || got.Budget != manifest.Budget || !got.ScopeComplete || len(got.Completeness) != 1 {
		t.Fatalf("manifest read back = %+v, want the header that was written %+v", got, manifest)
	}
	open := model.SessionOpen{ID: model.SessionID(model.H("session", "1")), ActorID: actorID, OpenRequestHash: model.H("open"),
		ManifestID: manifest.ID, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if _, err := f.s.OpenSession(ctx, open); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	// Phase: a session's retention lease lives and dies with the session.
	//
	// The failure mode is silent and unbounded: OpenSession is idempotent, so a
	// retried plan returns the session that already exists, and an acquire that
	// minted a second lease each time would pin the generation under lease ids
	// nobody holds -- every retry adding one, none of them ever released. The
	// other half is the close: a session that ends must stop pinning, or a
	// generation no reader can reach still refuses collection for the rest of
	// the lease TTL.
	retried := model.SessionOpen{ID: model.SessionID(model.H("session", "retried")), ActorID: actorID,
		OpenRequestHash: model.H("open"), ManifestID: manifest.ID, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if _, err := f.s.OpenSession(ctx, retried); err != nil {
		t.Fatalf("OpenSession(retried): %v", err)
	}
	beforeLease := f.stats().Leases
	sessionLease := func(tag string) model.Lease {
		return model.Lease{ID: model.H("lease", tag), GenerationID: manifest.Binding.GenerationID,
			OwnerKind: model.LeaseSession, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	}
	if err := f.s.AcquireLease(ctx, sessionLease("open"), string(retried.ID)); err != nil {
		t.Fatalf("AcquireLease(session): %v", err)
	}
	err = f.s.AcquireLease(ctx, sessionLease("reopen"), string(retried.ID))
	if !store.OwnerLeaseConflict(err) {
		t.Fatalf("a second lease for one session was accepted (err = %v); an idempotent re-open must leave the one lease it already holds", err)
	}
	if live := f.stats().Leases; live != beforeLease+1 {
		t.Fatalf("re-opening the session left %d leases, want %d; a retried open must not pin the generation twice", live, beforeLease+1)
	}
	if _, err := f.s.AdvanceSession(ctx, model.AdvanceRequest{SessionID: retried.ID, ActorID: actorID,
		Target: model.StateClosed, ExpectedVersion: 1}); err != nil {
		t.Fatalf("AdvanceSession(closed): %v", err)
	}
	if live := f.stats().Leases; live != beforeLease {
		t.Fatalf("closing the session left %d leases, want %d; a closed session must stop pinning its generation", live, beforeLease)
	}

	// Phase: cross-snapshot FK. A session pinned to snap2 cannot record a
	// chunk issued for snap1's bytes of b.go, and no other actor can issue
	// chunks into this session at all.
	stale := model.IssuedChunk{ID: model.H("chunk", "1"), SessionID: open.ID, ActorID: actorID, FileID: b1.id, ContentHash: b1.hash,
		Bytes: model.ByteRange{Start: 0, End: 8}, ExpiresAt: time.Now().Add(time.Minute).UTC()}
	if err := f.s.IssueChunk(ctx, stale); err == nil {
		t.Fatal("IssueChunk accepted a chunk for another snapshot's bytes of a session file")
	}
	fresh := stale
	fresh.ID = model.H("chunk", "2")
	fresh.ContentHash = b2.hash
	other := fresh
	other.ActorID = "someone-else"
	if err := f.s.IssueChunk(ctx, other); err == nil {
		t.Fatal("IssueChunk accepted another actor's chunk into this session")
	} else {
		wantCode(t, err, model.CodeActorMismatch)
	}
	if err := f.s.IssueChunk(ctx, fresh); err != nil {
		t.Fatalf("IssueChunk(current snapshot): %v", err)
	}
	// Phase: the coverage summary Status reads instead of paging every file.
	// The silent failure it protects against is a summary that disagrees with
	// Coverage's own state switch and so reports a session read-complete when
	// it is not. The sharp case is the empty required file: it has no bytes, so
	// no served range can ever exist for it, and a summary that derived
	// "served == size" arithmetically would count it fully served the instant
	// the session opened. It counts only once its zero-length EOF chunk is
	// confirmed.
	summary := func(step string) store.CoverageCounts {
		t.Helper()
		c, err := f.s.CoverageSummary(ctx, open.ID, actorID)
		if err != nil {
			t.Fatalf("CoverageSummary(%s): %v", step, err)
		}
		return c
	}
	eof := model.IssuedChunk{ID: model.H("chunk", "eof"), SessionID: open.ID, ActorID: actorID, FileID: empty.id, ContentHash: empty.hash,
		Bytes: model.ByteRange{Start: 0, End: 0}, ExpiresAt: time.Now().Add(time.Minute).UTC()}
	if err := f.s.IssueChunk(ctx, eof); err != nil {
		t.Fatalf("IssueChunk(zero-length EOF chunk over an empty file): %v", err)
	}
	if c := summary("issued but unconfirmed"); c.Required != 2 || c.FullyRead != 0 || c.Served != 0 || c.Waived != 0 {
		t.Fatalf("summary with the EOF chunk issued but unconfirmed = %+v, want 2 required / 0 fully read / 0 served / 0 waived; an unconfirmed chunk is not coverage", c)
	}
	if err := f.s.ConfirmChunks(ctx, open.ID, actorID, []string{eof.ID}); err != nil {
		t.Fatalf("ConfirmChunks(EOF): %v", err)
	}
	if c := summary("EOF confirmed"); c.Required != 2 || c.FullyRead != 1 || c.Served != 1 || c.Waived != 0 {
		t.Fatalf("summary after confirming the EOF chunk = %+v, want 2 required / 1 fully read / 1 served / 0 waived; an empty file is fully served only on a confirmed zero-length receipt", c)
	}
	// The other branch of the same switch: a nonempty file counts only when the
	// merged served union is exactly its size, so the partial prefix [0,8) must
	// not count and the adjacent remainder must complete it.
	if err := f.s.ConfirmChunks(ctx, open.ID, actorID, []string{fresh.ID}); err != nil {
		t.Fatalf("ConfirmChunks(prefix): %v", err)
	}
	if c := summary("partial prefix"); c.Required != 2 || c.FullyRead != 1 || c.Served != 1 || c.Waived != 0 {
		t.Fatalf("summary with only [0,8) of b.go confirmed = %+v, want 2 required / 1 fully read / 1 served / 0 waived; a partial union is not full coverage", c)
	}
	// Phase: containment over the served ranges (Section 17.2). A scope review
	// may cite source intervals, and a citation over bytes the actor never
	// confirmed would let a note mark a file read without coverage, so the test
	// must be containment and not overlap. The sharp case is the interval that
	// spans a gap between two confirmed ranges: it touches both and is covered
	// by neither. Confirming [12,16) beside the prefix [0,8) makes that gap.
	confirmed := func(start, end uint64) bool {
		t.Helper()
		ok, err := f.s.RangeConfirmed(ctx, open.ID, actorID, b2.id, b2.hash, model.ByteRange{Start: start, End: end})
		if err != nil {
			t.Fatalf("RangeConfirmed([%d,%d)): %v", start, end, err)
		}
		return ok
	}
	island := fresh
	island.ID = model.H("chunk", "island")
	island.Bytes = model.ByteRange{Start: 12, End: 16}
	if err := f.s.IssueChunk(ctx, island); err != nil {
		t.Fatalf("IssueChunk(island): %v", err)
	}
	if err := f.s.ConfirmChunks(ctx, open.ID, actorID, []string{island.ID}); err != nil {
		t.Fatalf("ConfirmChunks(island): %v", err)
	}
	if !confirmed(0, 8) || !confirmed(4, 8) || !confirmed(12, 16) {
		t.Fatal("RangeConfirmed denied an interval inside a confirmed range; containment must hold for a sub-interval too")
	}
	if confirmed(0, 16) {
		t.Fatal("RangeConfirmed confirmed [0,16) while [8,12) was never served; the test is containment, not overlap")
	}
	if confirmed(0, 36) {
		t.Fatal("RangeConfirmed confirmed an interval running past the confirmed ranges")
	}
	// A file outside the pinned scope is not confirmed and is not an error: a
	// citation over it is a claim to refuse, not a storage failure.
	if ok, err := f.s.RangeConfirmed(ctx, open.ID, actorID, a.id, a.hash, model.ByteRange{Start: 0, End: 1}); err != nil || ok {
		t.Fatalf("RangeConfirmed(file outside the session scope) = %v %v, want false and no error", ok, err)
	}
	rest := fresh
	rest.ID = model.H("chunk", "3")
	rest.Bytes = model.ByteRange{Start: 8, End: uint64(len(b2.content))}
	if err := f.s.IssueChunk(ctx, rest); err != nil {
		t.Fatalf("IssueChunk(remainder): %v", err)
	}
	if err := f.s.ConfirmChunks(ctx, open.ID, actorID, []string{rest.ID}); err != nil {
		t.Fatalf("ConfirmChunks(remainder): %v", err)
	}
	if c := summary("union complete"); c.Required != 2 || c.FullyRead != 2 || c.Served != 2 || c.Waived != 0 {
		t.Fatalf("summary after the served union reaches b.go's size = %+v, want 2 required / 2 fully read / 2 served / 0 waived", c)
	}
	// The summary reimplements Coverage's state switch in SQL so Status costs
	// one round trip; the two answering differently is the duplicate
	// implementation Section 30.1 forbids, so assert agreement rather than only
	// the absolute counts. The summary's served column is "fully confirmed AND
	// not waived", so the paged count applies the waiver clause too -- and the
	// waiver written below re-checks the same agreement once one exists.
	page, err := f.s.Coverage(ctx, open.ID, actorID, "", 10)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	var paged int64
	for _, fc := range page {
		if fc.Requirement == model.RequirementFull && fc.State == model.CoverageFullServed && !fc.Waived {
			paged++
		}
	}
	if served := summary("agreement").Served; paged != served {
		t.Fatalf("CoverageSummary says %d required files are fully served; paging Coverage says %d", served, paged)
	}
	// Once the union closes the gap the same interval is confirmed: the answer
	// tracks the stored ranges rather than the order they arrived in.
	if !confirmed(0, 16) {
		t.Fatal("RangeConfirmed still denied [0,16) after the remainder closed the gap")
	}
	// Phase: naming the files of a session. The batch reader is scoped by
	// session_files, not by the snapshot: a.go is in the same pinned snapshot
	// but not in this session's scope, and a reader that named it would let a
	// batch enumerate paths outside the actor's scope.
	paths, err := f.s.SessionFilePaths(ctx, open.ID, actorID, []model.FileID{b2.id, empty.id, a.id})
	if err != nil {
		t.Fatalf("SessionFilePaths: %v", err)
	}
	if paths[b2.id] != b2.path || paths[empty.id] != empty.path {
		t.Fatalf("SessionFilePaths named %q and %q for the session's own files; want %q and %q",
			paths[b2.id], paths[empty.id], b2.path, empty.path)
	}
	if _, named := paths[a.id]; named || len(paths) != 2 {
		t.Fatalf("SessionFilePaths returned %d paths including a file outside the session's scope: %v", len(paths), paths)
	}

	// A waiver is audited beside coverage, never AS coverage: it raises the
	// waived count and REMOVES the file from the served count, even though this
	// one is fully served (its confirmed zero-length EOF chunk). A summary that
	// counted it in both columns reported a fully waived session as fully
	// covered, which is the claim the waiver exists to deny (Section 17.3).
	if _, err := f.s.Waive(ctx, model.WaiverRequest{SessionID: open.ID, ActorID: actorID, FileID: empty.id, Reason: "generated file reviewed out of band"}); err != nil {
		t.Fatalf("Waive: %v", err)
	}
	if c := summary("waived"); c.Required != 2 || c.FullyRead != 2 || c.Served != 1 || c.Waived != 1 {
		t.Fatalf("summary after waiving a fully served required file = %+v, want 2 required / 2 fully read / 1 served / 1 waived; the waiver clears the served column and leaves the read fact standing", c)
	}
	// ...and Coverage still reports the file full_served with the waived flag
	// set, so the two readers agree under the same definition.
	waivedPage, err := f.s.Coverage(ctx, open.ID, actorID, "", 10)
	if err != nil {
		t.Fatalf("Coverage after the waiver: %v", err)
	}
	paged = 0
	for _, fc := range waivedPage {
		if fc.Requirement == model.RequirementFull && fc.State == model.CoverageFullServed && !fc.Waived {
			paged++
		}
	}
	if served := summary("agreement under a waiver").Served; paged != served {
		t.Fatalf("CoverageSummary says %d served with a waiver in scope; paging Coverage says %d", served, paged)
	}
	// The waiver's reason survives only in coverage_waivers: Waive's return
	// value echoes the request, so this read-back is what proves the stored row
	// is what a sealed capsule carries.
	waivers, err := f.s.Waivers(ctx, open.ID, actorID)
	if err != nil {
		t.Fatalf("Waivers: %v", err)
	}
	if len(waivers) != 1 || waivers[0].FileID != empty.id || waivers[0].Reason != "generated file reviewed out of band" ||
		waivers[0].ActorID != actorID || waivers[0].CreatedAt.IsZero() {
		t.Fatalf("Waivers read back %+v; want one row for %s with the recorded reason, actor and timestamp", waivers, empty.id)
	}
	// One wrong-actor assertion covers every session reader: they all resolve
	// the session through the same actor-checked s.session, so a second reader
	// asserting it again restates one guard rather than protecting another.
	if _, err := f.s.CoverageSummary(ctx, open.ID, "someone-else"); err == nil {
		t.Fatal("CoverageSummary answered for another actor; coverage is never shared across actors")
	} else {
		wantCode(t, err, model.CodeActorMismatch)
	}
	// A capsule is the consolidate phase's completion record; a sweep session
	// has nothing to consolidate yet.
	capsule := model.Capsule{SessionID: open.ID, ActorID: actorID, Binding: bind2, ManifestHash: manifest.CanonicalHash,
		ScopeVersion: 1, CanonicalHash: model.H("capsule"), CreatedAt: time.Now().UTC()}
	if _, err := f.s.PutCapsule(ctx, capsule); err == nil {
		t.Fatal("PutCapsule stored a capsule for a session that is not in consolidate_open")
	} else {
		wantCode(t, err, model.CodeVersionConflict)
	}
	// Phase: lexical statistics are generation-local (Section 14.4). A BM25
	// score is a function of df and the document facts of the pinned
	// generation, so the same query against the same binding must yield the
	// same score while other generations stage, activate and are swept
	// underneath the reader; readerStable holds gen2 across the publication
	// and retention sequence below, which republishes "changed" in a.go.
	readerStable, err := f.s.PinGeneration(ctx, f.repo, gen2, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(gen2, stable): %v", err)
	}
	defer readerStable.Close()
	dfBefore, err := readerStable.DocumentFrequency(ctx, []string{"changed"})
	if err != nil {
		t.Fatalf("DocumentFrequency: %v", err)
	}
	hitsBefore, err := readerStable.SearchDocuments(ctx, ids)
	if err != nil {
		t.Fatalf("SearchDocuments: %v", err)
	}
	if len(dfBefore) != 1 || dfBefore[0] != 1 || len(hitsBefore) != 1 {
		t.Fatalf("gen2 lexical statistics = df %v over %d documents, want df 1 and the one b.go document containing \"changed\"", dfBefore, len(hitsBefore))
	}

	if err := reader2.Close(); err != nil {
		t.Fatalf("reader2.Close: %v", err)
	}

	// Phase: stale carry and its identity (Section 13.3). A semantic unit
	// still rebuilding after an edit is carried into the new generation so its
	// capability can answer `stale` with a distance instead of `pending`. The
	// failure mode guarded here is the one that makes a stale answer
	// indistinguishable from a fresh one: either the carried unit cannot be
	// published at all, or it publishes under an AnalysisKey identical to a
	// fresh generation's.
	//
	// The identity half is proved over snap2 with gen2's own membership: the
	// same two unit keys, the same capabilities, the same snapshot and the
	// same normalization and semantic hashes, with exactly one member marked
	// carried. Nothing else can move the key, so a key equal to gen2's would
	// mean the carried marker and the distance never reached the fold. A
	// genuinely stale unit would have a different unit key and the two keys
	// would differ for the old reason, proving nothing.
	genCarry, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "feature")
	if err != nil {
		t.Fatalf("BeginGeneration(carry): %v", err)
	}
	if err := f.s.AttachUnit(ctx, genCarry, unitA); err != nil {
		t.Fatalf("AttachUnit(unitA) into the carry generation: %v", err)
	}
	if err := f.s.AttachCarried(ctx, genCarry, unitB2, store.Carry{DistanceGenerations: 2, DistanceFiles: 1}); err != nil {
		t.Fatalf("AttachCarried(unitB2): %v", err)
	}
	bindCarry := f.activate(genCarry, gen2)
	if bindCarry.AnalysisKey == bind2.AnalysisKey {
		t.Fatal("a generation carrying a stale unit has the same analysis key as the fresh generation with the same membership; a stale answer is indistinguishable from a fresh one")
	}
	carried, err := f.s.CarriedUnits(ctx, genCarry, "", "", 0)
	if err != nil {
		t.Fatalf("CarriedUnits: %v", err)
	}
	if len(carried) != 1 || carried[0].Unit != unitB2 || carried[0].Carry != (store.Carry{DistanceGenerations: 2, DistanceFiles: 1}) {
		t.Fatalf("CarriedUnits = %+v, want only unitB2 with its recorded distance", carried)
	}

	// A ref of its own, holding a unit no other generation selects. Every
	// generation so far selects exactly the same two units, so deleting any of
	// them frees nothing and retention has nothing it could report as
	// reclaimable; this is the generation that makes that figure measurable.
	genOwn, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "release")
	if err != nil {
		t.Fatalf("BeginGeneration(release): %v", err)
	}
	runOwn := f.run(genOwn)
	wOwn := f.beginScope(genOwn, runOwn, "cfg-release", b2)
	f.fillFile(wOwn, runOwn, b2)
	if err := f.s.SealUnit(ctx, wOwn); err != nil {
		t.Fatalf("SealUnit(release): %v", err)
	}
	f.activate(genOwn, genCarry)
	// genOwn's unit indexes b.go a second time under its own config, so the
	// corpus now holds a second document containing "changed" that gen2 does
	// not select. Its rowid is the probe the stability leg below uses.
	ownReader, err := f.s.PinGeneration(ctx, f.repo, genOwn, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(genOwn): %v", err)
	}
	ownIDs, err := ownReader.Match(ctx, `"changed"`, 0, 10)
	if err != nil || len(ownIDs) != 1 || ownIDs[0] == ids[0] {
		t.Fatalf("genOwn Match(changed) = %v %v, want its own single document, not gen2's %d", ownIDs, err, ids[0])
	}
	// Released at once: a lease here would stop retention sweeping genOwn below.
	if err := ownReader.Close(); err != nil {
		t.Fatalf("ownReader.Close: %v", err)
	}

	// The publication half: over a snapshot whose a.go changed, unitA can no
	// longer be reused, and carrying it must be admitted at attach AND at
	// activation, where the membership input check runs again.
	a2 := f.file("pkg/a.go", "package pkg\nfunc A() { changed() }\n")
	snap3 := f.snapshot("three", a2, b2)
	genStale, err := f.s.BeginGeneration(ctx, f.repo, snap3.ID, model.H("semantic"), "topic")
	if err != nil {
		t.Fatalf("BeginGeneration(stale): %v", err)
	}
	if err := f.s.AttachUnit(ctx, genStale, unitA); err == nil {
		t.Fatal("AttachUnit reused a unit whose input bytes changed")
	} else {
		wantCode(t, err, model.CodeSnapshotChanged)
	}
	if err := f.s.AttachCarried(ctx, genStale, unitA, store.Carry{DistanceGenerations: 1, DistanceFiles: 1}); err != nil {
		t.Fatalf("AttachCarried(stale unitA): %v", err)
	}
	if err := f.s.AttachUnit(ctx, genStale, unitB2); err != nil {
		t.Fatalf("AttachUnit(unitB2): %v", err)
	}
	bindStale := f.activate(genStale, genOwn)
	if bindStale.AnalysisKey == bindCarry.AnalysisKey {
		t.Fatal("two generations over different snapshots share an analysis key")
	}

	// A unit whose inputs no longer exist is not carried: answering from it
	// would describe source the snapshot does not have.
	snap4 := f.snapshot("four", b2)
	genGone, err := f.s.BeginGeneration(ctx, f.repo, snap4.ID, model.H("semantic"), "topic")
	if err != nil {
		t.Fatalf("BeginGeneration(deleted input): %v", err)
	}
	if err := f.s.AttachCarried(ctx, genGone, unitA, store.Carry{DistanceGenerations: 1}); err == nil {
		t.Fatal("AttachCarried carried a unit whose input file is gone from the snapshot")
	} else {
		wantCode(t, err, model.CodeSnapshotChanged)
	}
	if err := f.s.Abort(ctx, genGone); err != nil {
		t.Fatalf("Abort(deleted input): %v", err)
	}

	// Phase: retention by distinct ref (Section 12.4). Four refs have been
	// indexed — "main" (gen2), "feature" (genCarry), "release" (genOwn) and
	// "topic" (genStale, active) — so retention keeps each ref's most recent
	// generation and sweeps what no retained ref keeps. A byte limit then
	// evicts the least recently used refs first. The failure mode is
	// unambiguous: evicting the published generation leaves the workspace
	// serving nothing, and sweeping a generation a retained session still
	// holds destroys the source a coverage receipt was issued against.
	r1, err := f.s.RetainByRef(ctx, f.repo, store.RetentionPolicy{RetainRefs: 4}, time.Now())
	if err != nil {
		t.Fatalf("RetainByRef: %v", err)
	}
	if r1.RefsRetained != 4 || r1.GenerationsSwept != 2 || r1.BytesReclaimed <= 0 || r1.UnitsDeleted < 1 {
		t.Fatalf("RetainByRef(4 refs) = %+v, want every ref retained and the two failed generations swept", r1)
	}
	// Ruling Q10: max_retained_bytes = 0 is unlimited but still measures what a
	// stricter limit could free, so `status` can warn before a disk fills. A
	// silent zero here is a warning that never fires.
	if r1.BytesReclaimable <= 0 {
		t.Fatalf("RetainByRef(%+v) reported no reclaimable bytes although two non-active "+
			"generations are retained; ruling Q10 measures it even when unlimited", r1)
	}
	for _, gen := range []model.GenerationID{gen2, genCarry, genOwn, genStale} {
		if _, err := f.s.GenerationStatus(ctx, gen); err != nil {
			t.Fatalf("retention swept generation %d, which is its ref's most recent: %v", gen, err)
		}
	}
	// The other half of the ranking-stability leg: three generations have now
	// staged and activated over gen2 and two have been swept, and genOwn's
	// second "changed" document is live. gen2's df and document facts must be
	// exactly what they were, and genOwn's rowid -- a real document of the
	// same file, outside this generation -- must be omitted, not hydrated.
	dfAfter, err := readerStable.DocumentFrequency(ctx, []string{"changed"})
	if err != nil {
		t.Fatalf("DocumentFrequency after publication and retention: %v", err)
	}
	hitsAfter, err := readerStable.SearchDocuments(ctx, []int64{ids[0], ownIDs[0]})
	if err != nil {
		t.Fatalf("SearchDocuments after publication and retention: %v", err)
	}
	if len(dfAfter) != 1 || dfAfter[0] != dfBefore[0] || len(hitsAfter) != 1 || hitsAfter[0] != hitsBefore[0] {
		t.Fatalf("gen2 lexical statistics moved from df %v %+v to df %v %+v while later generations published and were swept; a score is not reproducible within its binding",
			dfBefore, hitsBefore, dfAfter, hitsAfter)
	}

	// A limit nothing fits under: every ref but the active one is evicted, and
	// gen2 is swept only insofar as nothing else holds it — the read session
	// opened above does, so it stays.
	r2, err := f.s.RetainByRef(ctx, f.repo, store.RetentionPolicy{RetainRefs: 4, MaxRetainedBytes: 1}, time.Now())
	if err != nil {
		t.Fatalf("RetainByRef(byte limit): %v", err)
	}
	if r2.RefsRetained != 1 || r2.GenerationsSwept != 2 {
		t.Fatalf("RetainByRef(byte limit) = %+v, want only the active ref retained and the two collectable generations swept", r2)
	}
	for _, gen := range []model.GenerationID{genCarry, genOwn} {
		if _, err := f.s.GenerationStatus(ctx, gen); err == nil {
			t.Fatalf("an evicted ref's generation %d survived the byte limit", gen)
		}
	}
	if _, err := f.s.GenerationStatus(ctx, gen2); err != nil {
		t.Fatalf("retention swept a generation a retained session still holds: %v", err)
	}
	if got, _ := f.s.ActiveGeneration(ctx, f.repo); got != genStale {
		t.Fatalf("active generation after retention = %d, want the published %d; retention must never evict the active generation", got, genStale)
	}
	reader3, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration after retention: %v", err)
	}
	if _, err := reader3.Node(ctx, f.nodeID(b2)); err != nil {
		t.Fatalf("the retained active generation no longer serves its facts: %v", err)
	}
	if err := reader3.Close(); err != nil {
		t.Fatalf("reader3.Close: %v", err)
	}

	// Phase: recovery. A generation left staging with a half-built unit is
	// failed by Recover, its unsealed output deleted and its collected
	// leftovers swept exactly as DeleteGeneration's tail would; the active
	// pointer is untouched. A second Recover is a no-op.
	gen4, err := f.s.BeginGeneration(ctx, f.repo, snap3.ID, model.H("semantic"), "topic")
	if err != nil {
		t.Fatalf("BeginGeneration(4): %v", err)
	}
	run4 := f.run(gen4)
	// A dependency changes the unit key, so this is a new unit over b.go
	// rather than a rebuild of the sealed one.
	baseline := f.stats()
	// A build the caller cancelled abandons its unit rather than unwinding it:
	// the cascading delete is unbounded work the operator is waiting on, so
	// the rows survive the cancel and Recover is what must reclaim them.
	w4 := f.begin(gen4, run4, b2, unitA)
	f.fill(w4, run4, b2)
	if err := w4.Abandon(ctx); err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if abandoned := f.stats(); abandoned.NodeFacts != baseline.NodeFacts+1 || abandoned.Evidence != baseline.Evidence+1 || abandoned.SearchUnits != baseline.SearchUnits+1 {
		t.Fatalf("Abandon deleted the cancelled unit's rows (%+v, from %+v); the cancel path must defer that work to collection", abandoned, baseline)
	}
	// An expired lease is exactly the kind of leftover only the collection
	// tail removes; Recover must run that tail, not just fail the generation.
	expired := model.Lease{ID: model.H("lease", "expired"), GenerationID: genStale, OwnerKind: model.LeaseQuery, ExpiresAt: time.Now().Add(-time.Hour).UTC()}
	if err := f.s.AcquireLease(ctx, expired, ""); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	beforeRecover := f.stats()
	if err := f.s.Recover(ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if st, _ := f.s.GenerationStatus(ctx, gen4); st != model.GenerationFailed {
		t.Fatalf("gen4 status after Recover = %s, want failed", st)
	}
	if got, _ := f.s.ActiveGeneration(ctx, f.repo); got != genStale {
		t.Fatalf("Recover moved the active pointer to %d", got)
	}
	afterRecover := f.stats()
	if afterRecover.Units != beforeRecover.Units-1 {
		t.Fatalf("Recover left %d units, want the building unit deleted from %d", afterRecover.Units, beforeRecover.Units)
	}
	if afterRecover.NodeFacts != baseline.NodeFacts || afterRecover.Evidence != baseline.Evidence || afterRecover.SearchUnits != baseline.SearchUnits {
		t.Fatalf("Recover left the abandoned unit's rows behind (%+v, want the pre-build %+v)", afterRecover, baseline)
	}
	if afterRecover.Leases != beforeRecover.Leases-1 {
		t.Fatalf("Recover left %d leases, want the expired lease swept from %d", afterRecover.Leases, beforeRecover.Leases)
	}
	if err := f.s.Recover(ctx, time.Now()); err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if again := f.stats(); again != afterRecover {
		t.Fatalf("Recover is not idempotent: %+v then %+v", afterRecover, again)
	}
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Phase: a blob demoted to trash by the grace protocol is restored when
	// capture publishes the same content again; otherwise a snapshot could
	// name a blob that collection is about to remove.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hashRaw, _ := model.DecodeID(c.hash)
	if _, err := raw.Exec(`UPDATE blobs SET state = 'trash' WHERE hash = ?`, hashRaw); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	f2 := newFixture(t, dbPath)
	f2.file(c.path, string(c.content))
	if err := f2.s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, _ = sql.Open("sqlite", dbPath)
	var state string
	if err := raw.QueryRow(`SELECT state FROM blobs WHERE hash = ?`, hashRaw).Scan(&state); err != nil || state != "ready" {
		t.Fatalf("re-put blob state = %q %v, want ready", state, err)
	}

	// Phase: schema fingerprint mismatch fails closed. A tampered fingerprint
	// must refuse to open; the database is not migrated or reset.
	if _, err := raw.Exec(`UPDATE schema_meta SET fingerprint = 'not-this-schema'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := store.Open(ctx, dbPath, store.Options{}); err == nil {
		t.Fatal("Open accepted a database with a foreign schema fingerprint")
	} else {
		wantCode(t, err, model.CodeSchemaMismatch)
	}
	raw, _ = sql.Open("sqlite", dbPath)
	var remaining int
	if err := raw.QueryRow(`SELECT count(*) FROM generations`).Scan(&remaining); err != nil || remaining == 0 {
		t.Fatalf("mismatched database was altered: generations=%d err=%v", remaining, err)
	}
	raw.Close()
}

// Delta import (Section 11.4). The helpers below build a package-scoped unit
// over two files with the shapes a carry-over has to get right: a fact located
// in each file, a fact and a relation in the index-level bucket that name no
// file at all, one relation whose only evidence is in one file, aliases in
// per-file and workspace scopes, and a lexical document per file.

const deltaScope = "pkg"

// beginScope opens a package-scoped unit over several files under cfg. cfg is
// the analysis configuration digest, which is the only part of a unit identity
// a test can vary while holding the inputs fixed, so a delta-built unit and a
// full re-import of the same snapshot can coexist and be compared.
func (f *fixture) beginScope(gen model.GenerationID, run model.ProviderRunID, cfg string, files ...fileFixture) *store.UnitWriter {
	f.t.Helper()
	inputs := make([]model.UnitInput, 0, len(files))
	h := model.NewUnitInputHasher()
	for _, ff := range files {
		inputs = append(inputs, model.UnitInput{FileID: ff.id, ContentHash: ff.hash})
	}
	slices.SortFunc(inputs, func(a, b model.UnitInput) int { return strings.Compare(string(a.FileID), string(b.FileID)) })
	for _, in := range inputs {
		if err := h.Add(in); err != nil {
			f.t.Fatal(err)
		}
	}
	spec := model.UnitSpec{ProviderID: providerID, ProviderVersion: providerVersion, ScopeKey: deltaScope,
		InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, cfg)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: cfg, OriginRunID: run, SourceBinding: model.SourceBindingVerified}
	w, err := f.s.BeginUnit(f.ctx, gen, build, func(yield func(model.UnitInput) error) error {
		for _, in := range inputs {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		f.t.Fatalf("BeginUnit(%s): %v", cfg, err)
	}
	return w
}

// keyList renders readable fixture labels as the sorted lowercase digests the
// fact-key contract requires, so a test reads as "this fact is backed by these
// producer keys" while storage sees the shape a real producer publishes.
func keyList(labels ...string) []string {
	keys := make([]string, len(labels))
	for i, l := range labels {
		keys[i] = model.H("fixture-fact-key", l)
	}
	slices.Sort(keys)
	return keys
}

// putNode publishes one node fact backed by the given producer keys. ff nil
// places the fact in the unit's index-level bucket: no file, no range, and
// evidence that names neither.
func (f *fixture) putNode(w *store.UnitWriter, run model.ProviderRunID, scope, name string, ff *fileFixture, labels ...string) model.NodeID {
	f.t.Helper()
	fact := f.nodeFact(w, run, scope, name, ff)
	if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, [][]string{keyList(labels...)}); err != nil {
		f.t.Fatalf("PutKeyedNodes(%s): %v", name, err)
	}
	return fact.Node.ID
}

func (f *fixture) nodeFact(w *store.UnitWriter, run model.ProviderRunID, scope, name string, ff *fileFixture) model.NodeFact {
	f.t.Helper()
	canonical := model.CanonicalNodeKey(scope, name)
	id := model.NewNodeID(f.repo, model.NodeFunction, canonical)
	node := model.Node{ID: id, Kind: model.NodeFunction, Language: "go", Name: name, QualifiedName: scope + "." + name}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
		NodeID: id, Precision: model.PrecisionSyntax, NativeKey: name}
	if ff != nil {
		rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(len(ff.content)), Line: 1, Column: uint32(len(ff.content))}}
		node.FileID, node.ContentHash, node.Range = ff.id, ff.hash, rng
		ev.FileID, ev.ContentHash, ev.Range = ff.id, ff.hash, rng
	}
	ev.ID = model.NewEvidenceID(ev)
	return model.NodeFact{Node: node, CanonicalKey: canonical, Evidence: []model.Evidence{ev}}
}

// putRelation publishes one relation backed by one producer key. ff nil places
// its only occurrence in the index-level bucket.
func (f *fixture) putRelation(w *store.UnitWriter, run model.ProviderRunID, from, to model.NodeID, key string, ff *fileFixture) {
	f.t.Helper()
	id := model.NewRelationID(f.repo, from, model.RelCalls, to)
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
		RelationID: id, Precision: model.PrecisionSyntax, NativeKey: key}
	if ff != nil {
		ev.FileID, ev.ContentHash = ff.id, ff.hash
		ev.Range = &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(len(ff.content)), Line: 1, Column: uint32(len(ff.content))}}
	}
	ev.ID = model.NewEvidenceID(ev)
	rel := model.Relation{ID: id, From: from, Kind: model.RelCalls, To: to}
	if err := w.PutKeyedRelations(f.ctx, []model.RelationFact{{Relation: rel, Evidence: []model.Evidence{ev}}}, [][]string{keyList(key)}); err != nil {
		f.t.Fatalf("PutKeyedRelations(%s): %v", key, err)
	}
}

// fillScope publishes the whole fact set of the package unit over a and b.
func (f *fixture) fillScope(w *store.UnitWriter, run model.ProviderRunID, a, b fileFixture) {
	f.t.Helper()
	f.fillFile(w, run, a)
	f.fillFile(w, run, b)
	f.fillIndexLevel(w, run, a)
}

// fillFile publishes everything bucketed to one path: its node, its alias and
// its lexical document.
func (f *fixture) fillFile(w *store.UnitWriter, run model.ProviderRunID, ff fileFixture) model.NodeID {
	f.t.Helper()
	id := f.putNode(w, run, ff.path, "F", &ff, "key:node:"+ff.path)
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: "file:" + ff.path, NativeKey: "F", NodeID: id}}); err != nil {
		f.t.Fatalf("PutAliases(%s): %v", ff.path, err)
	}
	if err := w.PutSearchUnits(f.ctx, []model.SearchUnit{f.searchDoc(ff)}); err != nil {
		f.t.Fatalf("PutSearchUnits(%s): %v", ff.path, err)
	}
	return id
}

// fillIndexLevel publishes the facts that name no file: an external callee and
// the edge into it. Its evidence belongs to the unit's single index-level
// bucket, which a per-path delta never replaces.
func (f *fixture) fillIndexLevel(w *store.UnitWriter, run model.ProviderRunID, a fileFixture) {
	f.t.Helper()
	external := f.putNode(w, run, "workspace", "X", nil, "key:node:external")
	caller := model.NewNodeID(f.repo, model.NodeFunction, model.CanonicalNodeKey(a.path, "F"))
	f.putRelation(w, run, caller, external, "key:rel:external", nil)
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: "workspace", NativeKey: "X", NodeID: external}}); err != nil {
		f.t.Fatalf("PutAliases(workspace): %v", err)
	}
}

// factColumns are the identifying columns of each fact table, with every
// unit-scoped column excluded: the row's own unit id, the rowid search_units
// mints on insert and the evidence id, which folds the unit id by construction
// (Section 9.3) and therefore cannot match across two units describing the
// same occurrence.
var factColumns = []struct{ table, cols string }{
	{"node_facts", "lower(hex(node_id)), language, name, qualified_name, signature, lower(hex(coalesce(file_id, x''))), coalesce(start_byte,-1), coalesce(end_byte,-1), metadata_json"},
	{"relation_facts", "lower(hex(relation_id))"},
	{"fact_keys", "lower(hex(coalesce(node_id, x''))), lower(hex(coalesce(relation_id, x''))), fact_key"},
	{"native_aliases", "scope_key, native_key, lower(hex(node_id))"},
	{"search_units", "lower(hex(search_key)), lower(hex(coalesce(node_id, x''))), lower(hex(file_id)), path, kind, name, qualified_name, signature, start_byte, end_byte, body, token_count"},
	{"evidence", "lower(hex(coalesce(node_id, x''))), lower(hex(coalesce(relation_id, x''))), precision, lower(hex(coalesce(file_id, x''))), coalesce(start_byte,-1), coalesce(end_byte,-1), native_key, detail, content_hash_bound"},
}

// unitRowID resolves a unit key to the integer row the fact tables reference.
func unitRowID(t *testing.T, db *sql.DB, id model.UnitID) int64 {
	t.Helper()
	raw, _ := model.DecodeID(string(id))
	var row int64
	if err := db.QueryRow(`SELECT id FROM units WHERE unit_key = ?`, raw).Scan(&row); err != nil {
		t.Fatalf("unit row for %s: %v", id, err)
	}
	return row
}

// sameFacts asserts that two units hold exactly the same facts.
func sameFacts(t *testing.T, db *sql.DB, want, got int64) {
	t.Helper()
	for _, tb := range factColumns {
		for _, dir := range []struct {
			what string
			a, b int64
		}{{"missing from the delta unit", want, got}, {"only in the delta unit", got, want}} {
			var n int64
			q := `SELECT count(*) FROM (SELECT ` + tb.cols + ` FROM ` + tb.table + ` WHERE unit_id = ?1
				EXCEPT SELECT ` + tb.cols + ` FROM ` + tb.table + ` WHERE unit_id = ?2)`
			if err := db.QueryRow(q, dir.a, dir.b).Scan(&n); err != nil {
				t.Fatalf("%s: %v", tb.table, err)
			}
			if n != 0 {
				t.Errorf("%s: %d rows %s", tb.table, n, dir.what)
			}
		}
	}
}

func unitRows(t *testing.T, db *sql.DB, table string, unit int64) int64 {
	t.Helper()
	var n int64
	if err := db.QueryRow(`SELECT count(*) FROM `+table+` WHERE unit_id = ?`, unit).Scan(&n); err != nil {
		t.Fatalf("%s: %v", table, err)
	}
	return n
}

// TestDeltaImportInvariants covers the three ways a delta import can corrupt a
// generation while looking like it worked. Each subtest names the silent
// failure it protects against.
func TestDeltaImportInvariants(t *testing.T) {
	t.Run("carried unit is identical to a full re-import", func(t *testing.T) {
		// A delta that produced anything other than what a full import would
		// produce makes the index depend on how it was built, not on the
		// source: two machines refreshing the same tree would answer
		// differently and neither would be wrong by its own accounting.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a1 := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		b := f.file("pkg/b.go", "package pkg\nfunc F() { F() }\n")
		snap1 := f.snapshot("one", a1, b)
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		w1 := f.beginScope(gen1, run1, configHash, a1, b)
		f.fillScope(w1, run1, a1, b)
		if err := f.s.SealUnit(f.ctx, w1); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
		prev := w1.UnitID()

		// a.go is edited; b.go is untouched.
		a2 := f.file("pkg/a.go", "package pkg\nfunc F() { /* edited */ }\n")
		snap2 := f.snapshot("two", a2, b)
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		wFull := f.beginScope(gen2, run2, "cfg-full", a2, b)
		f.fillScope(wFull, run2, a2, b)
		if err := f.s.SealUnit(f.ctx, wFull); err != nil {
			t.Fatalf("SealUnit(full): %v", err)
		}

		gen3, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run3 := f.run(gen3)
		wDelta := f.beginScope(gen3, run3, "cfg-delta", a2, b)
		f.fillFile(wDelta, run3, a2)
		f.fillIndexLevel(wDelta, run3, a2)
		stats, err := wDelta.CarryOver(f.ctx, prev, store.Replaced{
			Files:  slices.Values([]model.FileID{a2.id}),
			Scopes: slices.Values([]string{"file:" + a2.path}),
			Keys:   slices.Values(keyList("key:node:" + a2.path)),
		})
		if err != nil {
			t.Fatalf("CarryOver: %v", err)
		}
		if stats.Nodes != 1 || stats.SearchUnits != 1 || stats.Aliases != 1 {
			t.Errorf("carried %+v, want b.go's node, alias and document", stats)
		}
		if err := f.s.SealUnit(f.ctx, wDelta); err != nil {
			t.Fatalf("SealUnit(delta): %v", err)
		}

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		sameFacts(t, raw, unitRowID(t, raw, wFull.UnitID()), unitRowID(t, raw, wDelta.UnitID()))
		// Carried search documents keep the rowids the FTS index was built
		// against. SealUnit only counts documents, so an index entry pointing
		// at the wrong row passes every other check and then answers a lexical
		// query with another document's text. FTS5 with rank 1 compares the
		// index against its external content table and errors when they
		// disagree; without the argument it only checks the index internally
		// and this mismatch passes.
		if _, err := raw.Exec(`INSERT INTO search_fts(search_fts, rank) VALUES('integrity-check', 1)`); err != nil {
			t.Fatalf("carried search documents left the FTS index disagreeing with its content: %v", err)
		}
	})

	t.Run("a replaced key is never carried", func(t *testing.T) {
		// A delta that keeps a fact its producer says is gone publishes a
		// declaration the source no longer contains, and no later refresh
		// removes it: the row is carried again forever.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		b := f.file("pkg/b.go", "package pkg\nfunc F() {}\n")
		snap := f.snapshot("one", a, b)
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		w1 := f.beginScope(gen1, run1, configHash, a, b)
		f.fillFile(w1, run1, a)
		f.fillFile(w1, run1, b)
		gone := f.putNode(w1, run1, "pkg/b.go", "Gone", &b, "key:node:gone")
		if err := f.s.SealUnit(f.ctx, w1); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}

		// The refresh reports key:node:gone removed and re-emits nothing for
		// it. Nothing else about b.go changed, so its bucket is not replaced:
		// only the key can keep the fact out.
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		w2 := f.beginScope(gen2, run2, "cfg-delta", a, b)
		if _, err := w2.CarryOver(f.ctx, w1.UnitID(), store.Replaced{Keys: slices.Values(keyList("key:node:gone"))}); err != nil {
			t.Fatalf("CarryOver: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w2); err != nil {
			t.Fatalf("SealUnit(delta): %v", err)
		}

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		row := unitRowID(t, raw, w2.UnitID())
		if n := unitRows(t, raw, "node_facts", row); n != 2 {
			t.Errorf("delta unit has %d node facts, want 2 (the removed key must not be carried)", n)
		}
		goneRaw, _ := model.DecodeID(string(gone))
		var leaked int64
		if err := raw.QueryRow(`SELECT count(*) FROM node_facts WHERE unit_id = ? AND node_id = ?`, row, goneRaw).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Errorf("the removed fact leaked into the new unit")
		}
		// Its evidence must go with it: evidence is never carried for a fact
		// the new unit does not hold.
		if err := raw.QueryRow(`SELECT count(*) FROM evidence WHERE unit_id = ? AND node_id = ?`, row, goneRaw).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Errorf("the removed fact's evidence leaked into the new unit")
		}
	})

	t.Run("a divergent repeated identity is refused", func(t *testing.T) {
		// The fact insert yields to the row already written, so a second
		// observation of one identity that DESCRIBES IT DIFFERENTLY would be
		// dropped with no counter and no diagnostic — and because CarryOver
		// runs last, the full path would keep the first description while the
		// delta path kept the fresh one. The same source would then seal two
		// different units depending on how each was built. An identical
		// repeat, which is what two workers and a subdivided unit produce, must
		// stay free, and a repeat may add keys.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		snap := f.snapshot("one", a)
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run := f.run(gen)
		w := f.beginScope(gen, run, configHash, a)
		fact := f.nodeFact(w, run, "pkg/a.go", "F", &a)
		if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, [][]string{keyList("key:one")}); err != nil {
			t.Fatalf("PutKeyedNodes: %v", err)
		}
		// The same identity, described differently, with its own occurrence.
		divergent := fact
		divergent.Node.Signature = "func F(x int)"
		ev := fact.Evidence[0]
		ev.NativeKey = "divergent-occurrence"
		ev.ID = model.NewEvidenceID(ev)
		divergent.Evidence = []model.Evidence{ev}
		wantCode(t, w.PutKeyedNodes(f.ctx, []model.NodeFact{divergent}, [][]string{keyList("key:one")}), model.CodeProviderOutputInvalid)
		// An identical repeat, and a repeat that adds a second key, are both
		// ordinary: one fact is backed by every key that produced it.
		if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, [][]string{keyList("key:one")}); err != nil {
			t.Fatalf("identical repeat: %v", err)
		}
		if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, [][]string{keyList("key:one", "key:two")}); err != nil {
			t.Fatalf("repeat under a second key: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		row := unitRowID(t, raw, w.UnitID())
		var leaked int64
		if err := raw.QueryRow(`SELECT count(*) FROM evidence WHERE unit_id = ? AND native_key = ?`, row, "divergent-occurrence").Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Errorf("the refused fact's evidence was stored anyway")
		}
		if n := unitRows(t, raw, "fact_keys", row); n != 2 {
			t.Errorf("the fact carries %d keys, want both keys it was published under", n)
		}
	})

	t.Run("a fact is carried unless any of its keys is replaced", func(t *testing.T) {
		// A published fact is backed by every key that produced it, and its
		// producer re-emits the whole fact as soon as ONE of those keys
		// changes. Carrying a fact because some other key of it still holds
		// would therefore keep a stale copy beside the fresh one; dropping a
		// fact none of whose keys was replaced would lose a fact the source
		// still contains, with nothing to re-emit it.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		b := f.file("pkg/b.go", "package pkg\nfunc F() {}\n")
		snap := f.snapshot("one", a, b)
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		w1 := f.beginScope(gen1, run1, configHash, a, b)
		f.fillFile(w1, run1, a)
		f.fillFile(w1, run1, b)
		partly := f.putNode(w1, run1, "pkg/b.go", "Partly", &b, "key:partly:1", "key:partly:2")
		stable := f.putNode(w1, run1, "pkg/b.go", "Stable", &b, "key:stable:1", "key:stable:2")
		if err := f.s.SealUnit(f.ctx, w1); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}

		// One of Partly's two keys is replaced and none of Stable's. No bucket
		// is replaced, so only the keys decide.
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		w2 := f.beginScope(gen2, run2, "cfg-delta", a, b)
		if _, err := w2.CarryOver(f.ctx, w1.UnitID(), store.Replaced{Keys: slices.Values(keyList("key:partly:1"))}); err != nil {
			t.Fatalf("CarryOver: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w2); err != nil {
			t.Fatalf("SealUnit(delta): %v", err)
		}

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		row := unitRowID(t, raw, w2.UnitID())
		for _, want := range []struct {
			what    string
			id      model.NodeID
			carried bool
		}{{"a fact with one replaced key", partly, false}, {"a fact with no replaced key", stable, true}} {
			idRaw, _ := model.DecodeID(string(want.id))
			var n int64
			if err := raw.QueryRow(`SELECT count(*) FROM node_facts WHERE unit_id = ? AND node_id = ?`, row, idRaw).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (n != 0) != want.carried {
				t.Errorf("%s: carried=%v, want %v", want.what, n != 0, want.carried)
			}
			// A carried fact keeps every key it was published under, or the
			// successor holds facts no later refresh can replace or remove.
			if err := raw.QueryRow(`SELECT count(*) FROM fact_keys WHERE unit_id = ? AND node_id = ?`, row, idRaw).Scan(&n); err != nil {
				t.Fatal(err)
			}
			wantKeys := int64(0)
			if want.carried {
				wantKeys = 2
			}
			if n != wantKeys {
				t.Errorf("%s: carried %d keys, want %d", want.what, n, wantKeys)
			}
		}
	})

	t.Run("a failed delta leaves the previous unit intact", func(t *testing.T) {
		// Carry-over copies rows into the new unit rather than sharing the
		// previous unit's. If it ever shared them, discarding a failed refresh
		// would delete facts the still-published generation is serving.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		b := f.file("pkg/b.go", "package pkg\nfunc F() {}\n")
		snap := f.snapshot("one", a, b)
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		w1 := f.beginScope(gen1, run1, configHash, a, b)
		f.fillScope(w1, run1, a, b)
		if err := f.s.SealUnit(f.ctx, w1); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
		f.activate(gen1, 0)

		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		prevRow := unitRowID(t, raw, w1.UnitID())
		before := map[string]int64{}
		for _, tb := range factColumns {
			before[tb.table] = unitRows(t, raw, tb.table, prevRow)
		}

		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		w2 := f.beginScope(gen2, run2, "cfg-delta", a, b)
		if _, err := w2.CarryOver(f.ctx, w1.UnitID(), store.Replaced{}); err != nil {
			t.Fatalf("CarryOver: %v", err)
		}
		if err := w2.Fail(f.ctx); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		if state, exists, err := f.s.UnitState(f.ctx, w2.UnitID()); err != nil || exists {
			t.Errorf("failed delta unit is %s/exists=%v (err %v), want gone", state, exists, err)
		}
		for _, tb := range factColumns {
			if got := unitRows(t, raw, tb.table, prevRow); got != before[tb.table] {
				t.Errorf("%s: previous unit has %d rows after the failed delta, had %d", tb.table, got, before[tb.table])
			}
		}
		if state, exists, err := f.s.UnitState(f.ctx, w1.UnitID()); err != nil || !exists || state != model.UnitSealed {
			t.Errorf("previous unit is %s/exists=%v (err %v), want sealed", state, exists, err)
		}
	})

	t.Run("batched adjacency spans units but stays inside the generation", func(t *testing.T) {
		// A frontier expansion reads many units in one statement. If the batch
		// lost its membership predicate, an edge published only by a unit this
		// generation does not select would be served as a fact of it: the
		// traversal would report a call the pinned snapshot does not contain,
		// and every answer derived from it would be wrong with no way to tell.
		// The same batch must still cross unit boundaries, or the traversal
		// silently stops at the first unit edge.
		dbPath := filepath.Join(t.TempDir(), "codectx.db")
		f := newFixture(t, dbPath)
		a := f.file("pkg/a.go", "package pkg\nfunc F() { G() }\n")
		b := f.file("pkg/b.go", "package pkg\nfunc F() { G() }\n")
		snap1 := f.snapshot("one", a, b)
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		// fanOut is the number of edges the unit publishes from one extra node,
		// one more than a page can hold, so a single batch over that node
		// cannot come back whole. It hangs off nothing the assertions below
		// name, so it changes no other count in this scenario.
		const fanOut = model.MaxPageItems + 1
		edge := func(gen model.GenerationID, run model.ProviderRunID, ff fileFixture, wide int) (model.UnitID, model.NodeID, model.NodeID) {
			w := f.begin(gen, run, ff)
			from := f.putNode(w, run, ff.path, "F", &ff, "key:node:from:"+ff.path)
			to := f.putNode(w, run, ff.path, "G", &ff, "key:node:to:"+ff.path)
			f.putRelation(w, run, from, to, "key:rel:"+ff.path, &ff)
			var hub model.NodeID
			if wide > 0 {
				hub = f.putNode(w, run, ff.path, "W", &ff, "key:node:wide:"+ff.path)
				for i := 0; i < wide; i++ {
					name := fmt.Sprintf("W%03d", i)
					leaf := f.putNode(w, run, ff.path, name, &ff, "key:node:wide:"+name+":"+ff.path)
					f.putRelation(w, run, hub, leaf, "key:rel:wide:"+name+":"+ff.path, &ff)
				}
			}
			if err := f.s.SealUnit(f.ctx, w); err != nil {
				t.Fatalf("SealUnit(%s): %v", ff.path, err)
			}
			return w.UnitID(), from, hub
		}
		unitA, fromA, wideA := edge(gen1, run1, a, fanOut)
		_, fromB, _ := edge(gen1, run1, b, 0)
		f.activate(gen1, 0)

		// gen2 drops b.go entirely: only unit A is a member, so b.go's edge is
		// still stored but is not a fact of gen2.
		snap2 := f.snapshot("two", a)
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.AttachUnit(f.ctx, gen2, unitA); err != nil {
			t.Fatalf("AttachUnit: %v", err)
		}
		f.activate(gen2, gen1)

		batch := func(gen model.GenerationID, dir model.Direction) []model.Relation {
			t.Helper()
			r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
			if err != nil {
				t.Fatalf("PinGeneration(%d): %v", gen, err)
			}
			defer r.Close()
			// 256 is the limit the graph engine actually passes (its
			// adjacencyBatch), which is ABOVE model.MaxPageItems: pageLimit
			// clamps it, and a row calling with exactly MaxPageItems would
			// never exercise that clamp at all.
			rels, err := r.EdgesBatch(f.ctx, []model.NodeID{fromA, fromB}, dir, nil, "", 256)
			if err != nil {
				t.Fatalf("EdgesBatch(%d, %s): %v", gen, dir, err)
			}
			return rels
		}
		// gen1 selects both units, so one batch over both seed nodes crosses
		// the unit boundary and returns both edges.
		if got := batch(gen1, model.DirectionOutgoing); len(got) != 2 {
			t.Fatalf("gen1 batch returned %d edges, want both units' edges: %+v", len(got), got)
		}
		// gen2 selects only unit A. The UNION form must be restricted too, so
		// both directions are asserted.
		for _, dir := range []model.Direction{model.DirectionOutgoing, model.DirectionBoth} {
			got := batch(gen2, dir)
			if len(got) != 1 || got[0].From != fromA {
				t.Fatalf("gen2 %s batch = %+v, want only unit A's edge from %s", dir, got, fromA)
			}
		}

		// The clamp itself, observed rather than assumed: the engine asks for
		// 256 and pageLimit hands back at most model.MaxPageItems, so a caller
		// that read a short page as the end of the walk would stop one row
		// short of this node's neighbourhood and call it complete.
		func() {
			r, err := f.s.PinGeneration(f.ctx, f.repo, gen1, time.Minute)
			if err != nil {
				t.Fatalf("PinGeneration(%d): %v", gen1, err)
			}
			defer r.Close()
			first, err := r.EdgesBatch(f.ctx, []model.NodeID{wideA}, model.DirectionOutgoing, nil, "", 256)
			if err != nil {
				t.Fatalf("EdgesBatch(wide): %v", err)
			}
			if len(first) != model.MaxPageItems {
				t.Fatalf("a 256-row request over a %d-edge node returned %d rows, want the %d-row clamp",
					fanOut, len(first), model.MaxPageItems)
			}
			next, err := r.EdgesBatch(f.ctx, []model.NodeID{wideA}, model.DirectionOutgoing, nil,
				first[len(first)-1].ID, 256)
			if err != nil {
				t.Fatalf("EdgesBatch(wide, after): %v", err)
			}
			if len(next) != fanOut-model.MaxPageItems {
				t.Fatalf("the page after the clamp returned %d rows, want the remaining %d: the clamped page was the whole neighbourhood after all",
					len(next), fanOut-model.MaxPageItems)
			}
		}()
	})
}

// TestActivateCapabilityDetails covers the diagnostic detail map a provider
// attaches to a non-fresh capability. It protects two silent failures: the
// details never reaching storage at all, which turns "partial" into a claim
// with no evidence behind it, and the AnalysisKey depending on the order a
// publisher happened to add the pairs, which would make two identical
// generations disagree on their own reproducible fingerprint.
func TestActivateCapabilityDetails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	snap := f.snapshot("one", a)

	partial := func(pairs ...string) []model.CapabilityState {
		c := model.CapabilityState{ProviderID: providerID, Capability: "structure", Scope: "workspace",
			State: model.CapabilityPartial, DiagnosticCode: "CTX_PROVIDER_PARTIAL"}
		for i := 0; i < len(pairs); i += 2 {
			c = c.WithDetail(pairs[i], pairs[i+1])
		}
		return []model.CapabilityState{c}
	}

	publish := func(caps []model.CapabilityState, expected model.GenerationID, reuse model.UnitID) (model.Binding, model.UnitID) {
		t.Helper()
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		unit := reuse
		if unit == "" {
			unit = f.unit(gen, f.run(gen), a)
		} else if err := f.s.AttachUnit(f.ctx, gen, unit); err != nil {
			t.Fatalf("AttachUnit: %v", err)
		}
		b, err := f.s.Activate(f.ctx, gen, expected, model.HealthDegraded, caps, "norm-v1")
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		return b, unit
	}

	first, unit := publish(partial("skipped_methods", "hover,signature", "unsupported_labels", "7"), 0, "")
	// The same pairs added in the opposite order are the same report.
	second, _ := publish(partial("unsupported_labels", "7", "skipped_methods", "hover,signature"), first.GenerationID, unit)
	if first.AnalysisKey != second.AnalysisKey {
		t.Errorf("analysis key depends on the order details were added: %s vs %s", first.AnalysisKey, second.AnalysisKey)
	}
	// A different detail is a different report, or the fold is decorative.
	third, _ := publish(partial("skipped_methods", "hover", "unsupported_labels", "7"), second.GenerationID, unit)
	if third.AnalysisKey == first.AnalysisKey {
		t.Errorf("analysis key ignores capability details")
	}

	r, err := f.s.PinGeneration(f.ctx, f.repo, third.GenerationID, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer r.Close()
	got, err := r.Capabilities(f.ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(got) != 1 || got[0].Details["skipped_methods"] != "hover" || got[0].Details["unsupported_labels"] != "7" {
		t.Errorf("capability details did not round-trip: %+v", got)
	}
}
