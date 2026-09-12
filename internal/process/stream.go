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
// Once the limit is reached the drain keeps reading and discarding. Stopping
// instead would leave the child blocked writing into a full pipe, where it
// could neither make progress nor notice that it is being asked to stop.
type streamPipe struct {
	r, w *os.File
	sink io.Writer
	// capture holds the bytes when the caller supplied no writer.
	capture bytes.Buffer
	limit   int64

	limitHit  chan struct{}
	limitOnce sync.Once

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
	p := &streamPipe{r: r, w: w, sink: sink, limit: limit, limitHit: make(chan struct{})}
	// Assigning an *os.File makes os/exec hand the descriptor to the child
	// directly, with no copying goroutine of its own to wait for.
	*target = w
	return p, nil
}

func (p *streamPipe) drain() {
	buf := make([]byte, drainBufferBytes)
	for {
		n, err := p.r.Read(buf)
		if n > 0 {
			remaining := p.limit - p.delivered
			if remaining > 0 {
				take := int64(n)
				if take > remaining {
					take = remaining
				}
				if werr := p.write(buf[:take]); werr != nil && p.failure == nil {
					p.failure = werr
				}
				p.delivered += take
			}
			if int64(n) > remaining {
				p.truncated = true
				p.limitOnce.Do(func() { close(p.limitHit) })
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

func (p *streamPipe) limited() bool { return p.truncated }

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
