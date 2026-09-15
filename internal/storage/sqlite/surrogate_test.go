package sqlite_test

import (
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestSealedUnitRoundTripsCanonicalIdsThroughSurrogates is the one invariant
// the surrogate redesign can break silently. Every reference column is now an
// INTEGER into a dictionary table, and a reference bound to the WRONG row --
// the previous insert's rowid from a conflicting upsert, an endpoint resolved
// against the wrong canonical -- still satisfies every constraint, still seals,
// and still answers queries; it just answers them about another symbol.
//
// So this walks the store the way nothing else does: from the canonical ids the
// provider published, OUT through node_ids, relation_ids, scope_keys and
// native_keys, and asserts the fact rows come back. No other test joins the
// dictionaries (the delta tests compare surrogate to surrogate, which proves
// two units agree, not that either names the right identity) and none asserts
// that the canonical id survives at all, which is the ruling that makes the
// surrogate storage-internal: model.NodeID on the wire stays canonical.
func TestSealedUnitRoundTripsCanonicalIdsThroughSurrogates(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	b := f.file("pkg/b.go", "package pkg\nfunc G() {}\n")
	snap := f.snapshot("one", a, b)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScope(gen, run, configHash, a, b)
	f.fillFile(w, run, a)
	f.fillFile(w, run, b)
	from := f.putNode(w, run, "pkg/a.go", "Caller", &a, "key:caller")
	to := f.putNode(w, run, "pkg/b.go", "Callee", &b, "key:callee")
	f.putRelation(w, run, from, to, "key:edge", &a)
	if err := w.PutAliases(f.ctx, []model.NativeAlias{{ScopeKey: "pkg/a.go", NativeKey: "Caller", NodeID: from}}); err != nil {
		t.Fatalf("PutAliases: %v", err)
	}
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	unit := unitRowID(t, raw, w.UnitID())
	fromRaw, _ := model.DecodeID(string(from))
	toRaw, _ := model.DecodeID(string(to))
	relation := model.NewRelationID(f.repo, from, model.RelCalls, to)
	relRaw, _ := model.DecodeID(string(relation))

	// Each query starts at a canonical id or an interned string the provider
	// published and must land on exactly one row of the unit.
	for _, c := range []struct {
		what  string
		query string
		args  []any
	}{
		{"the node fact of a published identity", `SELECT count(*) FROM node_facts nf JOIN node_ids ni ON ni.id = nf.node_id
			WHERE nf.unit_id = ?1 AND ni.canonical = ?2 AND nf.name = 'Caller'`, []any{unit, fromRaw}},
		{"the relation fact of a published edge, with both endpoints", `SELECT count(*) FROM relation_facts rf
			JOIN relation_ids ri ON ri.id = rf.relation_id
			JOIN node_ids nf ON nf.id = ri.from_node_id JOIN node_ids nt ON nt.id = ri.to_node_id
			WHERE rf.unit_id = ?1 AND ri.canonical = ?2 AND nf.canonical = ?3 AND nt.canonical = ?4`,
			[]any{unit, relRaw, fromRaw, toRaw}},
		{"the alias of a published scope and native key", `SELECT count(*) FROM native_aliases na
			JOIN scope_keys sk ON sk.id = na.scope_key_id JOIN native_keys nk ON nk.id = na.native_key_id
			JOIN node_ids ni ON ni.id = na.node_id
			WHERE na.unit_id = ?1 AND sk.key = 'pkg/a.go' AND nk.key = 'Caller' AND ni.canonical = ?2`, []any{unit, fromRaw}},
		{"the delta key of a published fact", `SELECT count(*) FROM fact_keys fk JOIN node_ids ni ON ni.id = fk.node_id
			WHERE fk.unit_id = ?1 AND ni.canonical = ?2 AND fk.fact_key = ?3`, []any{unit, fromRaw, model.H("fixture-fact-key", "key:caller")}},
	} {
		var n int64
		if err := raw.QueryRow(c.query, c.args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		if n != 1 {
			t.Errorf("%s: joined back to %d rows, want exactly 1", c.what, n)
		}
	}

	// The canonical key is stored as the 32-byte digest it is, not as its
	// 64-character hex rendering (S-4), and it still derives the identity it is
	// registered under -- which is the whole reason the wire keeps canonical
	// ids while the store keeps surrogates.
	var key []byte
	var kind string
	if err := raw.QueryRow(`SELECT kind, canonical_key FROM node_ids WHERE canonical = ?`, fromRaw).
		Scan(&kind, &key); err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("canonical_key is %d bytes, want the 32-byte digest", len(key))
	}
	if got := model.NewNodeID(f.repo, model.NodeKind(kind), hex.EncodeToString(key)); got != from {
		t.Errorf("the stored kind and canonical key derive %s, want the published %s", got, from)
	}
}
