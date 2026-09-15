//go:build linux

package sqlite_test

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// procWriteBytes reads the bytes this process has submitted to the block
// layer so far (write_bytes in /proc/self/io). It counts what reaches the
// disk, which is what saturates a host, and not what a file grew by.
func procWriteBytes(t *testing.T) int64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Skipf("/proc/self/io: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "write_bytes: "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				t.Fatalf("write_bytes: %v", err)
			}
			return n
		}
	}
	t.Fatal("/proc/self/io has no write_bytes line")
	return 0
}

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
// Mutation that fails it: commit every batch on its own with hash-keyed
// indexes maintained per row.
func TestIngestionWritesAreProportionalToStoredBytes(t *testing.T) {
	// A byte ratio, not a wall-clock one: host load cannot change it, so the
	// test runs on a loaded host too.
	units := envInt(t, "CODECTX_INGEST_UNITS", 600)
	perUnit := envInt(t, "CODECTX_INGEST_FACTS", 40)

	dir := t.TempDir()
	path := dir + "/ingest.db"
	f := newFixture(t, path)
	files := make([]fileFixture, units)
	for i := range files {
		files[i] = f.file(fmt.Sprintf("pkg%d/file%d.go", i%37, i), fmt.Sprintf("package p%d\n// unit %d\n%s", i%37, i, strings.Repeat("x", 32*perUnit+32)))
	}
	snap := f.snapshot("ingest", files...)
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

	before := procWriteBytes(t)
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
	}
	// The measurement is of the durable outcome: the run's group commits and
	// the log is folded into the database, as it is at every activation.
	if err := f.s.Flush(f.ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	written := procWriteBytes(t) - before
	if written == 0 {
		t.Skip("the temporary directory is not on a block device, so the bytes sent to disk cannot be read")
	}
	var stored int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(path + suffix); err == nil {
			stored += st.Size()
		}
	}
	ratio := float64(written) / float64(stored)
	t.Logf("units=%d facts/unit=%d stored=%.1f MiB written=%.1f MiB ratio=%.1fx",
		units, perUnit, float64(stored)/(1<<20), float64(written)/(1<<20), ratio)
	if written > 4*stored {
		t.Errorf("ingestion sent %.1f MiB to disk for %.1f MiB stored (%.1fx): the write path rewrites pages it already wrote",
			float64(written)/(1<<20), float64(stored)/(1<<20), ratio)
	}
}
