package filesystem_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// emitOne publishes one node with fields as its metadata through a real
// emitter and returns the fact the sink received.
func emitOne(t *testing.T, fields map[string]any) model.NodeFact {
	t.Helper()
	files := map[string]string{"package.json": "{}\n"}
	h := providertest.New(t, files)
	p, err := filesystem.New(filesystem.Options{})
	if err != nil {
		t.Fatal(err)
	}
	u := h.Plan(t, p, filesystem.ScopeKey("package.json"), []string{"package.json"})
	rec := &factSink{UnitOutput: h.Begin(t, u, []string{"package.json"})}
	fv := h.File(t, "package.json")
	e := filesystem.NewEmitter(u.Request, rec, fv, 0)
	e.Capability(filesystem.CapabilityStructure, model.CapabilityFresh, "")
	// The file node is minted the way the providers mint it, by path: a
	// candidate with no qualified name, range or descriptor resolves to a
	// provider-local unresolved entity, and its metadata then carries
	// resolution=unresolved beside the fields under test.
	cand := filesystem.PathCandidate("filesystem", model.NodeFile, "package.json")
	if _, err := e.Node(context.Background(), cand, filesystem.Attrs{Precision: model.PrecisionSyntax, Metadata: fields}); err != nil {
		t.Fatalf("Node: %v", err)
	}
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(rec.facts) != 1 {
		t.Fatalf("emitter published %d node facts, want 1", len(rec.facts))
	}
	return rec.facts[0]
}

// TestManifestMapPastTheBoundKeepsEveryEntryAndTheScalars pins the
// emitter's handling of a metadata payload past model.MaxMetadataBytes: a
// package.json `scripts` map of 400 entries. The scalar `version` stays inline,
// the map is stored as one row per entry with a `scripts_rows` marker, and
// the rows rebuild the map entry for entry in its own order.
// Mutation caught: replacing an over-bound payload whole (the scalars lost,
// as `{"truncated":true}` did), dropping or reordering entries, or omitting
// the marker a reader pages by.
func TestManifestMapPastTheBoundKeepsEveryEntryAndTheScalars(t *testing.T) {
	const n = 400
	scripts := map[string]string{}
	for i := range n {
		scripts[fmt.Sprintf("task%04d", i)] = fmt.Sprintf("node scripts/run.js --step %04d --verbose", i)
	}
	want, err := json.Marshal(scripts)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) <= model.MaxMetadataBytes {
		t.Fatalf("the fixture's scripts encode to %d bytes, which fits the %d-byte bound; it does not exercise it", len(want), model.MaxMetadataBytes)
	}
	fact := emitOne(t, map[string]any{"version": "1.2.3", "scripts": scripts})
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(fact.Node.Metadata, &meta); err != nil {
		t.Fatalf("node metadata %s: %v", fact.Node.Metadata, err)
	}
	if string(meta["version"]) != `"1.2.3"` {
		t.Errorf("version = %s, want the scalar kept inline", meta["version"])
	}
	if string(meta[model.MetadataRowsKey("scripts")]) != fmt.Sprint(n) {
		t.Errorf("scripts_rows = %s, want %d", meta[model.MetadataRowsKey("scripts")], n)
	}
	if _, inline := meta["scripts"]; inline {
		t.Errorf("scripts is inline in %d bytes of metadata; it does not fit", len(fact.Node.Metadata))
	}
	var rows []model.MetadataRow
	for _, ev := range fact.Evidence {
		if r, ok := model.ParseMetadataRow(ev.Detail); ok && r.Field == "scripts" {
			rows = append(rows, r)
		}
	}
	got, err := model.JoinMetadataRows(rows)
	if err != nil {
		t.Fatalf("join %d rows: %v", len(rows), err)
	}
	if string(got) != string(want) {
		t.Errorf("the rows rebuild %d bytes, want the %d-byte map entry for entry", len(got), len(want))
	}
}

// TestMetadataThatFitsWritesNoRows pins that metadata within the bound stays
// one inline object and the node carries only its occurrence.
// Mutation caught: storing every list as rows regardless of size, which
// costs an evidence row per element on every node.
func TestMetadataThatFitsWritesNoRows(t *testing.T) {
	fact := emitOne(t, map[string]any{"version": "1.2.3", "workspaces": []string{"a", "b"}})
	if string(fact.Node.Metadata) != `{"version":"1.2.3","workspaces":["a","b"]}` {
		t.Errorf("node metadata = %s, want the fields inline", fact.Node.Metadata)
	}
	if len(fact.Evidence) != 1 {
		t.Errorf("node carries %d evidence rows, want only its occurrence", len(fact.Evidence))
	}
}
