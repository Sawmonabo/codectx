//go:build linux

package sqlite_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// procIO reads one counter of /proc/self/io.
func procIO(t *testing.T, field string) int64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, field+": "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				t.Fatalf("%s: %v", field, err)
			}
			return n
		}
	}
	t.Fatalf("/proc/self/io has no %s line", field)
	return 0
}

// procWriteBytes is the bytes this process has caused to be sent to the
// block layer so far (write_bytes): a page counts when it is dirtied, so a
// page written many times while it stays dirty counts once, and a page the
// pacer has cleaned counts again when it is dirtied again. It is what reaches
// the disk, which is what saturates a host, and not what a file grew by.
func procWriteBytes(t *testing.T) int64 { return procIO(t, "write_bytes") }

// procWriteChars is the bytes this process has passed to write calls so far
// (wchar), whether or not the kernel later merged them: what the engine
// itself wrote.
func procWriteChars(t *testing.T) int64 { return procIO(t, "wchar") }

func envInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

// sealRepository seals units file-scoped units of perUnit facts each into a
// fresh store at path, the way an index run does, and returns the fixture.
func sealRepository(t *testing.T, path string, units, perUnit int) *fixture {
	t.Helper()
	f := newFixture(t, path)
	sealRepositoryInto(t, f, units, perUnit, "", nil)
	return f
}

// sealRepositoryInto seals units file-scoped units of perUnit facts each into
// f's store, calling after (when given) once each unit is sealed, and returns
// the staging generation they were sealed into. salt varies the file bodies,
// so a second call into the same store seals units of its own rather than
// re-sealing the first call's.
func sealRepositoryInto(t *testing.T, f *fixture, units, perUnit int, salt string, after func()) model.GenerationID {
	t.Helper()
	files := make([]fileFixture, units)
	for i := range files {
		files[i] = f.file(fmt.Sprintf("pkg%d/file%d.go", i%37, i), fmt.Sprintf("package p%d\n// unit %d%s\n%s", i%37, i, salt, strings.Repeat("x", 32*perUnit+32)))
	}
	snap := f.snapshot("ingest"+salt, files...)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	rng := rand.New(rand.NewPCG(7, 11))
	words := []string{"handle", "parse", "render", "store", "fetch", "merge", "apply", "resolve", "encode", "walk"}
	rangeOf := func(i int) *model.SourceRange {
		return &model.SourceRange{Start: model.Position{Byte: uint64(i * 32), Line: uint32(i + 1)},
			End: model.Position{Byte: uint64(i*32 + 16), Line: uint32(i + 1), Column: 16}}
	}
	nodeOf := func(ff fileFixture, i int) (model.Node, string) {
		name := words[i%len(words)] + strconv.Itoa(i) + "_" + strconv.Itoa(int(ff.id[0]))
		key := model.CanonicalNodeKey(ff.path, name)
		id := model.NewNodeID(f.repo, model.NodeFunction, key)
		return model.Node{ID: id, Kind: model.NodeFunction, Language: "go", Name: name,
			QualifiedName: ff.path + "." + name, FileID: ff.id, ContentHash: ff.hash, Range: rangeOf(i)}, key
	}
	for ui, ff := range files {
		w := f.begin(gen, run, ff)
		facts := make([]model.NodeFact, 0, perUnit)
		aliases := make([]model.NativeAlias, 0, perUnit)
		docs := make([]model.SearchUnit, 0, perUnit)
		for i := 0; i < perUnit; i++ {
			node, key := nodeOf(ff, i)
			ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
				OriginRunID: run, NodeID: node.ID, Precision: model.PrecisionSyntax, FileID: ff.id,
				ContentHash: ff.hash, Range: rangeOf(i), NativeKey: ff.path + "#" + node.Name}
			ev.ID = model.NewEvidenceID(ev)
			facts = append(facts, model.NodeFact{Node: node, CanonicalKey: key, Evidence: []model.Evidence{ev}})
			aliases = append(aliases, model.NativeAlias{ScopeKey: ff.path, NativeKey: node.Name, NodeID: node.ID})
			docs = append(docs, model.SearchUnit{ID: model.H("ingest-doc", node.QualifiedName), NodeID: node.ID,
				FileID: ff.id, Path: ff.path, Kind: model.NodeFunction, Name: node.Name,
				QualifiedName: node.QualifiedName, Bytes: model.ByteRange{Start: uint64(i * 32), End: uint64(i*32 + 16)},
				Body: "func " + node.Name + "() { return " + words[(i+3)%len(words)] + " }", TokenCount: 8})
		}
		if err := w.PutNodes(f.ctx, facts); err != nil {
			t.Fatalf("PutNodes(%d): %v", ui, err)
		}
		rels := make([]model.RelationFact, 0, perUnit)
		for i := 0; i < perUnit; i++ {
			from, _ := nodeOf(ff, i)
			// Calls stay inside the unit (a relation endpoint must be a node
			// the unit sees); the identities are content hashes, so every
			// dictionary entry still lands at a random point of its index.
			to, _ := nodeOf(ff, rng.IntN(perUnit))
			rel := model.Relation{From: from.ID, Kind: model.RelCalls, To: to.ID}
			rel.ID = model.NewRelationID(f.repo, rel.From, rel.Kind, rel.To)
			ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
				OriginRunID: run, RelationID: rel.ID, Precision: model.PrecisionSyntax, FileID: ff.id,
				ContentHash: ff.hash, Range: rangeOf(i), NativeKey: ff.path + "#" + to.Name}
			ev.ID = model.NewEvidenceID(ev)
			rels = append(rels, model.RelationFact{Relation: rel, Evidence: []model.Evidence{ev}})
		}
		if err := w.PutRelations(f.ctx, rels); err != nil {
			t.Fatalf("PutRelations(%d): %v", ui, err)
		}
		if err := w.PutAliases(f.ctx, aliases); err != nil {
			t.Fatalf("PutAliases(%d): %v", ui, err)
		}
		if err := w.PutSearchUnits(f.ctx, docs); err != nil {
			t.Fatalf("PutSearchUnits(%d): %v", ui, err)
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit(%d): %v", ui, err)
		}
		if after != nil {
			after()
		}
	}
	return gen
}

