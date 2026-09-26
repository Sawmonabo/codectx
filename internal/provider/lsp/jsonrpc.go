package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// JSON-RPC 2.0 error codes the client interprets. Every other code from the
// server is reported as an unavailable answer with the code in the details.
const (
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	// rpcRequestCancelled is the server confirming a $/cancelRequest.
	rpcRequestCancelled = -32800
)

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// message is the one wire shape: a request has Method and ID, a notification
// has Method and no ID, a response has ID and one of Result or Error.
type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

// serverRequestHandler answers one server-initiated request. It returns the
// result to send or the error to send; it never performs an action on the
// caller's behalf (Section 11.5: never workspace/applyEdit, never a command).
type serverRequestHandler func(method string, params json.RawMessage) (any, *rpcError)

// conn is a JSON-RPC client over one byte stream. Responses arrive in any
// order and are matched by ID; outstanding requests are capped only when the
// user set providers.lsp.max_outstanding_requests; a request
// abandoned by its context sends $/cancelRequest and stops waiting. The
// stream is an io.ReadWriter so the process wiring is independent of it.
type conn struct {
	reader   *bufio.Reader
	writer   io.Writer
	maxFrame int64
	handler  serverRequestHandler

	// moved counts every byte this connection has read from or written to the
	// server. It is the connection-wide fallback progress signal, used only
	// where no processor-time signal exists; see progress.
	moved atomic.Int64

	// cpu is the child's live processor time, published by the runner that
	// started it. It is the signal that keeps a server computing an answer in
	// silence alive: bytes on the wire alone cannot tell that server from a
	// wedged one.
	cpu cpuProgress

	writeMu sync.Mutex
	// written counts the bytes sent inside the current window, and windowStart
	// is when that window opened. The budget is rolling rather than a lifetime
	// total: a session that lives for hours legitimately sends more bytes than
	// any one window, and charging them against one cap ended a healthy server
	// mid-flight. A flood still trips it, because a flood is bytes per unit of
	// time. Zero maxWrite is no budget at all, which is the default.
	written     int64
	windowStart time.Time
	maxWrite    int64

	// slots holds one token per in-flight request when the user bounded them,
	// and is nil when they are unbounded: every caller is then bounded by its
	// own gate (one request at a time per tool call, and the tool calls by the
	// query slots).
	slots chan struct{}

	// replies holds the encoded answers to server-initiated requests for the
	// one dedicated writer goroutine, and replyBytes their total size until
	// each is written. The reader must never write a reply itself: a write
	// parks on the child's stdin while the child is parked writing stdout that
	// only the reader drains, which deadlocks the pair and, while the write
	// lock is held, makes every call block before it can honour its own
	// deadline. A reply is never dropped: the queue is bounded by bytes, and a
	// server that asks for more than one frame's worth of answers it has not
	// read is not draining its input, which fails the connection (see
	// queueReply). replyWake tells the writer there is something to write.
	replyMu    sync.Mutex
	replies    [][]byte
	replyBytes int64
	replyWake  chan struct{}
	// done is closed by fail so the writer goroutine cannot outlive the
	// connection.
	done chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	failure error
}

// cpuProgress reports a running child's consumed processor time and whether
// it is observed. It is process.CPUProgress in the product -- the handle the
// runner binds to the sampler that already measures every child -- and one
// method is the whole of what the hang detector needs. ok is false where the
// platform cannot sample a running tree, and also until the sampler's first
// sweep has found this child's tree; the connection watches its byte count in
// that window (see progress).
type cpuProgress interface {
	Ticks() (int64, bool)
}

// reply is what a waiting call receives: the server's response, or the typed
// failure that ended the connection while the call was outstanding.
type reply struct {
	msg message
	err error
}

// newConn builds a connection. maxOutstanding bounds the requests in flight at
// once; zero is no bound.
func newConn(rw io.ReadWriter, maxFrame, maxWrite int64, maxOutstanding int,
	cpu cpuProgress, handler serverRequestHandler) *conn {

	c := &conn{
		reader:    bufio.NewReaderSize(rw, 64<<10),
		writer:    rw,
		maxFrame:  maxFrame,
		maxWrite:  maxWrite,
		cpu:       cpu,
		handler:   handler,
		replyWake: make(chan struct{}, 1),
		done:      make(chan struct{}),
		pending:   make(map[int64]chan reply),
	}
	if maxOutstanding > 0 {
		c.slots = make(chan struct{}, maxOutstanding)
	}
	return c
}

