package ledger_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
)

// BenchmarkSpanStartEnd is what a stage pays to record itself: one Start and
// one End, which is two struct builds and two non-blocking sends. It is the
// figure that decides whether the ledger is free to the run, and a cost above
// noise here is a defect to report rather than a setting to add.
func BenchmarkSpanStartEnd(b *testing.B) {
	dir := b.TempDir()
	l, err := ledger.Open(context.Background(), dir)
	if err != nil {
		b.Fatalf("open the ledger: %v", err)
	}
	run, err := l.NewRun(ledger.KindIndex, repositoryID)
	if err != nil {
		b.Fatalf("new run: %v", err)
	}
	ctx := run.Context(context.Background())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, span := ledger.Start(ctx, "engine_unit", "scope")
		span.AddIn(1)
		span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	}
	b.StopTimer()
	run.Finish(ledger.OutcomeOK)
	if err := l.Stop(); err != nil {
		b.Fatalf("stop the ledger: %v", err)
	}
}

// BenchmarkCollectorRun is the collector's own write volume: a synthetic run
// of a few thousand spans, recorded end to end, reported as the rows and the
// bytes per second the ledger file receives. It is the second half of the cost
// question -- what the recording costs the disk, not the run's goroutines.
func BenchmarkCollectorRun(b *testing.B) {
	const spansPerRun = 4000
	for i := 0; i < b.N; i++ {
		dir := b.TempDir()
		start := time.Now()
		l, err := ledger.Open(context.Background(), dir)
		if err != nil {
			b.Fatalf("open the ledger: %v", err)
		}
		run, err := l.NewRun(ledger.KindIndex, repositoryID)
		if err != nil {
			b.Fatalf("new run: %v", err)
		}
		ctx := run.Context(context.Background())
		stageCtx, stage := ledger.Start(ctx, "structural_parse", "")
		for unit := 0; unit < spansPerRun-1; unit++ {
			_, span := ledger.StartProvider(stageCtx, "engine_unit", "scope", "structural")
			span.AddIn(1)
			span.AddOut(2)
			span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
		}
		stage.End(ledger.OutcomeOK, ledger.Measured{}, nil)
		run.Finish(ledger.OutcomeOK)
		if err := l.Stop(); err != nil {
			b.Fatalf("stop the ledger: %v", err)
		}
		elapsed := time.Since(start)
		bytes := fileBytes(b, ledger.Path(dir)) + fileBytes(b, ledger.Path(dir)+"-wal")
		b.ReportMetric(float64(spansPerRun)/elapsed.Seconds(), "rows/s")
		b.ReportMetric(float64(bytes)/elapsed.Seconds(), "bytes/s")
		b.ReportMetric(float64(bytes)/float64(spansPerRun), "bytes/span")
		// Events the bus refused: a producer this fast is the worst case the
		// bound was sized against, and the figure says how much of it a run
		// that never waits gives up.
		b.ReportMetric(float64(run.Dropped()), "dropped")
	}
}

// fileBytes sizes one file, reporting a missing write-ahead log as the zero
// bytes it genuinely is after a checkpoint and any other failure as a failure.
func fileBytes(b *testing.B, path string) int64 {
	b.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		b.Fatalf("size %s: %v", path, err)
	}
	return info.Size()
}
