package ledger_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Sawmonabo/codectx/internal/ledger"
)

// TestALedgerAnotherSchemaWroteIsReadAsAbsentAndReplacedByTheWriter protects
// the rule for a run ledger another build's schema wrote: it is derived
// accounting keyed by that build, so a reader holding no lock reads it as a
// workspace with no run recorded, and the writer, which holds the workspace
// lock, replaces it with an empty ledger of this schema and records into it.
//
// The fixture is a real ledger with a recorded run whose fingerprint row is
// then rewritten, the shape a build with another schema leaves behind.
//
// Mutations it catches: refusing the foreign file on Attach (the writer
// fails); a reader reporting it as recorded or as an error (OpenReader answers
// true or fails); Attach keeping the old rows instead of replacing the file
// (the old run is read back).
func TestALedgerAnotherSchemaWroteIsReadAsAbsentAndReplacedByTheWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l := ledger.New(dir)
	if err := l.Attach(ctx); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}
	run, err := l.NewRun(ledger.KindIndex, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	record(t, l, run)
	if err := l.Detach(); err != nil {
		t.Fatalf("close the ledger: %v", err)
	}
	if r, recorded, err := ledger.OpenReader(ctx, dir); err != nil || !recorded {
		t.Fatalf("the fixture ledger is not readable before the forgery: %v, %v", recorded, err)
	} else if _, found, err := r.Run(ctx, run.ID()); err != nil || !found {
		r.Close()
		t.Fatalf("the fixture's run is not recorded (found %v, %v)", found, err)
	} else {
		r.Close()
	}
	db, err := sql.Open("sqlite", ledger.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE ledger_meta SET fingerprint = 'a-build-this-one-is-not' WHERE singleton = 1`); err != nil {
		t.Fatalf("forge the fingerprint: %v", err)
	}

	if foreign, err := ledger.WrittenByAnotherSchema(ctx, dir); err != nil || !foreign {
		t.Fatalf("WrittenByAnotherSchema = %v, %v; want true", foreign, err)
	}
	if r, recorded, err := ledger.OpenReader(ctx, dir); err != nil || recorded {
		if r != nil {
			r.Close()
		}
		t.Fatalf("OpenReader over a foreign ledger = recorded %v, %v; want no run recorded and no error", recorded, err)
	}
	db.Close()

	again := ledger.New(dir)
	if err := again.Attach(ctx); err != nil {
		t.Fatalf("the writer must replace a foreign ledger, got %v", err)
	}
	fresh, err := again.NewRun(ledger.KindIndex, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	record(t, again, fresh)
	if err := again.Stop(); err != nil {
		t.Fatalf("close the replaced ledger: %v", err)
	}
	if foreign, err := ledger.WrittenByAnotherSchema(ctx, dir); err != nil || foreign {
		t.Fatalf("after the writer held it, WrittenByAnotherSchema = %v, %v; want false", foreign, err)
	}
	r, recorded, err := ledger.OpenReader(ctx, dir)
	if err != nil || !recorded {
		t.Fatalf("OpenReader after the replacement = %v, %v; want the new ledger", recorded, err)
	}
	defer r.Close()
	if _, found, err := r.Run(ctx, run.ID()); err != nil || found {
		t.Fatalf("the foreign ledger's run is still readable (found %v, %v): the file was not replaced", found, err)
	}
	if _, found, err := r.Run(ctx, fresh.ID()); err != nil || !found {
		t.Fatalf("the run recorded after the replacement is not readable (found %v, %v)", found, err)
	}
}

// record writes one stage of run and its finish, and flushes them.
func record(t *testing.T, l *ledger.Ledger, run *ledger.Run) {
	t.Helper()
	_, span := ledger.Start(run.Context(context.Background()), "capture", "")
	span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	run.Finish(ledger.OutcomeOK)
	if err := l.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
}
