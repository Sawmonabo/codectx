package ledger

import (
	"context"
	"database/sql"
	"time"
)

// collect is the one goroutine that writes. It drains the bus into a batch and
// commits the batch every flushInterval or every flushEvents, whichever comes
// first, snapshotting the counters of every span still running as it goes so a
// reader on another connection sees a live span advance. It exits when the bus
// closes, after a last flush that also closes whatever is still open.
//
// Nothing here returns an error to a caller: the run has already happened, and
// a ledger that cannot write must not take the run down with it. A failed
// flush drops that batch and the next one carries on; the rows a reader is
// missing are the honest consequence.
func (l *Ledger) collect() {
	defer close(l.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	batch := make([]event, 0, flushEvents)
	// running holds every span whose start has been written and whose end has
	// not, so each flush can snapshot its counters. A span whose end event was
	// dropped by a full bus stays here and is closed as interrupted at stop.
	running := map[*Span]struct{}{}

	for {
		select {
		case e, ok := <-l.bus:
			if !ok {
				l.flush(batch, running)
				l.finalize(running)
				return
			}
			batch = append(batch, e)
			if len(batch) >= flushEvents {
				l.flush(batch, running)
				batch = batch[:0]
			}
		case <-ticker.C:
			l.flush(batch, running)
			batch = batch[:0]
		}
	}
}

// flush writes one batch and the counter snapshot of every running span in one
// transaction, then publishes the spans that finished in it. Subscribers are
// called after the commit, so a subscriber never reports a row a reader could
// not yet see.
func (l *Ledger) flush(batch []event, running map[*Span]struct{}) {
	if len(batch) == 0 && len(running) == 0 && !l.anyDirty() {
		return
	}
	ctx := context.Background()
	var finished []SpanRow
	err := l.writeTx(ctx, func(tx *sql.Tx) error {
		for _, e := range batch {
			run := e.span.run
			if err := l.ensureRun(ctx, tx, run); err != nil {
				return err
			}
			switch e.kind {
			case eventStart:
				if err := insertSpan(ctx, tx, e.span); err != nil {
					return err
				}
				running[e.span] = struct{}{}
			case eventEnd:
				row, err := endSpan(ctx, tx, e)
				if err != nil {
					return err
				}
				delete(running, e.span)
				finished = append(finished, row)
			}
		}
		for span := range running {
			if _, err := tx.ExecContext(ctx,
				`UPDATE spans SET items_in = ?, items_out = ? WHERE run_id = ? AND seq = ?`,
				span.In(), span.Out(), span.run.id, span.seq); err != nil {
				return wrap("snapshot span counters", err)
			}
		}
		return l.updateRuns(ctx, tx)
	})
	if err != nil {
		return
	}
	for _, row := range finished {
		l.notify(row)
	}
}

// finalize is the last write of a stopping ledger: every span still open is
// closed as interrupted -- it never finished, so it gets no finish time and no
// wall, which would be a measurement nobody made -- and every run that did not
// say how it ended is interrupted too.
func (l *Ledger) finalize(running map[*Span]struct{}) {
	ctx := context.Background()
	now := time.Now()
	l.writeTx(ctx, func(tx *sql.Tx) error {
		l.runsMu.Lock()
		runs := append([]*Run(nil), l.runs...)
		l.runsMu.Unlock()
		for _, run := range runs {
			run.mu.Lock()
			if run.outcome == OutcomeRunning {
				run.outcome = OutcomeInterrupted
			}
			if run.finished == nil {
				finished := now
				run.finished = &finished
			}
			run.mu.Unlock()
			run.dirty.Store(true)
			if err := l.ensureRun(ctx, tx, run); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE spans SET outcome = 'interrupted' WHERE run_id = ? AND outcome = 'running'`, run.id); err != nil {
				return wrap("close open spans", err)
			}
		}
		return l.updateRuns(ctx, tx)
	})
	clear(running)
}

// ensureRun writes the run row the first time one of its spans is written, so
// a span never arrives before the row it references and nothing on the run's
// own goroutine ever waits for the database.
func (l *Ledger) ensureRun(ctx context.Context, tx *sql.Tx, run *Run) error {
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.inserted {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(
		run_id, kind, repository_id, generation_id, started_at, finished_at, outcome,
		file_count, source_bytes, units_planned, units_succeeded, units_failed, units_subdivided,
		events_dropped, process_peak_rss_bytes)
		VALUES(?, ?, ?, NULL, ?, NULL, 'running', 0, 0, 0, 0, 0, 0, 0, NULL)`,
		run.id, string(run.kind), run.repo, formatTime(run.started)); err != nil {
		return wrap("record the run", err)
	}
	run.inserted = true
	return nil
}

// updateRuns writes every known run's current totals. It runs on each flush
// because a live reader reads the run row as well as the spans: the counts a
// run has reached are as much of the live view as the spans are.
func (l *Ledger) updateRuns(ctx context.Context, tx *sql.Tx) error {
	l.runsMu.Lock()
	runs := append([]*Run(nil), l.runs...)
	l.runsMu.Unlock()
	for _, run := range runs {
		run.mu.Lock()
		inserted := run.inserted
		run.mu.Unlock()
		// The dirty flag is only cleared once there is a row to write it to,
		// so totals reported before the run's first span are not lost.
		if !inserted || !run.dirty.Swap(false) {
			continue
		}
		run.mu.Lock()
		var finished any
		if run.finished != nil {
			finished = formatTime(*run.finished)
		}
		var generation any
		if run.generationID != nil {
			generation = *run.generationID
		}
		var peak any
		if run.totals.ProcessPeakRSSBytes != nil {
			peak = int64(*run.totals.ProcessPeakRSSBytes)
		}
		t := run.totals
		outcome := run.outcome
		run.mu.Unlock()
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET
			generation_id = ?, finished_at = ?, outcome = ?,
			file_count = ?, source_bytes = ?, units_planned = ?, units_succeeded = ?,
			units_failed = ?, units_subdivided = ?, events_dropped = ?, process_peak_rss_bytes = ?
			WHERE run_id = ?`,
			generation, finished, string(outcome),
			t.FileCount, t.SourceBytes, t.UnitsPlanned, t.UnitsSucceeded,
			t.UnitsFailed, t.UnitsSubdivided, run.dropped.Load(), peak, run.id); err != nil {
			return wrap("update the run", err)
		}
	}
	return nil
}

// insertSpan writes a span's start. The parent is resolved in SQL from the
// parent's own ordinal: the bus is first-in-first-out and one goroutine drains
// it, so a parent's row is always already there, and neither side ever holds a
// row id. A span with no parent carries the ordinal -1, which no span can
// have, so the subselect finds nothing and the column is null.
func insertSpan(ctx context.Context, tx *sql.Tx, s *Span) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO spans(
		run_id, parent_id, seq, stage, scope_key, provider, started_at, finished_at,
		wall_ms, cpu_user_ms, cpu_sys_ms, cpu_unattributed, peak_rss_bytes, read_bytes, write_bytes,
		items_in, items_out, outcome, diagnostic_code, failure_json)
		VALUES(?, (SELECT id FROM spans WHERE run_id = ? AND seq = ?), ?, ?, ?, ?, ?, NULL,
		NULL, NULL, NULL, '', NULL, NULL, NULL, ?, ?, 'running', '', '')`,
		s.run.id, s.run.id, s.parent, s.seq, s.stage, s.scopeKey, s.provider, formatTime(s.started),
		s.In(), s.Out())
	return wrap("record a span", err)
}

// endSpan writes a span's end onto the row its start inserted and returns the
// row as it now stands, for the subscribers.
func endSpan(ctx context.Context, tx *sql.Tx, e event) (SpanRow, error) {
	s := e.span
	m := e.measured
	finished := formatTime(e.endWall)
	row := SpanRow{
		RunID:           s.run.idHex,
		Seq:             s.seq,
		Stage:           s.stage,
		ScopeKey:        s.scopeKey,
		Provider:        s.provider,
		StartedAt:       s.started,
		ItemsIn:         s.In(),
		ItemsOut:        s.Out(),
		Outcome:         e.outcome,
		CPUUnattributed: m.CPUUnattributed,
		DiagnosticCode:  m.DiagnosticCode,
		Failure:         m.Failure,
		CPUUserMS:       m.CPUUserMS,
		CPUSysMS:        m.CPUSysMS,
		PeakRSSBytes:    m.PeakRSSBytes,
		ReadBytes:       m.ReadBytes,
		WriteBytes:      m.WriteBytes,
	}
	end := e.endWall
	row.FinishedAt = &end
	wall := e.wallMS
	row.WallMS = &wall
	if s.parent >= 0 {
		parent := s.parent
		row.ParentSeq = &parent
	}
	_, err := tx.ExecContext(ctx, `UPDATE spans SET
		finished_at = ?, wall_ms = ?, cpu_user_ms = ?, cpu_sys_ms = ?, cpu_unattributed = ?,
		peak_rss_bytes = ?, read_bytes = ?, write_bytes = ?, items_in = ?, items_out = ?,
		outcome = ?, diagnostic_code = ?, failure_json = ?
		WHERE run_id = ? AND seq = ?`,
		finished, e.wallMS, nullInt(m.CPUUserMS), nullInt(m.CPUSysMS), m.CPUUnattributed,
		nullBytes(m.PeakRSSBytes), nullBytes(m.ReadBytes), nullBytes(m.WriteBytes), row.ItemsIn, row.ItemsOut,
		string(e.outcome), m.DiagnosticCode, m.Failure,
		s.run.id, s.seq)
	return row, wrap("close a span", err)
}

// nullInt and nullBytes render an absent measurement as SQL NULL rather than
// as a zero: a column this process could not fill says so.
func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullBytes(v *uint64) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}
