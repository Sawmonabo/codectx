package mcpserver

// The live view of an indexing run over the protocol: one progress
// notification per finished top-level stage while a client is driving a
// progress bar, and one log message per finished stage while a client is
// watching the stream.
//
// Both come from the SAME finished-stage rows the tools return and the CLI
// prints -- there is no second measurement here and nothing is timed in this
// file. The rows arrive through Options.Spans, are fanned out to the calls
// that are listening, and are sent from a goroutine of the call's own, never
// from the goroutine that publishes them: a publisher stalled behind a slow or
// absent client would delay every later row and with it every live reader.

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// progressInterval is the floor between two progress notifications for one
// call. The protocol asks a server not to flood a client with progress, and a
// stage is a thing that costs seconds, so a second is the resolution the rows
// themselves have. It is a package constant, not a configuration key.
const progressInterval = time.Second

// listenerDepth is how many rows one listening call may have queued before the
// next is dropped. The queue exists so that publishing never waits on a send;
// it is small because a dropped row costs a progress line and nothing else --
// the counter is taken at send time, so the value a client next sees is still
// the true number of finished stages, and the stage rows themselves are read
// back whole from the tool.
const listenerDepth = 64

// spanHub fans the one subscription this process holds out to the calls that
// are listening. Publishing is a non-blocking send per listener: the
// publisher's goroutine is the one that writes the rows every live reader
// waits on, and it must never be held up by a client.
type spanHub struct {
	mu        sync.Mutex
	next      int64
	listeners map[int64]chan<- model.StageRecord
}

func newSpanHub() *spanHub {
	return &spanHub{listeners: make(map[int64]chan<- model.StageRecord)}
}

// publish is what Options.Spans calls with every finished stage.
func (h *spanHub) publish(row model.StageRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.listeners {
		select {
		case ch <- row:
		default:
		}
	}
}

// listen adds one listener and returns the function that removes it. A hub
// with no listeners drops every row it is given, which is what a server whose
// clients asked for nothing costs.
func (h *spanHub) listen(ch chan<- model.StageRecord) (remove func()) {
	h.mu.Lock()
	id := h.next
	h.next++
	h.listeners[id] = ch
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.listeners, id)
		h.mu.Unlock()
	}
}

// reporter turns the rows one call listens to into notifications.
//
// token is the progress token the client passed in the request's _meta, and is
// nil when it passed none: a client that did not ask to follow progress gets
// no progress notifications and no work done on its behalf.
//
// The log message is NOT gated here. The level a client wants travels on the
// request the same way, and the session keeps it; which of the two channels a
// level arrived on, and whether it outranks info, is the session's own
// judgement and it is not readable from outside. Log applies it and sends
// nothing when no level was asked for, so a client that asked for none is
// charged one suppressed call per stage and no traffic.
type reporter struct {
	session *mcp.ServerSession
	token   any
	log     *slog.Logger
	// indexRun names the run this call is following. It is resolved on the
	// rows themselves rather than when the call starts, because the client's
	// progress token arrives before the coordinator has opened the run.
	indexRun func() (string, bool)

	rows chan model.StageRecord
	quit chan struct{}
	done chan struct{}

	// Sender-goroutine state, touched nowhere else.
	// runID is the run this call latched onto, empty until it has.
	runID    string
	finished float64
	sent     float64
	lastSent time.Time
}

