package process

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// drainBufferBytes is the fixed read buffer one stream uses. It is not a
// reservation: the captured bytes are what the limit bounds.
const drainBufferBytes = 32 * 1024

// streamPipe drains one of the child's output streams.
//
// The pipe is created here rather than by os/exec so that this package owns
// both ends. That matters on the termination path: a descendant that has not
// yet exited still holds the write end, and closing the read end is the only
// way to unblock the drain without waiting for it.
//
// The drain never stops reading and never stops the child. Stopping would
// leave the child blocked writing into a full pipe, where it could neither
// make progress nor notice that it is being asked to stop, and a stream that
// produced more bytes than expected is a large run, not a wedged one.
//
// The limit bounds the capture buffer and nothing else, because the capture
// buffer is the only thing this package allocates per byte. It applies only
// when sink is nil: a caller-supplied writer takes every byte, and its own
// blocking Write is the backpressure that bounds the run -- dropping bytes
// from a framed protocol such as JSON-RPC or the parser worker's stream would
// corrupt it rather than degrade it. A zero or negative limit is no bound at
// all, which is what a discarded stream wants.
type streamPipe struct {
	r, w *os.File
	sink io.Writer
	// capture holds the bytes when the caller supplied no writer.
	capture bytes.Buffer
	// limit bounds capture only; see the type comment. Zero or negative, and
	// any value at all when sink is non-nil, means unbounded.
	limit int64

	// progress counts every byte the drain has read from the pipe, whether it
	// was delivered, discarded past the limit, or written to a caller's sink.
	// It is the stall detector's evidence that the child is alive, so it must
	// count raw reads: delivered stops advancing once the limit is reached, and
	// a stream pointed at io.Discard delivers nothing anyone can observe. It is
	// atomic because the watchdog reads it while the drain runs; every other
	// field here stays drain-only.
	progress atomic.Int64

	// delivered and truncated are written only by drain and read only after it
	// has finished.
	delivered int64
	truncated bool
	failure   error

	closeWriterOnce sync.Once
	closeReaderOnce sync.Once
	// readerClosed records that the read end was closed deliberately, so the
	// resulting read error is not mistaken for a lost stream.
	readerClosed atomic.Bool
}

// newStreamPipe creates one stream's pipe and points the command's stream at
// its write end. sink may be nil, in which case the bytes are captured.
func newStreamPipe(sink io.Writer, limit int64, target *io.Writer) (*streamPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, internalError("an output pipe cannot be created: %v", err)
	}
	p := &streamPipe{r: r, w: w, sink: sink, limit: limit}
	// Assigning an *os.File makes os/exec hand the descriptor to the child
	// directly, with no copying goroutine of its own to wait for.
	*target = w
	return p, nil
}

func (p *streamPipe) drain() {
	buf := make([]byte, drainBufferBytes)
	bounded := p.sink == nil && p.limit > 0
	for {
		n, err := p.r.Read(buf)
		if n > 0 {
			p.progress.Add(int64(n))
			take := int64(n)
			if bounded {
				// Only a capture buffer is bounded, so the arithmetic below is
				// never reached for a caller's writer and an unlimited capture
				// never computes a negative remainder from the sentinel.
				if remaining := p.limit - p.delivered; take > remaining {
					take = remaining
					p.truncated = true
				}
			}
			if take > 0 {
				if werr := p.write(buf[:take]); werr != nil && p.failure == nil {
					p.failure = werr
				}
				p.delivered += take
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !p.readerClosed.Load() && p.failure == nil {
				// End of file means the writers are gone, and a deliberate
				// close is the runner giving up on a surviving descendant.
				// Anything else means output was lost without anyone noticing.
				p.failure = internalError("the child's output stream could not be read: %v", err)
			}
			return
		}
	}
}

func (p *streamPipe) write(b []byte) error {
	if p.sink == nil {
		p.capture.Write(b)
		return nil
	}
	if _, err := p.sink.Write(b); err != nil {
		return internalError("the output destination rejected %d bytes: %v", len(b), err)
	}
	return nil
}

// captured returns the captured bytes, which are empty when the caller supplied
// its own writer, and the number of bytes delivered.
func (p *streamPipe) captured() ([]byte, int64) {
	if p.sink != nil {
		return nil, p.delivered
	}
	return p.capture.Bytes(), p.delivered
}

// limited reports that bytes the child produced were dropped rather than
// captured. It is never a reason to fail the run: the caller reports it, and
// what it holds is a complete prefix of the stream.
func (p *streamPipe) limited() bool { return p.truncated }

// progressed reports the raw bytes read from the pipe so far. It is safe to
// call while the drain is running, which is the only reason it exists.
func (p *streamPipe) progressed() int64 { return p.progress.Load() }

func (p *streamPipe) err() error { return p.failure }

// closeWriter releases the parent's copy of the write end. Until it is closed
// the drain cannot see end of file, however promptly the child exits.
func (p *streamPipe) closeWriter() { p.closeWriterOnce.Do(func() { p.w.Close() }) }

// closeReader unblocks a drain whose writers have not all exited.
func (p *streamPipe) closeReader() {
	p.closeReaderOnce.Do(func() {
		p.readerClosed.Store(true)
		p.r.Close()
	})
}

func (p *streamPipe) closeAll() {
	p.closeWriter()
	p.closeReader()
}
