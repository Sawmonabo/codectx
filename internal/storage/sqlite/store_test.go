package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
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
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", ff.path, err)
	}
	return w.UnitID()
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
	gen1, err := f.s.BeginGeneration(ctx, f.repo, snap1.ID, model.H("semantic"))
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
	snap2 := f.snapshot("two", a, b2)
	reader1, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	gen2, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"))
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
	gen3, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"))
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
	if afterGC.Snapshots != 1 || afterGC.Files != 2 {
		t.Fatalf("after collecting gen1: %d snapshots %d files, want 1 snapshot and 2 files (c.go collected)", afterGC.Snapshots, afterGC.Files)
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
	entries := []model.ContextEntry{{Ordinal: 0, FileID: b2.id, Requirement: model.RequirementFull, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10, Reasons: []string{"seed"}}}
	slices := []model.ContextSlice{{Index: 0, EntryOrdinals: []int{0}, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10}}
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
	// A capsule is the consolidate phase's completion record; a sweep session
	// has nothing to consolidate yet.
	capsule := model.Capsule{SessionID: open.ID, ActorID: actorID, Binding: bind2, ManifestHash: manifest.CanonicalHash,
		ScopeVersion: 1, CanonicalHash: model.H("capsule"), CreatedAt: time.Now().UTC()}
	if _, err := f.s.PutCapsule(ctx, capsule); err == nil {
		t.Fatal("PutCapsule stored a capsule for a session that is not in consolidate_open")
	} else {
		wantCode(t, err, model.CodeVersionConflict)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("reader2.Close: %v", err)
	}

	// Phase: recovery. A generation left staging with a half-built unit is
	// failed by Recover, its unsealed output deleted and its collected
	// leftovers swept exactly as DeleteGeneration's tail would; the active
	// pointer is untouched. A second Recover is a no-op.
	gen4, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"))
	if err != nil {
		t.Fatalf("BeginGeneration(4): %v", err)
	}
	run4 := f.run(gen4)
	// A dependency changes the unit key, so this is a new unit over b.go
	// rather than a rebuild of the sealed one.
	f.begin(gen4, run4, b2, unitA)
	// An expired lease is exactly the kind of leftover only the collection
	// tail removes; Recover must run that tail, not just fail the generation.
	expired := model.Lease{ID: model.H("lease", "expired"), GenerationID: gen2, OwnerKind: model.LeaseQuery, ExpiresAt: time.Now().Add(-time.Hour).UTC()}
	if err := f.s.AcquireLease(ctx, expired); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	beforeRecover := f.stats()
	if err := f.s.Recover(ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if st, _ := f.s.GenerationStatus(ctx, gen4); st != model.GenerationFailed {
		t.Fatalf("gen4 status after Recover = %s, want failed", st)
	}
	if got, _ := f.s.ActiveGeneration(ctx, f.repo); got != gen2 {
		t.Fatalf("Recover moved the active pointer to %d", got)
	}
	afterRecover := f.stats()
	if afterRecover.Units != beforeRecover.Units-1 {
		t.Fatalf("Recover left %d units, want the building unit deleted from %d", afterRecover.Units, beforeRecover.Units)
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

// putNode publishes one keyed node fact. ff nil places the fact in the unit's
// index-level bucket: no file, no range, and evidence that names neither.
func (f *fixture) putNode(w *store.UnitWriter, run model.ProviderRunID, scope, name, key string, ff *fileFixture) model.NodeID {
	f.t.Helper()
	fact := f.nodeFact(w, run, scope, name, ff)
	if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, []string{key}); err != nil {
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

// putRelation publishes one keyed relation. ff nil places its only occurrence
// in the index-level bucket.
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
	if err := w.PutKeyedRelations(f.ctx, []model.RelationFact{{Relation: rel, Evidence: []model.Evidence{ev}}}, []string{key}); err != nil {
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
	id := f.putNode(w, run, ff.path, "F", "key:node:"+ff.path, &ff)
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
	external := f.putNode(w, run, "workspace", "X", "key:node:external", nil)
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
	{"node_facts", "lower(hex(node_id)), language, name, qualified_name, signature, lower(hex(coalesce(file_id, x''))), coalesce(start_byte,-1), coalesce(end_byte,-1), metadata_json, fact_key"},
	{"relation_facts", "lower(hex(relation_id)), fact_key"},
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
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"))
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
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"))
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		wFull := f.beginScope(gen2, run2, "cfg-full", a2, b)
		f.fillScope(wFull, run2, a2, b)
		if err := f.s.SealUnit(f.ctx, wFull); err != nil {
			t.Fatalf("SealUnit(full): %v", err)
		}

		gen3, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"))
		if err != nil {
			t.Fatal(err)
		}
		run3 := f.run(gen3)
		wDelta := f.beginScope(gen3, run3, "cfg-delta", a2, b)
		f.fillFile(wDelta, run3, a2)
		f.fillIndexLevel(wDelta, run3, a2)
		stats, err := wDelta.CarryOver(f.ctx, prev, store.Replaced{
			Files:  []model.FileID{a2.id},
			Scopes: []string{"file:" + a2.path},
			Keys:   slices.Values([]string{"key:node:" + a2.path}),
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
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
		if err != nil {
			t.Fatal(err)
		}
		run1 := f.run(gen1)
		w1 := f.beginScope(gen1, run1, configHash, a, b)
		f.fillFile(w1, run1, a)
		f.fillFile(w1, run1, b)
		gone := f.putNode(w1, run1, "pkg/b.go", "Gone", "key:node:gone", &b)
		if err := f.s.SealUnit(f.ctx, w1); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}

		// The refresh reports key:node:gone removed and re-emits nothing for
		// it. Nothing else about b.go changed, so its bucket is not replaced:
		// only the key can keep the fact out.
		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
		if err != nil {
			t.Fatal(err)
		}
		run2 := f.run(gen2)
		w2 := f.beginScope(gen2, run2, "cfg-delta", a, b)
		if _, err := w2.CarryOver(f.ctx, w1.UnitID(), store.Replaced{Keys: slices.Values([]string{"key:node:gone"})}); err != nil {
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

	t.Run("a repeated identity under a second key is refused", func(t *testing.T) {
		// A stored fact row holds one delta key. A producer whose keys are
		// finer than the identities they resolve to would leave a row keyed by
		// one of its several keys; a later refresh that removes only that key,
		// while the others still hold, drops a fact the source still contains
		// — and nothing re-emits it, because its surviving keys are unchanged.
		f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		snap := f.snapshot("one", a)
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
		if err != nil {
			t.Fatal(err)
		}
		run := f.run(gen)
		w := f.beginScope(gen, run, configHash, a)
		fact := f.nodeFact(w, run, "pkg/a.go", "F", &a)
		if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, []string{"key:one"}); err != nil {
			t.Fatalf("PutKeyedNodes: %v", err)
		}
		wantCode(t, w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, []string{"key:two"}), model.CodeProviderOutputInvalid)
		// The same key again is the ordinary repeat a deduplicating sink
		// produces and must still be accepted.
		if err := w.PutKeyedNodes(f.ctx, []model.NodeFact{fact}, []string{"key:one"}); err != nil {
			t.Fatalf("repeat under the same key: %v", err)
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
		gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
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

		gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
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
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"))
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
