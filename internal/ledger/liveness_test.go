package ledger

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// liveRepository is a fixed 32-byte identity in the wire shape every repository
// id has; the ledger never interprets it.
const liveRepository = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// TestLapsedRunLosesToTheActiveGeneration protects the liveness judgement in
// LatestRun. The failure mode: a process that dies without stopping its ledger
// leaves its run row saying 'running' for ever, so a reader that trusted that
// word would from then on show the dead run -- and its abandoned spans as if
// they were still working -- instead of the run that produced the active
// generation, precisely after the crash an operator is investigating.
func TestLapsedRunLosesToTheActiveGeneration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := l.Stop(); err != nil {
			t.Errorf("stop the ledger: %v", err)
		}
	}()

	const generation = int64(7)
	published, err := l.NewRun(KindIndex, liveRepository)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	_, sealed := Start(published.Context(ctx), "seal", "")
	sealed.End(OutcomeOK, Measured{}, nil)
	published.AttachGeneration(generation)
	published.Finish(OutcomeOK)

	// The run of a process that died: one span still open, no Finish, and --
	// below -- a liveness deadline its writer never renewed. It started after
	// the published run, so nothing but the liveness judgement can stop it
	// winning the query.
	crashed, err := l.NewRun(KindIndex, liveRepository)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	if _, parse := Start(crashed.Context(ctx), "structural_parse", "pkg/one"); parse == nil {
		t.Fatal("no span was opened")
	}
	waitForSpan(t, l, crashed)

	// What a writer that stopped refreshing leaves behind. The deadline is the
	// writer's own, so backdating the row is exactly the state a killed
	// process produces; the run's in-memory deadline stays ahead, so no later
	// flush renews it.
	lapsed := time.Now().Add(-liveWindow)
	if err := l.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET expires_at = ? WHERE run_id = ?`,
			formatTime(lapsed), crashed.id)
		return err
	}); err != nil {
		t.Fatalf("backdate the crashed run's liveness: %v", err)
	}

	reader, open, err := OpenReader(ctx, dir)
	if err != nil || !open {
		t.Fatalf("open the reader: %v (found %t)", err, open)
	}
	defer reader.Close()

	view, ok, err := reader.LatestRun(ctx, liveRepository, generation)
	if err != nil || !ok {
		t.Fatalf("latest run: %v (found %t)", err, ok)
	}
	if view.Run.RunID != published.ID() {
		t.Fatalf("the latest run is %s (%s), want the run that produced the active generation %s: "+
			"a run whose writer stopped refreshing still won the query",
			view.Run.RunID, view.Run.Outcome, published.ID())
	}

	// With no active generation the crashed run is what an operator is shown,
	// and it must read as cut off rather than as still working.
	view, ok, err = reader.LatestRun(ctx, liveRepository, 0)
	if err != nil || !ok {
		t.Fatalf("latest run: %v (found %t)", err, ok)
	}
	if view.Run.RunID != crashed.ID() || view.Run.Outcome != OutcomeInterrupted {
		t.Fatalf("the crashed run reads as %s (%s), want %s interrupted",
			view.Run.RunID, view.Run.Outcome, crashed.ID())
	}
	if len(view.Spans) != 1 {
		t.Fatalf("the crashed run reads back %d spans, recorded 1", len(view.Spans))
	}
	if span := view.Spans[0]; span.Outcome != OutcomeInterrupted || span.Running || span.WallMS != 0 {
		t.Fatalf("the span left open by the dead writer reads as %s (running %t, %d ms), "+
			"want interrupted with no measured wall", span.Outcome, span.Running, span.WallMS)
	}
}

// waitForSpan blocks until the collector has written run's first span, and so
// the run row it references. Before that flush there is legitimately nothing in
// the file to judge.
func waitForSpan(t *testing.T, l *Ledger, run *Run) {
	t.Helper()
	deadline := time.Now().Add(30 * flushInterval)
	for {
		var spans int
		if err := l.db.QueryRowContext(context.Background(),
			`SELECT count(*) FROM spans WHERE run_id = ?`, run.id).Scan(&spans); err != nil {
			t.Fatalf("count the run's spans: %v", err)
		}
		if spans > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector wrote no span within %s", 30*flushInterval)
		}
		time.Sleep(flushInterval / 5)
	}
}
