package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
)

// The three tests here guard the run ledger's rendered surfaces. They are a
// file of their own because the command table in root_test.go drives whole
// commands through a workspace, and none of these three invariants is about a
// command's envelope, exit code or error class.

// ledgerFixture is the smallest run that exercises every rendered case: a
// finished stage, a stage that is still going, and a stage that failed.
func ledgerFixture() (*model.RunRecord, []model.StageRecord) {
	finished := time.Date(2026, 3, 1, 10, 0, 12, 0, time.UTC)
	started := finished.Add(-12 * time.Second)
	generation := int64(7)
	run := &model.RunRecord{RunID: "a1b2", Kind: "index", RepositoryID: "c3d4", GenerationID: &generation,
		StartedAt: started, FinishedAt: &finished, WallMS: 12000, Outcome: "ok",
		FileCount: 120, UnitsPlanned: 3, UnitsSucceeded: 2, UnitsFailed: 1}
	stages := []model.StageRecord{
		{Seq: 0, Stage: "walk", StartedAt: started, FinishedAt: &finished, WallMS: 8000,
			Outcome: "ok", ItemsIn: 120, ItemsOut: 118, ShareOfWall: 0.666},
		{Seq: 1, Stage: "structural_parse", StartedAt: started, WallMS: 3000, Running: true,
			Outcome: "running", ItemsIn: 40, ShareOfWall: 0.25},
		{Seq: 2, Stage: "seal", StartedAt: started, FinishedAt: &finished, WallMS: 1000,
			Outcome: "failed", DiagnosticCode: "CTX_UNIT_FAILED", Failure: "the unit did not seal",
			ShareOfWall: 0.083},
	}
	return run, stages
}

// TestRunningStageNeverRendersLikeAFinishedOne guards the one invariant that
// makes the live view worth having: "this is taking a long time" and "this took
// a long time" must never render alike. A stalled stage whose elapsed time
// printed as a measured wall would tell an operator the stage had finished
// quickly, which is the reading they would act on and the one that is false.
//
// Mutation: render a running stage's wall as zero (wallMetric's running case).
func TestRunningStageNeverRendersLikeAFinishedOne(t *testing.T) {
	_, stages := ledgerFixture()
	running := wallMetric(stages[1].WallMS, stages[1].Running, stages[1].FinishedAt)
	finished := wallMetric(stages[0].WallMS, stages[0].Running, stages[0].FinishedAt)
	if !strings.HasPrefix(running, "running ") {
		t.Fatalf("a running stage rendered as %q, which does not say it is still going", running)
	}
	if strings.Contains(running, "0s") && !strings.Contains(running, "3s") {
		t.Fatalf("a running stage rendered as %q, hiding the time it has been going", running)
	}
	if running == finished || strings.HasPrefix(finished, "running") {
		t.Fatalf("a running stage rendered as %q and a finished one as %q; the two must not read alike",
			running, finished)
	}
	// A run whose end nothing measured is the third case: absent, never a
	// measured zero, because a run an operator is investigating must not read
	// as one that took no time.
	if got := wallMetric(0, false, nil); got != metricUnavailable {
		t.Fatalf("a run with no measured end rendered as %q, want %q", got, metricUnavailable)
	}
}