// run reads messages until the stream ends or a protocol error occurs. It
// returns the reason, which the owner uses to fail the connection.
func (c *conn) run() error {
	go c.writeReplies()
	for {
		payload, err := readFrame(c.reader, c.maxFrame)
		if len(payload) > 0 {
			c.moved.Add(int64(len(payload)))
		}
		if err != nil {
			return err
		}
		if err := checkDepth(payload); err != nil {
			return err
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			return outputInvalid("message is not a JSON-RPC object: %v", err)
		}
		switch {
		case msg.Method != "" && msg.ID != nil:
			if err := c.serveRequest(msg); err != nil {
				return err
			}
		case msg.Method != "":
			// Notifications (logMessage, publishDiagnostics, $/progress) carry
			// nothing this client acts on; they are read to keep the stream
			// moving and dropped.
		case msg.ID != nil:
			if err := c.deliver(msg); err != nil {
				return err
			}
		case msg.Error != nil:
			// A parse or invalid-request error the server could not attribute
			// to a request, spelled `"id": null` on the wire — encoding/json
			// nils the pointer for a JSON null, so it arrives here with no id.
			// There is nothing to route it to; the next call that fails
			// reports the reason.
		default:
			return outputInvalid("message has neither a method nor an id")
		}
	}
}

// deliver routes a response to its waiting call. A response to an unknown ID
// is legitimate after a cancellation, when the call has already given up, and
// is dropped; an ID that is not an integer was never issued by this client.
func (c *conn) deliver(msg message) error {
	var id int64
	if err := json.Unmarshal(*msg.ID, &id); err != nil {
		return outputInvalid("response id %s is not the integer this client issued", truncate(string(*msg.ID), 32))
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok {
		ch <- reply{msg: msg}
	}
	return nil
}

// serveRequest answers a server-initiated request through the handler and
// hands the encoded response to the writer goroutine. The handler decides;
// this method only frames. It never writes: it runs on the reader goroutine,
// and a reader that blocks on a write stops draining the server's output,
// which is exactly what the server is waiting on to drain the client's input.
//
// Every request is answered. An answer that does not fit one frame is
// replaced by an error answer that does, so the server is told rather than
// left waiting; a request whose id alone leaves no room for any answer, or a
// queue the server has stopped draining, fails the connection with the reason,
// so the calls waiting on it fail at once rather than at the stall timeout.
// The error is returned for the reader to end on.
func (c *conn) serveRequest(msg message) error {
	result, rpcErr := c.handler(msg.Method, msg.Params)
	out := message{JSONRPC: "2.0", ID: msg.ID}
	if rpcErr != nil {
		out.Error = rpcErr
	} else {
		raw, err := json.Marshal(result)
		if err != nil {
			out.Error = &rpcError{Code: rpcInvalidRequest, Message: "the client could not encode its response"}
		} else {
			out.Result = raw
		}
	}
	payload, err := json.Marshal(out)
	if err == nil && int64(len(payload)) > c.maxFrame {
		out.Result, out.Error = nil, &rpcError{Code: rpcInvalidRequest, Message: "the client's response does not fit one protocol message"}
		payload, err = json.Marshal(out)
	}
	switch {
	case err != nil:
		err = outputInvalid("the response to %s cannot be encoded: %v", truncate(msg.Method, 64), err)
	case int64(len(payload)) > c.maxFrame:
		err = resourceLimit("the language server sent a %s request whose id leaves no room for an answer within the %d-byte frame bound",
			truncate(msg.Method, 64), c.maxFrame).WithDetail("limit", "max_frame_bytes")
	default:
		err = c.queueReply(payload)
	}
	if err != nil {
		c.fail(err)
	}
	return err
}

// queueReply hands one encoded answer to the writer goroutine. The answers
// waiting to be written may together fill one protocol message: they are
// written as fast as the server reads its input, so a server holding more than
// a frame's worth of them unread is asking while it has stopped reading, and
// would wait forever on answers it can never receive.
func (c *conn) queueReply(payload []byte) error {
	c.replyMu.Lock()
	if c.replyBytes+int64(len(payload)) > c.maxFrame {
		queued := c.replyBytes
		c.replyMu.Unlock()
		return unavailable("the language server has %d bytes of answers to its own requests unread and asked for more; it is not reading its input", queued).
			WithDetail("reason", "not_draining")
	}
	c.replies = append(c.replies, payload)
	c.replyBytes += int64(len(payload))
	c.replyMu.Unlock()
	select {
	case c.replyWake <- struct{}{}:
	default:
	}
	return nil
}

// writeReplies is the one goroutine that writes answers to server-initiated
// requests, in the order they were asked. An answer's bytes stay counted
// against the queue until it is written. A write that fails fails the
// connection: an answer that cannot be delivered leaves the server waiting on
// it, so the conversation cannot continue. It exits when the connection is
// latched failed.
func (c *conn) writeReplies() {
	for {
		select {
		case <-c.replyWake:
		case <-c.done:
			return
		}
		for {
			c.replyMu.Lock()
			if len(c.replies) == 0 {
				c.replyMu.Unlock()
				break
			}
			payload := c.replies[0]
			c.replies[0] = nil
			c.replies = c.replies[1:]
			c.replyMu.Unlock()
			err := c.send(payload)
			c.replyMu.Lock()
			c.replyBytes -= int64(len(payload))
			c.replyMu.Unlock()
			if err != nil {
				c.fail(err)
				return
			}
		}
	}
}

// call sends one request and waits for its response. When requests are
// bounded it holds one of the outstanding-request slots for the duration, so
// at most cap(slots) requests are in flight. When ctx ends first the call sends $/cancelRequest, forgets
// the ID and returns: a deadline as CTX_PROVIDER_TIMEOUT, a cancellation as
// CTX_CANCELED. A response that arrives afterwards is dropped by deliver.
func (c *conn) call(ctx context.Context, method string, params any, result any) error {
	if c.slots != nil {
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return contextError(ctx, method)
		}
		defer func() { <-c.slots }()
	}

	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return c.failure
	}
	c.nextID++
	id := c.nextID
	ch := make(chan reply, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	raw, err := marshalParams(params)
	if err != nil {
		c.forget(id)
		return err
	}
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	if err := c.write(message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: raw}); err != nil {
		c.forget(id)
		return err
	}

	var got reply
	select {
	case got = <-ch:
	case <-ctx.Done():
		if c.forget(id) {
			// Best effort: the server may already be answering. Failure to
			// send the cancel is reported by the stream, not by this call.
			_ = c.notify("$/cancelRequest", cancelParams{ID: id})
			return contextError(ctx, method)
		}
		// The response landed between ctx ending and forget: use it.
		got = <-ch
	}
	if got.err != nil {
		return got.err
	}
	if got.msg.Error != nil {
		return serverError(method, got.msg.Error)
	}
	if result != nil && len(got.msg.Result) > 0 && string(got.msg.Result) != "null" {
		if err := json.Unmarshal(got.msg.Result, result); err != nil {
			return outputInvalid("%s returned a result of the wrong shape: %v", method, err)
		}
	}
	return nil
}

