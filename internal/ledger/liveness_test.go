package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"slices"
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
	l := New(dir)
	err := l.Attach(ctx)
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
	lapsed := time.Now().Add(-LiveWindow)
	if err := l.current().writeTx(ctx, func(tx *sql.Tx) error {
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
		if err := l.current().db.QueryRowContext(context.Background(),
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

// TestSweepsOnlyRunsWhoseWriterIsGone protects the liveness predicate both
// sweeps share. The failure mode: a sweep with no predicate deletes the ledger
// of a process that is still running -- the overlay run of another process's
// language servers, or an index run that has not reached its generation yet --
// so the record of live work disappears from under the surface reading it.
//
// Mutation: drop `notLive` from either sweep's WHERE clause and the live
// overlay run is deleted here, which is a status surface reading a run this
// process is still writing.
func TestSweepsOnlyRunsWhoseWriterIsGone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l := New(dir)
	err := l.Attach(ctx)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := l.Stop(); err != nil {
			t.Errorf("stop the ledger: %v", err)
		}
	}()

	serving, err := l.NewRun(KindOverlay, liveRepository)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	abandoned, err := l.NewRun(KindOverlay, liveRepository)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	// A run row exists from its first span, so each of these records the one
	// span its kind is opened for.
	for _, run := range []*Run{serving, abandoned} {
		_, span := Start(run.Context(ctx), "server_start", "")
		span.End(OutcomeOK, Measured{}, nil)
	}
	// A tick that published nothing: it ended, and no generation will ever be
	// attached to it. It is inside the retained window, so it stays.
	barren, err := l.NewRun(KindDeferred, liveRepository)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	_, sealed := Start(barren.Context(ctx), "seal", "")
	sealed.End(OutcomeOK, Measured{}, nil)
	barren.Finish(OutcomeOK)
	waitForRuns(t, l, 3)
	// What a process killed while serving leaves behind: a deadline its writer
	// never renewed, on a row that still says 'running'.
	if err := l.current().writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE runs SET expires_at = ? WHERE run_id = ?`,
			formatTime(time.Now().Add(-LiveWindow)), abandoned.id)
		return err
	}); err != nil {
		t.Fatalf("backdate the abandoned run's liveness: %v", err)
	}

	ids, err := l.OverlayRuns(ctx)
	if err != nil {
		t.Fatalf("overlay runs: %v", err)
	}
	if len(ids) != 1 || ids[0] != abandoned.ID() {
		t.Fatalf("the sweep offers %v, want only the abandoned overlay run %s: "+
			"the overlay run of a process that is still serving must never be swept",
			ids, abandoned.ID())
	}
	if err := l.DeleteOverlayRuns(ctx, ids); err != nil {
		t.Fatalf("delete overlay runs: %v", err)
	}
	if err := l.SweepRuns(ctx, liveRepository, 0); err != nil {
		t.Fatalf("sweep the runs: %v", err)
	}
	left := runIDs(t, l)
	slices.Sort(left)
	want := []string{serving.ID(), barren.ID()}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("after both sweeps the ledger holds %v, want the live overlay run and the tick "+
			"that published nothing (%v): only the abandoned overlay run's writer is gone, and a "+
			"run inside the retained window keeps its account whether or not it reached a generation",
			left, want)
	}
}

// waitForRuns blocks until the collector has written n run rows; before that
// flush there is legitimately nothing in the file to sweep.
func waitForRuns(t *testing.T, l *Ledger, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * flushInterval)
	for {
		var runs int
		if err := l.current().db.QueryRowContext(context.Background(), `SELECT count(*) FROM runs`).Scan(&runs); err != nil {
			t.Fatalf("count the runs: %v", err)
		}
		if runs >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the collector wrote %d of %d run rows within %s", runs, n, 30*flushInterval)
		}
		time.Sleep(flushInterval / 5)
	}
}

// runIDs reads back the run ids the file still holds.
func runIDs(t *testing.T, l *Ledger) []string {
	t.Helper()
	rows, err := l.current().db.QueryContext(context.Background(), `SELECT run_id FROM runs ORDER BY started_at`)
	if err != nil {
		t.Fatalf("read the runs: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("read the runs: %v", err)
		}
		ids = append(ids, hex.EncodeToString(raw))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the runs: %v", err)
	}
	return ids
}

// TestARunsAccountOutlivesItsGeneration protects the one thing the ledger file
// exists for: that an operator who asks `status --resources` after a run is
// told what that run cost. A run's rows are about the run, not about the
// generation it published -- retention keeps only the newest generation of each
// ref, so the generation an index run activated is deleted the moment a
// deferred publication extends it on the same ref, seconds later and inside the
// same command.
//
// Mutation: restore a generation-keyed delete -- `DELETE FROM runs WHERE
// generation_id = ?` for each generation retention swept -- or key this sweep
// on generation_id at all, and the middle run below loses its spans while the
// store it built is still the active one.
//
// Mutation: drop the `generation_id <> ?` clause from SweepRuns and the active
// generation's run is swept once RetainedRuns newer runs exist, which is the
// same operator asking the same question and being told nothing.
func TestARunsAccountOutlivesItsGeneration(t *testing.T) {
	ctx := context.Background()
	l := New(t.TempDir())
	if err := l.Attach(ctx); err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() {
		if err := l.Stop(); err != nil {
			t.Errorf("stop the ledger: %v", err)
		}
	}()

	// One more run than the window retains, so the bound is actually exercised
	// rather than merely never reached.
	const total = RetainedRuns + 2
	ids := make([]string, 0, total)
	for i := range total {
		run, err := l.NewRun(KindIndex, liveRepository)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		_, span := Start(run.Context(ctx), "capture", "")
		span.End(OutcomeOK, Measured{}, nil)
		// Every run publishes a generation of its own, and every generation
		// but the first has been swept by retention by the time this sweep
		// runs: that is the ordinary shape of a workspace indexed twice.
		run.AttachGeneration(int64(i + 1))
		run.Finish(OutcomeOK)
		ids = append(ids, run.ID())
	}
	if err := l.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// The oldest run built the store that is active now. It is outside the
	// retained window and must survive anyway.
	if err := l.SweepRuns(ctx, liveRepository, 1); err != nil {
		t.Fatalf("sweep the runs: %v", err)
	}

	left := runIDs(t, l)
	want := append([]string{ids[0]}, ids[len(ids)-RetainedRuns:]...)
	slices.Sort(left)
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("after the sweep the ledger holds %v, want %v: the last %d runs and the run that "+
			"built the active generation, whichever generations retention has deleted",
			left, want, RetainedRuns)
	}
	// The run immediately inside the window is the reference case: its
	// generation is long gone and its account is still readable in full.
	reader, ok, err := OpenReader(ctx, l.dir)
	if err != nil || !ok {
		t.Fatalf("open the reader: %v (%v)", ok, err)
	}
	defer func() { _ = reader.Close() }()
	view, found, err := reader.Run(ctx, ids[len(ids)-1])
	if err != nil {
		t.Fatalf("read the run: %v", err)
	}
	if !found || len(view.Spans) != 1 {
		t.Fatalf("the newest run reads back as found=%v with %d spans, want it found with its one "+
			"span: a run whose generation is gone must still say what it cost", found, len(view.Spans))
	}
}
