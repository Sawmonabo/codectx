package app

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
)

// spanQueueDepth is how many finished stages may wait between the collector
// and the surfaces that render them. It is small on purpose: a stage is a
// thing that costs seconds, so a run produces these rows slowly, and a queue
// this deep is already more than any live surface is behind by. A depth that
// could not be reached would only hide a subscriber that had stopped reading.
const spanQueueDepth = 256

// spanFanout is the one hop between the ledger's collector goroutine and every
// surface that renders a finished stage: the structured log line, the
// command's progressive lines and the MCP server's notifications.
//
// It exists because a ledger subscriber runs ON the collector goroutine, which
// is the goroutine every live reader's view waits on, and the surfaces below
// write to terminals, sockets and log shippers. Publishing is therefore a
// single non-blocking send onto a bounded queue: a surface that cannot keep up
// loses rows and says so, and the run being measured is never paced by the
// accounting of it.
type spanFanout struct {
	rows    chan model.StageRecord
	done    chan struct{}
	dropped atomic.Int64

	mu   sync.Mutex
	subs []func(model.StageRecord)

	stopOnce sync.Once
}

func newSpanFanout() *spanFanout {
	f := &spanFanout{rows: make(chan model.StageRecord, spanQueueDepth), done: make(chan struct{})}
	go f.deliver()
	return f
}

// publish is what the ledger's collector calls. It never waits: a full queue
// drops the row and counts it, which is the same bargain the ledger's own
// event bus makes with the stages that record through it.
func (f *spanFanout) publish(span ledger.SpanRow) {
	select {
	case f.rows <- stageRecord(span):
	default:
		f.dropped.Add(1)
	}
}

// subscribe registers one surface. Subscribers are called in registration
// order on the fanout's own goroutine, so one that writes slowly delays the
// others and nothing else.
func (f *spanFanout) subscribe(fn func(model.StageRecord)) {
	if f == nil || fn == nil {
		return
	}
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	f.mu.Unlock()
}

func (f *spanFanout) deliver() {
	defer close(f.done)
	for row := range f.rows {
		f.mu.Lock()
		subs := f.subs
		f.mu.Unlock()
		for _, fn := range subs {
			fn(row)
		}
	}
}

// stop drains what is queued and reports what never reached a surface. It is
// called after the ledger has stopped, because stopping the ledger is what
// publishes the last finished spans.
func (f *spanFanout) stop(logger *slog.Logger) {
	if f == nil {
		return
	}
	f.stopOnce.Do(func() {
		close(f.rows)
		<-f.done
		// A drop is the one thing this queue can do that an operator would
		// otherwise never learn about: the rows are gone, and the log line and
		// the progressive lines they would have produced are simply missing.
		if dropped := f.dropped.Load(); dropped > 0 && logger != nil {
			logger.Warn("finished stages were dropped before they could be reported",
				"component", "ledger", "dropped", dropped)
		}
	})
}

// logSpan is the Section 4 structured line: one per finished stage, on the
// process's existing logger, with the recorded row's own fields as attributes.
// It is what a log shipper or a proof recorder reads, and it carries no source
// bytes, no environment and no analyzer output -- a stage name, a scope key
// and measurements.
func logSpan(logger *slog.Logger) func(model.StageRecord) {
	return func(row model.StageRecord) {
		attrs := []any{"component", "ledger", "run_id", row.RunID, "stage", row.Stage, "seq", row.Seq,
			"items_in", row.ItemsIn, "items_out", row.ItemsOut, "outcome", row.Outcome}
		// A stage that never ran -- a planned unit closed as unavailable when
		// the run ended -- has no finish and therefore no wall. Logging its
		// zero would report a measurement nobody took, and a reader cannot
		// tell that zero from a stage that genuinely cost nothing, so the
		// attribute is absent instead.
		if row.FinishedAt != nil {
			attrs = append(attrs, "wall_ms", row.WallMS)
		}
		if row.ScopeKey != "" {
			attrs = append(attrs, "scope_key", row.ScopeKey)
		}
		if row.Provider != "" {
			attrs = append(attrs, "provider", row.Provider)
		}
		if row.CPUUserMS != nil {
			attrs = append(attrs, "cpu_user_ms", *row.CPUUserMS)
		}
		if row.CPUSysMS != nil {
			attrs = append(attrs, "cpu_sys_ms", *row.CPUSysMS)
		}
		if row.CPUUnattributed != "" {
			attrs = append(attrs, "cpu_unattributed", row.CPUUnattributed)
		}
		if row.PeakRSSBytes != nil {
			attrs = append(attrs, "peak_rss_bytes", *row.PeakRSSBytes)
		}
		if row.DiagnosticCode != "" {
			attrs = append(attrs, "diagnostic_code", row.DiagnosticCode)
		}
		logger.Info("stage finished", attrs...)
	}
}

// stageRecord is the ONE conversion from a recorded span to the row every
// surface renders. The status table, the JSON, the command's progressive lines
// and the MCP notifications all come through here, so two surfaces can never
// disagree about what a run cost by mapping it differently.
func stageRecord(span ledger.SpanRow) model.StageRecord {
	return model.StageRecord{
		RunID:           span.RunID,
		Seq:             span.Seq,
		ParentSeq:       span.ParentSeq,
		Stage:           span.Stage,
		ScopeKey:        span.ScopeKey,
		Provider:        span.Provider,
		StartedAt:       span.StartedAt,
		FinishedAt:      span.FinishedAt,
		WallMS:          span.WallMS,
		Running:         span.Running,
		CPUUserMS:       span.CPUUserMS,
		CPUSysMS:        span.CPUSysMS,
		CPUUnattributed: span.CPUUnattributed,
		PeakRSSBytes:    span.PeakRSSBytes,
		ReadBytes:       span.ReadBytes,
		WriteBytes:      span.WriteBytes,
		ItemsIn:         span.ItemsIn,
		ItemsOut:        span.ItemsOut,
		Outcome:         string(span.Outcome),
		DiagnosticCode:  span.DiagnosticCode,
		Failure:         span.Failure,
		ShareOfWall:     span.ShareOfWall,
	}
}