// watchSpans starts reporting the run's finished stages to the client that
// made this call, and returns the function that stops it. It returns a no-op
// when this server was built without a span source, so a composition that
// records nothing needs no guard at the call site.
//
// The returned stop is called before the handler answers, and waits for the
// sending goroutine to be done, so no notification for a run can reach the
// client after the result that says the run is over.
func (h *handlers) watchSpans(ctx context.Context, req *mcp.CallToolRequest) (stop func()) {
	if h.spans == nil || req == nil || req.Session == nil {
		return func() {}
	}
	r := &reporter{
		session:  req.Session,
		token:    req.Params.GetProgressToken(),
		log:      h.log,
		indexRun: h.indexRun,
		rows:     make(chan model.StageRecord, listenerDepth),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	remove := h.spans.listen(r.rows)
	go r.run(ctx)
	return func() {
		remove()
		close(r.quit)
		<-r.done
	}
}

// run is the one goroutine that sends. It ends on stop, and on the call's own
// context, so a client that goes away mid-run takes its reporting with it.
func (r *reporter) run(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case <-r.quit:
			return
		case <-ctx.Done():
			return
		case row := <-r.rows:
			// Checked again immediately before sending: a row that was
			// already queued when the run ended must not be sent after it.
			select {
			case <-r.quit:
				return
			case <-ctx.Done():
				return
			default:
			}
			r.deliver(ctx, row)
		}
	}
}

// deliver counts one finished stage and sends what this client asked for.
func (r *reporter) deliver(ctx context.Context, row model.StageRecord) {
	// Only a stage the run opened at its top level moves the bar: a unit's
	// inner steps are rows of the same run, and counting them would make the
	// figure depend on how finely a provider happens to be instrumented. And
	// only a stage of the run this call is following: the rows arrive from
	// every run this process records.
	if row.ParentSeq == nil && r.following(row.RunID) {
		r.finished++
	}
	r.logRow(ctx, row)
	if r.token == nil {
		return
	}
	// Strictly increasing, and at most one per interval: the counter is read
	// here rather than when the row arrived, so the rate limit costs a
	// notification and never a count. A client driving a bar is therefore
	// never handed a value it has already seen or one that goes backwards.
	if r.finished <= r.sent {
		return
	}
	if !r.lastSent.IsZero() && time.Since(r.lastSent) < progressInterval {
		return
	}
	params := &mcp.ProgressNotificationParams{
		ProgressToken: r.token,
		Progress:      r.finished,
		Message:       progressMessage(row),
	}
	// Total is deliberately absent: the number of stages a run will open is
	// not known to a finished-stage row, and a total that is a guess is worse
	// than no total. Zero is omitted on the wire, which is the protocol's
	// spelling for "unknown".
	if err := r.session.NotifyProgress(ctx, params); err != nil {
		r.log.Debug("progress notification was not delivered", "detail", err.Error())
		return
	}
	r.sent = r.finished
	r.lastSent = time.Now()
}

// following reports whether a row belongs to the indexing run this call is
// following. The run is resolved from the process's ledger on the first row
// that arrives once one is open -- the call is made before the coordinator
// opens its run -- and then held for the rest of the call, so the rows this
// run publishes after it has finished still count and the overlay run's rows
// never latch it: an overlay run is not an indexing run, so the lookup never
// answers with one.
//
// Until a run is resolved nothing is counted. A bar that has not started is
// what a client sees for the moment before the run opens; counting rows that
// may belong to another run would be the untruth this exists to prevent.
func (r *reporter) following(runID string) bool {
	if r.runID == "" {
		id, ok := r.indexRun()
		if !ok || id == "" {
			return false
		}
		r.runID = id
	}
	return runID == r.runID
}

// logRow sends the finished stage as a log message, so an agent watching the
// stream sees what a human running the command sees. The row travels as
// structured data in the same shape the tools return it, never as a rendered
// line: a reader that wants the wall or the counts should not have to parse
// prose for them.
func (r *reporter) logRow(ctx context.Context, row model.StageRecord) {
	err := r.session.Log(ctx, &mcp.LoggingMessageParams{
		Level:  "info",
		Logger: "codectx.index",
		Data:   row,
	})
	if err != nil {
		r.log.Debug("stage log message was not delivered", "detail", err.Error())
	}
}

// progressMessage is the one line a client shows beside the bar: which stage
// finished, what it cost and what went through it.
func progressMessage(row model.StageRecord) string {
	name := row.Stage
	if row.ScopeKey != "" {
		name += " " + row.ScopeKey
	}
	return fmt.Sprintf("%s %s in %d ms, %d in, %d out",
		name, row.Outcome, row.WallMS, row.ItemsIn, row.ItemsOut)
}
