package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// A shallow Check must not walk the database. The invariant is not "it is
// faster" -- that is unassertable in a unit test -- but that the two
// whole-database statements do not run, and the only way to prove a statement
// did not run is to plant something that ONLY that statement sees.
//
// A referential violation is exactly that: `pragma_foreign_key_check` is the
// one statement in Check that reports it, and nothing else in the store or in
// the shallow header reads does. So a database carrying an orphan child row
// must pass Check(deep=false) and fail Check(deep=true). Re-enabling the
// foreign key check in the shallow path makes the first assertion fail; moving
// it out of the deep path makes the second fail.
func TestCheckShallowSkipsTheWholeDatabaseWalk(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	f := newFixture(t, path)

	// Plant the violation behind the store's back. Every connection the store
	// opens enforces foreign keys, so the row is written on a connection of
	// this test's own with enforcement off -- which is precisely the on-disk
	// state a crash or a corrupted page can leave and the check exists to find.
	flushed(t, f.s)
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO unit_inputs(unit_id, file_id, content_hash, executable) VALUES(999999, x'00', x'00', 0)`); err != nil {
		t.Fatalf("plant orphan unit_inputs row: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw connection: %v", err)
	}

	if err := f.s.Check(ctx, false); err != nil {
		t.Fatalf("shallow Check ran the referential walk and reported %v; shallow must read the header only", err)
	}
	if err := f.s.Check(ctx, true); err == nil {
		t.Fatal("deep Check passed a database with an orphan foreign key; --deep must still run the full walk")
	}
}

// The shallow half is not a no-op: it is the header, the schema fingerprint and
// the journal mode, and a database whose fingerprint is not this binary's must
// fail without --deep. Without this, "shallow is cheap" would be indistinguishable
// from "shallow checks nothing".
func TestCheckShallowStillFailsAForeignSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.db")
	f := newFixture(t, path)
	if err := f.s.Check(ctx, false); err != nil {
		t.Fatalf("shallow Check on a sound store: %v", err)
	}
	flushed(t, f.s)
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw connection: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE schema_meta SET fingerprint = 'not-this-binary' WHERE singleton = 1`); err != nil {
		t.Fatalf("rewrite fingerprint: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw connection: %v", err)
	}
	if err := f.s.Check(ctx, false); err == nil {
		t.Fatal("shallow Check passed a database written by another schema")
	}
}
