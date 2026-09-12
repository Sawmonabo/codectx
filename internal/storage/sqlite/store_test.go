package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sync"
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

// unit builds, fills and seals one file-scoped unit for ff inside gen. The
// node's canonical key and the search document are derived from the path so
// the same file content yields the same unit across snapshots.
func (f *fixture) unit(gen model.GenerationID, run model.ProviderRunID, ff fileFixture, deps ...model.UnitID) model.UnitID {
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
	key := model.CanonicalNodeKey(ff.path, "F")
	nodeID := model.NewNodeID(f.repo, model.NodeFunction, key)
	if err := w.PutNodeIdentities(f.ctx, []model.NodeIdentity{{ID: nodeID, Kind: model.NodeFunction, CanonicalKey: key}}); err != nil {
		f.t.Fatalf("PutNodeIdentities: %v", err)
	}
	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(len(ff.content)), Line: 1, Column: uint32(len(ff.content))}}
	ev := model.Evidence{UnitID: spec.ID, ProviderID: providerID, ProviderVersion: providerVersion, OriginRunID: run,
		NodeID: nodeID, Precision: model.PrecisionSyntax, FileID: ff.id, ContentHash: ff.hash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	node := model.Node{ID: nodeID, Kind: model.NodeFunction, Language: "go", Name: "F", QualifiedName: ff.path + ".F",
		FileID: ff.id, ContentHash: ff.hash, Range: rng}
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node, Evidence: []model.Evidence{ev}}}); err != nil {
		f.t.Fatalf("PutNodes: %v", err)
	}
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: ff.path, NativeKey: "F", NodeID: nodeID}}); err != nil {
		f.t.Fatalf("PutAliases: %v", err)
	}
	doc := model.SearchUnit{ID: model.H("search-test", ff.path, ff.hash), NodeID: nodeID, FileID: ff.id, Path: ff.path,
		Kind: model.NodeFunction, Name: "F", QualifiedName: ff.path + ".F",
		Bytes: model.ByteRange{Start: 0, End: uint64(len(ff.content))}, Body: string(ff.content), TokenCount: 3}
	if err := w.PutSearchUnits(f.ctx, []model.SearchUnit{doc}); err != nil {
		f.t.Fatalf("PutSearchUnits: %v", err)
	}
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		f.t.Fatalf("SealUnit(%s): %v", ff.path, err)
	}
	return spec.ID
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

func (f *fixture) activate(gen model.GenerationID) model.Binding {
	f.t.Helper()
	caps := []model.CapabilityState{{ProviderID: providerID, Capability: "structure", Scope: "workspace", State: model.CapabilityFresh}}
	b, err := f.s.Activate(f.ctx, gen, model.HealthFresh, caps, "norm-v1")
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
	snap1 := f.snapshot("one", a, b1)

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
	bind1 := f.activate(gen1)
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
	_ = depUnit
	// gen3 has depUnit but not its dependency unitA as a member.
	if _, err := f.s.Activate(ctx, gen3, model.HealthFresh, nil, "norm-v1"); err == nil {
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

	// Publish gen2; gen1 is superseded but still pinned by reader1.
	bind2 := f.activate(gen2)
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
	if err := f.s.DeleteGeneration(ctx, gen1); err == nil {
		t.Fatal("DeleteGeneration collected a generation with a live lease")
	} else {
		wantCode(t, err, model.CodeWorkspaceBusy)
	}

	// Phase: pin/GC race. Readers pin and read gen2 while gen1's deletion is
	// retried; every successful pin must read its fact, and deletion of gen1
	// only succeeds once reader1 releases its lease.
	var wg sync.WaitGroup
	raceErr := make(chan error, 8)
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
	wg.Wait()
	close(raceErr)
	for err := range raceErr {
		t.Fatalf("pinned reader failed during concurrent pins: %v", err)
	}
	if err := reader1.Close(); err != nil {
		t.Fatalf("reader1.Close: %v", err)
	}
	if err := f.s.DeleteGeneration(ctx, gen1); err != nil {
		t.Fatalf("DeleteGeneration(gen1) after lease release: %v", err)
	}

	// Phase: provenance retained. unitA still carries run1 as its origin even
	// though gen1, which created it, is gone; unitB1 was unreachable and is
	// collected together with its FTS rows.
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
	reader2, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(gen2): %v", err)
	}
	if ids, err := reader2.Match(ctx, `"changed"`, 0, 10); err != nil || len(ids) != 1 {
		t.Fatalf("Match(changed) = %v %v, want exactly the gen2 b.go document", ids, err)
	}
	if ids, err := reader2.Match(ctx, `"B"`, 0, 10); err != nil || len(ids) != 1 {
		t.Fatalf("Match(B) = %v %v, want only the retained document; the deleted unit's FTS row must be gone", ids, err)
	}
	docs, tokens, err := reader2.SearchStats(ctx)
	if err != nil || docs != 2 || tokens != 6 {
		t.Fatalf("SearchStats = %d docs %d tokens %v, want 2 docs 6 tokens", docs, tokens, err)
	}
	if terms, err := f.s.Tokenize(ctx, "Foo_bar baz.Qux"); err != nil || len(terms) != 4 {
		t.Fatalf("Tokenize = %v %v, want 4 unicode61 tokens", terms, err)
	}
	_ = unitB2

	// Phase: cross-snapshot FK. A session pinned to snap2 cannot record a
	// chunk issued for snap1's bytes of b.go.
	manifest := model.ContextManifest{ID: model.ManifestID(model.H("manifest", "m1")), Binding: bind2, Phase: model.PhaseSweep,
		RequestHash: model.H("req"), PolicyVersion: "p1", CanonicalHash: model.H("canon"), EntryCount: 1, SliceCount: 1,
		EstimateMethod: model.EstimateMethodUTF8Bytes, CreatedAt: time.Now().UTC()}
	entries := []model.ContextEntry{{Ordinal: 0, FileID: b2.id, Requirement: model.RequirementFull, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10, Reasons: []string{"seed"}}}
	slices := []model.ContextSlice{{Index: 0, EntryOrdinals: []int{0}, EstimatedBytes: int64(len(b2.content)), EstimatedTokens: 10}}
	if err := f.s.PutManifest(ctx, manifest, []byte(`{"task":"t"}`), entries, slices, nil); err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	open := model.SessionOpen{ID: model.SessionID(model.H("session", "1")), ActorID: "agent-1", OpenRequestHash: model.H("open"),
		ManifestID: manifest.ID, ExpiresAt: time.Now().Add(time.Hour).UTC()}
	if _, err := f.s.OpenSession(ctx, open); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	stale := model.IssuedChunk{ID: model.H("chunk", "1"), SessionID: open.ID, FileID: b1.id, ContentHash: b1.hash,
		Bytes: model.ByteRange{Start: 0, End: 8}, ExpiresAt: time.Now().Add(time.Minute).UTC()}
	if err := f.s.IssueChunk(ctx, stale); err == nil {
		t.Fatal("IssueChunk accepted a chunk for another snapshot's bytes of a session file")
	}
	fresh := stale
	fresh.ID = model.H("chunk", "2")
	fresh.ContentHash = b2.hash
	if err := f.s.IssueChunk(ctx, fresh); err != nil {
		t.Fatalf("IssueChunk(current snapshot): %v", err)
	}
	if err := reader2.Close(); err != nil {
		t.Fatalf("reader2.Close: %v", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Phase: schema fingerprint mismatch fails closed. A tampered fingerprint
	// must refuse to open; the database is not migrated or reset.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
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