// notify sends one notification.
func (c *conn) notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(message{JSONRPC: "2.0", Method: method, Params: raw})
}

// forget removes a pending call, reporting whether it was still pending. A
// forgotten call's channel is never written to.
func (c *conn) forget(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.pending[id]
	delete(c.pending, id)
	return ok
}

// writeWindow is the period the rolling send budget is measured over. It is
// long enough that one burst of requests is charged together and short enough
// that a session is never ended for bytes it sent minutes ago.
const writeWindow = time.Minute

// write encodes and sends one request or notification. An outgoing message
// over the frame bound is refused before anything is written, so the stream is
// still intact and the refusal is the caller's. The bound is immutable and
// needs no lock. Answers to the server's own requests are sized when they are
// queued (serveRequest) and go straight to send.
func (c *conn) write(msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return outputInvalid("message cannot be encoded: %v", err)
	}
	if int64(len(payload)) > c.maxFrame {
		return resourceLimit("an outgoing %s message is %d bytes, over the %d-byte frame bound",
			truncate(msg.Method, 64), len(payload), c.maxFrame).WithDetail("limit", "max_frame_bytes")
	}
	return c.send(payload)
}

// send frames one encoded message under the writer lock, charging it against
// the rolling send budget. Exceeding the budget fails the connection: a
// truncated frame would leave the server mid-message.
func (c *conn) send(payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.failed(); err != nil {
		return err
	}
	if c.maxWrite > 0 {
		now := time.Now()
		if c.windowStart.IsZero() || now.Sub(c.windowStart) >= writeWindow {
			c.windowStart, c.written = now, 0
		}
		if c.written+int64(len(payload))+64 > c.maxWrite {
			err := resourceLimit("the language server has been sent %d bytes within %s; the %d-byte overlay bound is reached",
				c.written, writeWindow, c.maxWrite).
				WithDetail("limit", "max_overlay_bytes")
			c.fail(err)
			return err
		}
		c.written += int64(len(payload))
	}
	c.moved.Add(int64(len(payload)))
	return writeFrame(c.writer, payload)
}