// TestTableAndJSONReportTheSameRows guards that two surfaces of one model
// cannot disagree. The rows are ordered once, where the report is assembled, so
// the table and the envelope carry the same stages in the same order with the
// same counts; a renderer that re-sorted would make "which stage cost the most"
// depend on which surface the operator happened to read.
//
// Mutation: reverse the stage order in writeRunLedger.
func TestTableAndJSONReportTheSameRows(t *testing.T) {
	run, stages := ledgerFixture()
	var b strings.Builder
	writeRunLedger(&b, run, stages, 0)
	table := b.String()

	raw, err := json.Marshal(struct {
		Run    *model.RunRecord    `json:"run"`
		Stages []model.StageRecord `json:"stages"`
	}{run, stages})
	if err != nil {
		t.Fatalf("marshal the same rows: %v", err)
	}
	var decoded struct {
		Run    *model.RunRecord    `json:"run"`
		Stages []model.StageRecord `json:"stages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the same rows: %v", err)
	}
	if len(decoded.Stages) != len(stages) {
		t.Fatalf("--json carries %d stages, the table was given %d", len(decoded.Stages), len(stages))
	}
	at := 0
	for _, stage := range decoded.Stages {
		next := strings.Index(table[at:], "  "+stage.Stage+"  ")
		if next < 0 {
			t.Fatalf("stage %q is in --json but not after position %d of the table:\n%s",
				stage.Stage, at, table)
		}
		at += next + len(stage.Stage)
	}
	// The totals the two surfaces report about the run itself, read back off
	// the rendered table rather than recomputed from the same values.
	for _, want := range []string{
		"generation 7", decoded.Run.Outcome,
		millisMetric(decoded.Run.WallMS),
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("the table does not report %q, which --json carries:\n%s", want, table)
		}
	}
	if !strings.Contains(table, "the unit did not seal") {
		t.Fatalf("a failed stage's reason reached --json but not the table:\n%s", table)
	}
}

// TestFollowEmitsOneWholeEnvelopePerInterval guards that a tailing script is
// never handed a partial snapshot: every pass emits one complete envelope that
// parses on its own, the first without waiting an interval, and the operator
// stopping the follow ends it cleanly rather than as a failure.
//
// Mutation: move followStatus's snapshot call after its select, so the first
// snapshot waits an interval.
func TestFollowEmitsOneWholeEnvelopePerInterval(t *testing.T) {
	// A follow that is stopped before its first interval has still answered
	// once: the report goes out before the wait, so a live view is never blank
	// for its first interval where it cannot be told from one that failed to
	// start.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	first := 0
	if err := followStatus(stopped, time.Hour, func() error { first++; return nil }); err != nil {
		t.Fatalf("a follow stopped before its first interval reported %v, want a clean end", err)
	}
	if first != 1 {
		t.Fatalf("a follow stopped before its first interval reported %d times, want 1", first)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	passes := 0
	err := followStatus(ctx, time.Millisecond, func() error {
		passes++
		if passes == 3 {
			cancel()
		}
		return writeEnvelope(&out, successEnvelope("1", "status",
			statusReport{Index: model.IndexStatus{Binding: model.Binding{GenerationID: 7}}}))
	})
	if err != nil {
		t.Fatalf("a follow the operator stopped reported %v, want a clean end", err)
	}
	if passes != 3 {
		t.Fatalf("the follow made %d passes, want 3", passes)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != passes {
		t.Fatalf("%d passes emitted %d lines; a tailing script reads one envelope per line", passes, len(lines))
	}
	for i, line := range lines {
		var env Envelope[statusReport]
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			t.Fatalf("envelope %d does not parse on its own: %v\n%s", i, err, line)
		}
		if !env.OK || env.Command != "status" || env.Data.Index.Binding.GenerationID != 7 {
			t.Fatalf("envelope %d is not a whole snapshot: %+v", i, env)
		}
	}
}

// TestTruncatedStagesPageSaysHowManyItDropped guards the invariant a bounded
// response rests on: a page that dropped rows never presents itself as the
// whole list. A run records one span per stage and one per unit, so a large
// repository's run outruns the per-result ceiling; an operator reading the
// shares and the costliest stage off a silently short table would be reading a
// list that is missing rows and could not tell.
//
// Both human surfaces are checked -- the status table and the index completion
// block -- against the same number the envelope carries, because a count that
// reached --json alone would leave the operator reading the terminal with the
// defect this closes.
//
// Mutation: drop the omitted count at the truncation site (return 0 from
// Reader.spans, or stop passing it into the two renderers).
func TestTruncatedStagesPageSaysHowManyItDropped(t *testing.T) {
	run, stages := ledgerFixture()
	const omitted = 417

	var table strings.Builder
	writeRunLedger(&table, run, stages, omitted)
	var completion strings.Builder
	writeIndexRunLedger(&completion, run, stages, omitted)

	for surface, rendered := range map[string]string{
		"the status table":           table.String(),
		"the index completion block": completion.String(),
	} {
		if !strings.Contains(rendered, "417") {
			t.Fatalf("%s dropped %d stages and does not say how many:\n%s", surface, omitted, rendered)
		}
		if !strings.Contains(rendered, "omitted") {
			t.Fatalf("%s names the number %d without saying it is stages it left out:\n%s",
				surface, omitted, rendered)
		}
	}

	// The same figure on the machine surface, read back off the wire rather
	// than off the value that was rendered.
	raw, err := json.Marshal(model.ResourceReport{Run: run, Stages: stages, StagesOmitted: omitted})
	if err != nil {
		t.Fatalf("marshal the report: %v", err)
	}
	var decoded model.ResourceReport
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode the report: %v", err)
	}
	if decoded.StagesOmitted != omitted {
		t.Fatalf("--json reports %d omitted stages, the table was given %d",
			decoded.StagesOmitted, omitted)
	}

	// A full page that dropped nothing says nothing, so "omitted" in the
	// output always means rows are actually missing.
	var whole strings.Builder
	writeRunLedger(&whole, run, stages, 0)
	if strings.Contains(whole.String(), "omitted") {
		t.Fatalf("a page that dropped nothing claims it omitted stages:\n%s", whole.String())
	}
}

// fakeStageSource is a workspace's two progressive-line inputs and nothing
// else: the stages of every run the process records, and the run this command
// opened -- which, like the real one, is not known when the subscription is
// made.
type fakeStageSource struct {
	emit  func(model.StageRecord)
	runID string
}

func (f *fakeStageSource) Spans(fn func(model.StageRecord)) { f.emit = fn }

func (f *fakeStageSource) IndexRunID() (string, bool) {
	if f.runID == "" {
		return "", false
	}
	return f.runID, true
}

// ledgerCommand is a human-output command writing into a buffer: the
// progressive lines take the command's own writer, so the buffer is what was
// printed.
func ledgerCommand() (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "index"}
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

// TestProgressiveLinesFollowOnlyThisCommandsRun guards the progressive block
// against two failures that look opposite and are both silent.
//
// The rows reach every subscriber from every run this process records, so a
// deferred run recorded by a late seal tick would print its stages into an
// index command's output, where an operator reads them as that command's work.
//
// The second failure is the fix's own: the run id does not exist when the
// subscription is made, so a filter that captures it once captures nothing and
// suppresses every line of the run it is meant to follow. That is why the run
// id here appears only after the subscription, exactly as it does in a command.
//
// The third is the mirror of the second: one index call can open a second run
// when an attempt re-captures after a version conflict, so an id latched from
// the first row suppresses every stage of the attempt that actually published.
//
// Mutation: capture the id at subscribe time -- bind runID from ws.IndexRunID()
// before ws.Spans and compare against it -- and the followed run's line is gone.
// Latching it on first use instead leaves the re-capture's stage unprinted.
func TestProgressiveLinesFollowOnlyThisCommandsRun(t *testing.T) {
	ws := &fakeStageSource{}
	cmd, out := ledgerCommand()
	stop := progressiveStages(cmd, nil, ws)
	// The run opens after the subscription, which is the ordering that breaks a
	// filter bound once.
	ws.runID = "mine"
	ws.emit(model.StageRecord{RunID: "mine", Stage: "walk", ItemsIn: 1, ItemsOut: 1})
	ws.emit(model.StageRecord{RunID: "another", Stage: "collection", ItemsIn: 9, ItemsOut: 9})
	// Not stopped yet: stop is a barrier that ends the block, and the third
	// case below is the same command still running.
	printed := out.String()
	if !strings.Contains(printed, "walk") {
		t.Fatalf("the followed run's stage is missing from the progressive lines:\n%s", printed)
	}
	if strings.Contains(printed, "collection") {
		t.Fatalf("another run's stage was printed as this command's work:\n%s", printed)
	}
	// The attempt re-captures: the same command is now publishing under a new
	// run, and it is that run whose stages the operator is waiting to see.
	ws.runID = "retry"
	ws.emit(model.StageRecord{RunID: "retry", Stage: "activation", ItemsIn: 2, ItemsOut: 2})
	ws.emit(model.StageRecord{RunID: "mine", Stage: "retention", ItemsIn: 3, ItemsOut: 3})
	stop()
	printed = out.String()
	if !strings.Contains(printed, "activation") {
		t.Fatalf("the re-captured run's stage is missing: an attempt that published printed nothing:\n%s", printed)
	}
	if strings.Contains(printed, "retention") {
		t.Fatalf("the abandoned attempt's stage was printed as this command's work:\n%s", printed)
	}
}

// TestTheWaitingLineFollowsTheHolderOnOneLine guards what a building command
// shows while it waits for another process to give up the workspace. The wait
// is now unbounded as long as the holder keeps working, so silence here is a
// command that looks hung for as long as an index takes, and a line per poll is
// thousands of lines scrolling everything else away.
//
// Mutation: drop the carriage return, or print the holder unconditionally
// rather than on change, and the single rewritten line becomes a transcript.
func TestTheWaitingLineFollowsTheHolderOnOneLine(t *testing.T) {
	cmd, out := ledgerCommand()
	report := waitingForHolder(cmd, nil)
	// The longer stage first, so the shorter one that follows it is the case
	// that leaves a tail behind on a terminal when the rewrite does not pad.
	report(app.WaitingHolder{Waiting: true, PID: 4321, Operation: "index", Stage: "publication"})
	report(app.WaitingHolder{Waiting: true, PID: 4321, Operation: "index", Stage: "capture"})
	report(app.WaitingHolder{})

	printed := out.String()
	for _, want := range []string{"4321", "index", "capture", "publication"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the waiting line must name the holder's pid, operation and each stage; %q is missing from %q", want, printed)
		}
	}
	// One line, rewritten: every stage but the first arrives behind a carriage
	// return, and the only newline is the one that closes the line when the
	// wait ends.
	if strings.Count(printed, "\n") != 1 || !strings.HasSuffix(printed, "\n") {
		t.Fatalf("the wait must print one line, closed once at the end, got %q", printed)
	}
	if strings.Count(printed, "\r") != 2 {
		t.Fatalf("each change must rewrite the line rather than add one, got %q", printed)
	}
	// The shorter stage must overwrite the whole of the longer one: the line
	// it leaves on the terminal is at least as wide as the line it replaced.
	rewrites := strings.Split(strings.TrimRight(printed, "\n"), "\r")
	if last := rewrites[len(rewrites)-1]; len(last) < len(rewrites[len(rewrites)-2]) {
		t.Fatalf("the rewritten line must not keep the previous stage's tail, got %q", printed)
	}
}
