package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Bounds on one LSP base-protocol message. They are enforced before any
// allocation the peer controls: a Content-Length header is checked against
// MaxFrameBytes before the body buffer exists, the header block is bounded in
// line count and line length, and the decoded JSON is bounded in nesting depth
// before it is unmarshalled into typed structures (Section 21, provider data).
const (
	// maxHeaderLineBytes bounds one header line. The two headers the protocol
	// defines are short; a longer line is not a header.
	maxHeaderLineBytes = 1024
	// maxHeaderLines bounds the header block. The protocol defines two
	// headers; a server sending more than eight is not speaking it.
	maxHeaderLines = 8
	// maxJSONDepth bounds nesting in a decoded message. DocumentSymbol trees
	// are the deepest legitimate structure and never approach this.
	maxJSONDepth = 64
)

// readFrame reads one Content-Length framed message from r. The returned
// payload is at most maxBytes long; a larger declared length is a typed
// resource limit and nothing of the body is read, because the connection is
// then unusable and the caller fails it.
func readFrame(r *bufio.Reader, maxBytes int64) ([]byte, error) {
	length := int64(-1)
	for lines := 0; ; lines++ {
		if lines >= maxHeaderLines {
			return nil, outputInvalid("message header block exceeds %d lines", maxHeaderLines)
		}
		line, err := readHeaderLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, outputInvalid("message header %q has no colon", truncate(line, 64))
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || n < 0 {
				return nil, outputInvalid("Content-Length %q is not a non-negative integer", truncate(strings.TrimSpace(value), 64))
			}
			length = n
		}
		// Content-Type is the only other defined header and its only defined
		// value is UTF-8 JSON; it is accepted and ignored, as is an unknown
		// header, because neither changes how the body is read.
	}
	if length < 0 {
		return nil, outputInvalid("message has no Content-Length header")
	}
	if length > maxBytes {
		return nil, resourceLimit("message declares %d bytes, over the %d-byte frame bound", length, maxBytes).
			WithDetail("limit", "max_frame_bytes")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, streamError(err)
	}
	return payload, nil
}

// readHeaderLine reads one CRLF-terminated header line without the terminator.
// A bare LF is accepted as the terminator too; the protocol requires CRLF, and
// a server that sends LF is still unambiguous. The bound is checked against
// the chunk the reader already holds before it is appended, so an over-long
// line is never copied into the accumulator.
func readHeaderLine(r *bufio.Reader) (string, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(line)+len(chunk) > maxHeaderLineBytes {
			return "", outputInvalid("message header line exceeds %d bytes", maxHeaderLineBytes)
		}
		line = append(line, chunk...)
		if err == nil {
			break
		}
		if err != bufio.ErrBufferFull {
			return "", streamError(err)
		}
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// writeFrame writes one framed message as two writes: the short header and
// then the payload itself. Concatenating them would copy the whole payload a
// second time for no benefit, and the payload is already bounded by
// MaxFrameBytes in conn.write. The caller holds the writer lock, so the two
// writes cannot be interleaved with another message's.
func writeFrame(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, "Content-Length: "+strconv.Itoa(len(payload))+"\r\n\r\n"); err != nil {
		return streamError(err)
	}
	if _, err := w.Write(payload); err != nil {
		return streamError(err)
	}
	return nil
}

// checkDepth rejects a payload nested deeper than maxJSONDepth before it is
// decoded into typed structures. encoding/json has no depth limit of its own
// below the 10000-level guard, and a deeply nested response would otherwise
// cost a recursive decode the peer chose the size of.
func checkDepth(payload []byte) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return outputInvalid("message is not valid JSON: %v", err)
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
			if depth > maxJSONDepth {
				return outputInvalid("message nests deeper than %d levels", maxJSONDepth)
			}
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
}

// streamError types a transport failure. End of file is the server closing its
// side, which is reported as unavailable rather than as a protocol violation.
func streamError(err error) *model.Error {
	if err == io.EOF || err == io.ErrUnexpectedEOF || err == io.ErrClosedPipe {
		return unavailable("the language server closed its connection")
	}
	return unavailable("the language server connection failed: %v", err)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func outputInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: fmt.Sprintf(format, args...)}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

func unavailable(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderUnavailable, Message: fmt.Sprintf(format, args...)}
}

func trustRequired(format string, args ...any) *model.Error {
	return (&model.Error{Code: model.CodeTrustRequired, Message: fmt.Sprintf(format, args...)}).
		WithRemediation("Construct the profile with lsp.Resolve, which starts only the payload the embedded tool lock pins.")
}

// internalError is a product defect: a pinned definition that cannot be started
// as written. It is never a condition a user can correct.
func internalError(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func timedOut(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderTimeout, Message: fmt.Sprintf(format, args...), Retryable: true}
}
