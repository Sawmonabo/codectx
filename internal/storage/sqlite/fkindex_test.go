package sqlite

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

// foreignKey is one foreign-key constraint of the schema: the child table, its
// child columns in key order and the parent table.
type foreignKey struct {
	child  string
	cols   []string
	parent string
}

func schemaForeignKeys(t *testing.T, tx *sql.Tx) []foreignKey {
	t.Helper()
	tables := queryStrings(t, tx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		AND sql NOT LIKE 'CREATE VIRTUAL%'`)
	var out []foreignKey
	for _, table := range tables {
		rows, err := tx.Query(`SELECT id, seq, "table", "from" FROM pragma_foreign_key_list(?) ORDER BY id, seq`, table)
		if err != nil {
			t.Fatalf("foreign keys of %s: %v", table, err)
		}
		byID := map[int]*foreignKey{}
		var order []int
		for rows.Next() {
			var id, seq int
			var parent, from string
			if err := rows.Scan(&id, &seq, &parent, &from); err != nil {
				t.Fatal(err)
			}
			fk, ok := byID[id]
			if !ok {
				fk = &foreignKey{child: table, parent: parent}
				byID[id] = fk
				order = append(order, id)
			}
			fk.cols = append(fk.cols, from)
		}
		rows.Close()
		for _, id := range order {
			out = append(out, *byID[id])
		}
	}
	return out
}

func queryStrings(t *testing.T, tx *sql.Tx, query string, args ...any) []string {
	t.Helper()
	rows, err := tx.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// leadingColumns is, for every b-tree of table that can serve a lookup, its
// first column: each index's (automatic ones included), the primary key's of a
// rowid-less table, and the rowid alias of a rowid table.
func leadingColumns(t *testing.T, tx *sql.Tx, table string) []string {
	t.Helper()
	var out []string
	for _, index := range queryStrings(t, tx, `SELECT name FROM pragma_index_list(?)`, table) {
		out = append(out, queryStrings(t, tx, `SELECT name FROM pragma_index_info(?) WHERE seqno = 0`, index)...)
	}
	out = append(out, queryStrings(t, tx, `SELECT name FROM pragma_table_info(?) WHERE pk = 1`, table)...)
	return out
}

// TestEveryForeignKeyChildKeyIsIndexed holds the schema to the rule its
// foreign keys need: every connection runs with foreign_keys=ON, so deleting a
// parent row first proves no child row names it, and a child key no index
// leads with makes that proof a scan of the whole child table per deleted row
// -- a cost invisible in the deleting statement's own plan. Every child key
// must have a b-tree whose first column is one of its columns.
//
// mutation: dropping any child-key index the schema declares for this --
// idx_snapshot_files_file, idx_snapshot_files_blob, idx_unit_input_blob,
// idx_session_files_snapshot, idx_lease_snapshot, idx_lease_generation,
// idx_provider_runs_generation, idx_units_origin_run,
// idx_context_manifests_generation, idx_read_sessions_manifest,
// idx_session_manifests_manifest, or any older one -- names its key here.
func TestEveryForeignKeyChildKeyIsIndexed(t *testing.T) {
	_, _, tx := internFixture(t)
	fks := schemaForeignKeys(t, tx)
	if len(fks) == 0 {
		t.Fatal("the schema reports no foreign keys; the check would pass vacuously")
	}
	for _, fk := range fks {
		leading := leadingColumns(t, tx, fk.child)
		if !slices.ContainsFunc(fk.cols, func(c string) bool { return slices.Contains(leading, c) }) {
			t.Errorf("%s(%s) -> %s: no index leads with a child-key column, so every delete from %s scans %s",
				fk.child, strings.Join(fk.cols, ", "), fk.parent, fk.parent, fk.child)
		}
	}
}

// TestOrphanFileSweepTermsCoverEveryFileReference proves the orphan-file
// sweep's reachability (unreferencedFile) asks every table that can hold a
// file id: each such table is either one of its three terms or reaches one of
// them through an enforced foreign key on that same file id. A table that
// names a file by value alone would let the sweep delete a file it still
// names.
//
// mutation: removing a term from unreferencedFile, dropping evidence's,
// node_facts', search_units' or session_files' foreign key onto the table that
// implies it, or adding a table with a file_id column and neither, fails here.
func TestOrphanFileSweepTermsCoverEveryFileReference(t *testing.T) {
	_, _, tx := internFixture(t)
	terms := map[string]bool{}
	for _, table := range []string{"snapshot_files", "unit_inputs", "context_entries"} {
		if strings.Contains(unreferencedFile, "FROM "+table+" ") {
			terms[table] = true
		}
	}
	if len(terms) != 3 {
		t.Fatalf("unreferencedFile asks %d of snapshot_files, unit_inputs and context_entries, want all three", len(terms))
	}
	// A table is covered when a foreign key on its file id reaches a term or
	// a table already covered: session_files' children reach snapshot_files
	// through session_files.
	fks := schemaForeignKeys(t, tx)
	implied := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, fk := range fks {
			if !implied[fk.child] && (terms[fk.parent] || implied[fk.parent]) && slices.Contains(fk.cols, "file_id") {
				implied[fk.child], grew = true, true
			}
		}
	}
	holders := queryStrings(t, tx, `SELECT m.name FROM sqlite_schema m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' AND m.sql NOT LIKE 'CREATE VIRTUAL%' AND c.name = 'file_id'`)
	if len(holders) < 4 {
		t.Fatalf("the schema reports %d tables with a file_id column; the check would pass vacuously", len(holders))
	}
	for _, table := range holders {
		if !terms[table] && !implied[table] {
			t.Errorf("%s holds a file_id that the orphan-file sweep neither asks about nor reaches through a foreign key", table)
		}
	}
}