// TestIngestionWritesAreProportionalToStoredBytes seals a repository's worth
// of file-scoped units the way an index run does -- every unit its own
// batches of nodes, relations, evidence, aliases and search documents, one
// unit after another -- and compares the bytes the process sent to disk with
// the bytes the store holds afterwards.
//
// Requirement: ingestion writes each stored byte a small constant number of
// times. A store file plus its log is written at most once each, so the bound
// admits the log copy, the checkpoint copy and the index entries for every
// row, and refuses a write path that rewrites pages it already wrote. Nothing
// below the bound is a tuning question: a write volume that scales with the
// number of units times the number of index pages, rather than with the bytes
// stored, saturates the host's disk on a large repository.
//
// The second figure is what the engine itself wrote (wchar), which the
// kernel's page cache can hide from the first: a group whose dirty pages
// outgrow the writer's page cache spills them to the log and rewrites the
// hot ones there in place, tens of times each, and every such write becomes
// disk traffic once the log is paced to disk. The engine's writes are bounded
// the same way as the disk's.
//
// Mutations that fail it: commit every batch on its own with hash-keyed
// indexes maintained per row (44.9x to disk); leave the engine's statement
// journal at its default 64 KiB threshold so every batch's savepoint
// journals to a file (15.5x written by the engine).
func TestIngestionWritesAreProportionalToStoredBytes(t *testing.T) {
	// A byte ratio, not a wall-clock one: host load cannot change it, so the
	// test runs on a loaded host too.
	units := envInt(t, "CODECTX_INGEST_UNITS", 600)
	perUnit := envInt(t, "CODECTX_INGEST_FACTS", 40)

	dir := t.TempDir()
	path := dir + "/ingest.db"
	before := procWriteBytes(t)
	engineBefore := procWriteChars(t)
	f := sealRepository(t, path, units, perUnit)
	// The measurement is of the durable outcome: the run's group commits and
	// the log is folded into the database, as it is at every activation.
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	written := procWriteBytes(t) - before
	engine := procWriteChars(t) - engineBefore
	var stored int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(path + suffix); err == nil {
			stored += st.Size()
		}
	}
	t.Logf("units=%d facts/unit=%d stored=%.1f MiB engine wrote=%.1f MiB (%.1fx) sent to disk=%.1f MiB (%.1fx)",
		units, perUnit, float64(stored)/(1<<20), float64(engine)/(1<<20), float64(engine)/float64(stored),
		float64(written)/(1<<20), float64(written)/float64(stored))
	if engine > 4*stored {
		t.Errorf("the engine wrote %.1f MiB for %.1f MiB stored (%.1fx): the group's pages are being spilled and rewritten in the log",
			float64(engine)/(1<<20), float64(stored)/(1<<20), float64(engine)/float64(stored))
	}
	if written == 0 {
		t.Skip("the temporary directory is not on a block device, so the bytes sent to disk cannot be read")
	}
	if written > 4*stored {
		t.Errorf("ingestion sent %.1f MiB to disk for %.1f MiB stored (%.1fx): the write path rewrites pages it already wrote",
			float64(written)/(1<<20), float64(stored)/(1<<20), float64(written)/float64(stored))
	}
}

