package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
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
// order and are matched by ID; outstanding requests are capped; a request
// abandoned by its context sends $/cancelRequest and stops waiting. The
// stream is an io.ReadWriter so the process wiring is independent of it.
type conn struct {
	reader   *bufio.Reader
	writer   io.Writer
	maxFrame int64
	handler  serverRequestHandler

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

	slots chan struct{}

	// replies queues answers to server-initiated requests for the one
	// dedicated writer goroutine. The reader must never write a reply itself:
	// a write parks on the child's stdin while the child is parked writing
	// stdout that only the reader drains, which deadlocks the pair and, while
	// the write lock is held, makes every call block before it can honour its
	// own deadline. The queue is small and a full queue drops the reply.
	replies chan message
	// done is closed by fail so the writer goroutine cannot outlive the
	// connection.
	done chan struct{}

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan reply
	failure error
}

// maxQueuedReplies bounds the answers to server-initiated requests waiting to
// be written. Four is more than any server has outstanding against a client
// that answers everything with a constant; beyond it the server is not
// waiting for an answer it can use.
const maxQueuedReplies = 4

// reply is what a waiting call receives: the server's response, or the typed
// failure that ended the connection while the call was outstanding.
type reply struct {
	msg message
	err error
}

func newConn(rw io.ReadWriter, maxFrame, maxWrite int64, maxOutstanding int, handler serverRequestHandler) *conn {
	return &conn{
		reader:   bufio.NewReaderSize(rw, 64<<10),
		writer:   rw,
		maxFrame: maxFrame,
		maxWrite: maxWrite,
		handler:  handler,
		slots:    make(chan struct{}, maxOutstanding),
		replies:  make(chan message, maxQueuedReplies),
		done:     make(chan struct{}),
		pending:  make(map[int64]chan reply),
	}
}

// run reads messages until the stream ends or a protocol error occurs. It
// returns the reason, which the owner uses to fail the connection.
func (c *conn) run() error {
	go c.writeReplies()
	for {
		payload, err := readFrame(c.reader, c.maxFrame)
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
			c.serveRequest(msg)
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
// hands the response to the writer goroutine. The handler decides; this
// method only frames. It never writes: it runs on the reader goroutine, and a
// reader that blocks on a write stops draining the server's output, which is
// exactly what the server is waiting on to drain the client's input.
func (c *conn) serveRequest(msg message) {
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
	select {
	case c.replies <- out:
	default:
		// The queue is full: the server is asking faster than its own stream
		// can carry the answers. Dropping is the bounded outcome; the server's
		// request timeout applies. Blocking here would deadlock the pair.
	}
}

// writeReplies is the one goroutine that writes answers to server-initiated
// requests. It exits only when the connection is latched failed, so a single
// unwritable reply cannot silently stop every later one: an oversized reply
// is refused after marshalling with nothing written, and the peer chooses the
// id and the method name that make it oversized. Such a reply is dropped and
// the loop continues; nothing waits on it and the server's own request
// timeout applies. A raw stream error latches nothing here, so the loop
// continues and parks on the select until the reader sees the stream end and
// the server's failure path closes done.
func (c *conn) writeReplies() {
	for {
		select {
		case msg := <-c.replies:
			if err := c.write(msg); err != nil && c.failed() != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

// call sends one request and waits for its response. It holds one of the
// outstanding-request slots for the duration, so at most cap(slots) requests
// are in flight. When ctx ends first the call sends $/cancelRequest, forgets
// the ID and returns: a deadline as CTX_PROVIDER_TIMEOUT, a cancellation as
// CTX_CANCELED. A response that arrives afterwards is dropped by deliver.
func (c *conn) call(ctx context.Context, method string, params any, result any) error {
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return contextError(ctx, method)
	}
	defer func() { <-c.slots }()

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

// write frames and sends one message under the writer lock, charging it
// against the rolling send budget. Exceeding the budget fails the connection:
// a truncated frame would leave the server mid-message.
//
// An outgoing message over the frame bound is refused before the lock and
// before the accounting: nothing was written, so the stream is still intact.
// The refusal is returned to the caller for a request and dropped by
// writeReplies for a reply — a reply carries the peer's id and method name, so
// the peer, not this client, decides whether one is oversized, and the
// connection stays usable either way. The bound is immutable and needs no lock.
func (c *conn) write(msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return outputInvalid("message cannot be encoded: %v", err)
	}
	if int64(len(payload)) > c.maxFrame {
		return resourceLimit("an outgoing %s message is %d bytes, over the %d-byte frame bound",
			methodOf(msg), len(payload), c.maxFrame).WithDetail("limit", "max_frame_bytes")
	}
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
	return writeFrame(c.writer, payload)
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

// methodOf names an outgoing message for a bounded error message; a response
// has no method of its own.
func methodOf(msg message) string {
	if msg.Method == "" {
		return "response"
	}
	return truncate(msg.Method, 64)
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