// progress is the one number a request's hang detector watches: the processor
// time the language server child has consumed. A server computing an answer in
// silence is working, however long the project it is answering about takes,
// and only a child that is neither computing nor answering is wedged. This is
// the same signal the subprocess stall watchdog sums into its own -- the
// figure is sampled once, by that watchdog's sampler, and published here
// through process.CPUProgress, so there is one sampler and not two.
//
// It is deliberately NOT the connection's byte count. That count is
// per-connection while a hang is per-request, so a chatty sibling -- another
// request's answer, a diagnostics stream, a log line -- speaks for a request
// the server has wedged on and hides it for as long as the session lives.
// There is no per-request byte signal to put in its place: this client sends
// no partial-result or work-done token, so the protocol attributes nothing to
// a request before its answer, and the answer itself ends the wait.
//
// Two limits, stated because the parity with the subprocess watchdog has to be
// honest rather than claimed. Processor time is the whole child's, so a
// sibling request burning CPU still speaks for a wedged one; the protocol
// carries no per-request resource account and the signal cannot be narrower
// than the process. And a server that emits frames while consuming less than
// one clock tick of processor time for a whole window reads as wedged; that
// costs a live but effectively idle session, where counting its bytes would
// cost every wedged request on a busy connection.
//
// Where there is no processor-time reading -- a platform that cannot sample a
// running tree, or a child the sampler's first sweep has not found yet, which
// is the start of every server and so the start of the initialize request --
// the connection's byte count stands in. The fallback restores exactly the
// masking above, which is the right trade for it: it cannot terminate a server
// that is progressing, and while it applies there is nothing else to watch.
// The first processor-time reading after the fallback differs from the byte
// count it replaces and so reads as one change, which only ever counts as
// progress once and never as a stall.
func (c *conn) progress() int64 {
	if c.cpu != nil {
		if ticks, ok := c.cpu.Ticks(); ok {
			return ticks
		}
	}
	return c.moved.Load()
}

// watchProgress returns a context that ends when the request being watched has
// made no progress for window, a predicate that reports whether it was the
// detector that ended it, and a stop function that must be called on every
// path. A zero or negative window is no detector at all and hands the caller's
// own context straight back.
//
// The predicate is what keeps the outcome honest: the context ends by
// cancellation either way, and without it a hung server would be reported as
// the caller cancelling.
func (c *conn) watchProgress(ctx context.Context, window time.Duration) (context.Context, func() bool, context.CancelFunc) {
	if window <= 0 {
		return ctx, func() bool { return false }, func() {}
	}
	var stalled atomic.Bool
	watched, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	go func() {
		// Sampling at the window itself would let a server that went quiet just
		// after a sample survive for nearly twice it; a quarter of the window
		// bounds that overshoot the way the subprocess watchdog does.
		poll := max(window/4, 10*time.Millisecond)
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		last, quietSince := c.progress(), time.Now()
		for {
			select {
			case <-stop:
				return
			case <-watched.Done():
				return
			case now := <-ticker.C:
				if seen := c.progress(); seen != last {
					last, quietSince = seen, now
					continue
				}
				if now.Sub(quietSince) >= window {
					stalled.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return watched, stalled.Load, func() {
		close(stop)
		cancel()
	}
}

// fail latches the connection and releases every pending call with err. It
// is idempotent; the first reason wins. The latch is what makes close(c.done)
// happen exactly once, so the close must stay inside this critical section,
// after the early return: a second close would panic where a second map swap
// would not.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.failure != nil {
		c.mu.Unlock()
		return
	}
	c.failure = err
	pending := c.pending
	c.pending = make(map[int64]chan reply)
	close(c.done)
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: err}
	}
}

// failed reports the latched failure, if any.
func (c *conn) failed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failure
}

type cancelParams struct {
	ID int64 `json:"id"`
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, outputInvalid("request parameters cannot be encoded: %v", err)
	}
	return raw, nil
}

// contextError types a call abandoned by its context.
func contextError(ctx context.Context, method string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return timedOut("%s did not answer before its deadline", method).WithDetail("method", method)
	}
	return model.Canceled(ctx.Err())
}

// serverError types a JSON-RPC error the server returned. MethodNotFound is
// the honest "this capability is unavailable"; RequestCancelled confirms a
// cancel this client sent; anything else is the server declining this
// request, reported as unavailable with the server's bounded code and
// message, never as a substituted answer.
func serverError(method string, e *rpcError) error {
	switch e.Code {
	case rpcMethodNotFound:
		return unavailable("the language server does not implement %s", method).
			WithDetail("method", method).WithDetail("reason", "method_not_found")
	case rpcRequestCancelled:
		return model.Canceled(context.Canceled)
	}
	return unavailable("the language server declined %s: %s", method, truncate(e.Message, 200)).
		WithDetail("method", method).WithDetail("server_code", strconv.FormatInt(e.Code, 10))
}