// TestIngestionGroupCommitsWhenTheCacheWouldSpill gives the writer a page
// cache far smaller than the run and asserts that the run commits in groups
// the size of the cache: the log never holds more than the cache plus one
// batch.
//
// Requirement: an ingestion group is bounded by the writer's page cache, not
// by the log, because a group that outgrows the cache spills its dirty pages
// to the log and rewrites the hot ones there in place, and the pacer then
// carries every rewrite to the disk. The cache is the only memory the group
// takes, so this is also the run's memory bound.
//
// Mutation that fails it: bound the group by the log's size (1 GiB) and let
// the cache spill (the log reaches 18 MiB under a 2 MiB cache).
func TestIngestionGroupCommitsWhenTheCacheWouldSpill(t *testing.T) {
	const cacheKiB = 2048
	dir := t.TempDir()
	path := dir + "/ingest.db"
	f := newFixtureWithOptions(t, path, store.Options{WriterCacheKiB: cacheKiB})
	var peakLog int64
	sealRepositoryInto(t, f, 300, 40, "", func() {
		if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > peakLog {
			peakLog = st.Size()
		}
	})
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	stored := int64(0)
	if st, err := os.Stat(path); err == nil {
		stored = st.Size()
	}
	t.Logf("cache=%d KiB stored=%.1f MiB peak log=%.1f MiB", cacheKiB, float64(stored)/(1<<20), float64(peakLog)/(1<<20))
	// One batch of a 40-fact unit dirties well under a mebibyte; the bound
	// leaves room for the spill that triggers the commit and the frames the
	// commit itself appends.
	if bound := int64(2*cacheKiB) << 10; peakLog > bound {
		t.Errorf("the log reached %.1f MiB under a %d KiB writer cache: the group is not bounded by the cache", float64(peakLog)/(1<<20), cacheKiB)
	}
}

// TestTheActivationsCompactionCascadeIsBoundedLikeAnyOtherIngestion seals a
// first index's worth of units -- one segment per unit, so the geometric
// partitioning owes the generation a long cascade of merges -- and activates
// it while sampling the log.
//
// Requirement: the group's commit decision fires only at the end of an
// ingestion call, so a compaction cascade run inside the activation's own call
// would have no commit point and nothing in the group's accounting would bound
// its log; WALBoundBytes, which is stated as the largest log an ingestion group
// leaves behind, would not be a bound on an activation. A first index of a
// large repository owes thousands of merges. The merges therefore run as their
// own ingestion calls before the activation, which commits between them, and
// the activation itself only names the set they left.
//
// Mutation that fails it: run the merges inside the activation's call again
// (drop compactBeforeActivation and compact from buildLexical) and the
// activation commits once.
func TestTheActivationsCompactionCascadeIsBoundedLikeAnyOtherIngestion(t *testing.T) {
	const cacheKiB = 2048
	path := t.TempDir() + "/activate.db"
	f := newFixtureWithOptions(t, path, store.Options{WriterCacheKiB: cacheKiB})
	gen := sealRepositoryInto(t, f, envInt(t, "CODECTX_INGEST_UNITS", 600), envInt(t, "CODECTX_INGEST_FACTS", 40), "", nil)
	// The seals' own group is committed first, so what is counted below is the
	// activation's alone.
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	before := f.s.Commits()
	var peak int64
	done := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			if st, err := os.Stat(path + "-wal"); err == nil && st.Size() > peak {
				peak = st.Size()
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	f.activate(gen, 0)
	close(done)
	<-sampled
	commits := f.s.Commits() - before
	t.Logf("activation commits=%d peak log=%.1f MiB", commits, float64(peak)/(1<<20))
	if commits < 2 {
		t.Fatalf("the activation committed %d time(s): its compaction cascade never reached the group's commit decision, so nothing bounds its log", commits)
	}
	// The commit decision falls at the END of an ingestion call, so the frames
	// of the merge that crosses the bound are already in the log when the group
	// commits: a cascade run one merge per call peaks at the bound plus one
	// merge, never at the cascade. Measured on this fixture: 4.0 MiB bound,
	// 4.01 MiB peak with the merges as their own calls, 23.4 MiB with the
	// cascade inside the activation's one call -- and that figure grows with
	// the repository, which is what no bound would mean.
	if limit := 2 * f.s.WALBoundBytes(); peak > limit {
		t.Fatalf("the log reached %.1f MiB during the activation; one merge past the %.1f MiB group bound is %.1f MiB",
			float64(peak)/(1<<20), float64(f.s.WALBoundBytes())/(1<<20), float64(limit)/(1<<20))
	}
}

// TestAnActivationOvertakenByAnotherPaysNoMerges publishes one generation and
// then asks the store to publish a second while still naming the first
// generation's predecessor as the pointer it saw -- the loser of an activation
// race, deterministically.
//
// Requirement: the conditions an activation loses on are a pointer comparison
// and a membership count, one row each, while the compaction cascade the
// activation owes is thousands of merges, each its own committed and
// unrollbackable ingestion. A loser that pays the cascade before reading the
// row rebuilds a repository's segment set for an activation that was never
// going to happen -- work no operator asked for, on the disk the wave is
// about. The checks therefore run first, and the cascade is reached only by an
// activation that can still publish.
//
// Mutation that fails it: move the cascade back before the preflight (call
// compactBeforeActivation first) and the loser's merges insert segments before
// the version conflict is read.
func TestAnActivationOvertakenByAnotherPaysNoMerges(t *testing.T) {
	f := newFixture(t, t.TempDir()+"/overtaken.db")
	first := sealRepositoryInto(t, f, 40, 4, "", nil)
	f.activate(first, 0)
	second := sealRepositoryInto(t, f, 40, 4, " again", nil)
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	before, err := f.s.LexicalSegments(f.ctx)
	if err != nil {
		t.Fatalf("LexicalSegments: %v", err)
	}
	// Zero is the pointer this caller believes is published; the first
	// generation is, so this activation has already been overtaken.
	_, err = f.s.Activate(f.ctx, second, 0, model.HealthFresh, fixtureCaps, "norm-v1")
	if err == nil {
		t.Fatal("the overtaken activation published over a pointer it had not seen")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeVersionConflict {
		t.Fatalf("the overtaken activation failed with %v; want a version conflict", err)
	}
	after, err := f.s.LexicalSegments(f.ctx)
	if err != nil {
		t.Fatalf("LexicalSegments: %v", err)
	}
	if after != before {
		t.Fatalf("the overtaken activation merged %d segment(s) before reading the pointer it lost on "+
			"(segments %d -> %d)", after-before, before, after)
	}
}
