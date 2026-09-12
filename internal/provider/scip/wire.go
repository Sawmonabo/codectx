package scip

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Protobuf wire types. Groups (3 and 4) are not part of proto3 and never
// appear in a SCIP index; a reader that meets one refuses the input.
type wireType uint64

const (
	wireVarint  wireType = 0
	wireFixed64 wireType = 1
	wireBytes   wireType = 2
	wireFixed32 wireType = 5
)

// byteSource is what the wire reader consumes: a stream with single-byte
// reads. bufio.Reader and bytes.Reader both satisfy it, so the same reader
// walks the index stream and the bounded record buffers cut from it.
type byteSource interface {
	io.Reader
	io.ByteReader
}

// reader walks protobuf wire fields within one message of known length. It
// never reads past that length, never allocates a field larger than the
// bound the caller passes, and can hand back a sub-reader confined to one
// nested message so a Document of any size is walked field by field rather
// than materialized (Section 11.4).
type reader struct {
	src       byteSource
	remaining int64
	// consumed counts every byte taken from the shared stream; nested readers
	// share the counter with their parent.
	consumed *int64
}

// newReader bounds src to size bytes.
func newReader(src byteSource, size int64) *reader {
	var consumed int64
	return &reader{src: src, remaining: size, consumed: &consumed}
}

func (r *reader) done() bool { return r.remaining <= 0 }

func (r *reader) readByte() (byte, error) {
	if r.remaining <= 0 {
		return 0, malformed("message ends inside a field")
	}
	b, err := r.src.ReadByte()
	if err != nil {
		return 0, readError(err)
	}
	r.remaining--
	*r.consumed++
	return b, nil
}

// varint reads one base-128 varint of at most ten bytes.
func (r *reader) varint() (uint64, error) {
	var x uint64
	var shift uint
	for i := 0; i < 10; i++ {
		b, err := r.readByte()
		if err != nil {
			return 0, err
		}
		if b < 0x80 {
			if i == 9 && b > 1 {
				return 0, malformed("varint overflows 64 bits")
			}
			return x | uint64(b)<<shift, nil
		}
		x |= uint64(b&0x7f) << shift
		shift += 7
	}
	return 0, malformed("varint is longer than ten bytes")
}

// tag reads one field key.
func (r *reader) tag() (int32, wireType, error) {
	v, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	field, wt := v>>3, wireType(v&7)
	if field == 0 || field > 1<<29-1 {
		return 0, 0, malformed("field number " + strconv.FormatUint(field, 10) + " is out of range")
	}
	switch wt {
	case wireVarint, wireFixed64, wireBytes, wireFixed32:
		return int32(field), wt, nil
	}
	return 0, 0, malformed("wire type " + strconv.FormatUint(uint64(wt), 10) + " is not valid in a SCIP index")
}

// length reads a length prefix and refuses one that reaches past the
// enclosing message: no allocation is ever sized by an unchecked prefix.
func (r *reader) length() (int64, error) {
	v, err := r.varint()
	if err != nil {
		return 0, err
	}
	if v > uint64(r.remaining) {
		return 0, malformed("a length-delimited field is longer than the message that holds it")
	}
	return int64(v), nil
}

// sub charges n bytes to r and returns a reader confined to exactly them. The
// caller finishes it with discard so the parent's position stays correct
// whatever the nested message contained.
//
// n is checked against what is left of the enclosing message: a fixed-width
// field cut from a message with fewer bytes left would otherwise drive
// remaining negative and desynchronize every later field of that message,
// which is how a malformed index turns into wrong facts rather than a
// refusal.
func (r *reader) sub(n int64) (*reader, error) {
	if n < 0 || n > r.remaining {
		return nil, malformed("a nested field is longer than the message that holds it")
	}
	r.remaining -= n
	return &reader{src: r.src, remaining: n, consumed: r.consumed}, nil
}

// discard drops whatever remains of this reader's message in bounded chunks.
func (r *reader) discard() error {
	for r.remaining > 0 {
		n, err := io.CopyN(io.Discard, r.src, min(r.remaining, 32<<10))
		r.remaining -= n
		*r.consumed += n
		if err != nil {
			return readError(err)
		}
	}
	return nil
}

// bytes reads exactly n bytes into buf (reused when large enough), refusing n
// over limit with CTX_RESOURCE_LIMIT before anything is allocated.
func (r *reader) bytes(n, limit int64, what string, buf []byte) ([]byte, error) {
	if n > limit {
		return nil, overLimit(what, n, limit)
	}
	if int64(cap(buf)) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	if _, err := io.ReadFull(r.src, buf); err != nil {
		return nil, readError(err)
	}
	r.remaining -= n
	*r.consumed += n
	return buf, nil
}

// stream copies exactly n bytes to w without holding them.
func (r *reader) stream(n int64, w io.Writer) error {
	copied, err := io.CopyN(w, r.src, n)
	r.remaining -= copied
	*r.consumed += copied
	if err != nil {
		return readError(err)
	}
	return nil
}

// str reads one length-delimited string field bounded at limit bytes.
func (r *reader) str(limit int, what string, buf []byte) (string, []byte, error) {
	n, err := r.length()
	if err != nil {
		return "", buf, err
	}
	buf, err = r.bytes(n, int64(limit), what, buf)
	if err != nil {
		return "", buf, err
	}
	return string(buf), buf, nil
}

// skip drops one field of the given wire type.
func (r *reader) skip(wt wireType) error {
	switch wt {
	case wireVarint:
		_, err := r.varint()
		return err
	case wireFixed64:
		return r.discardSub(8)
	case wireFixed32:
		return r.discardSub(4)
	default:
		n, err := r.length()
		if err != nil {
			return err
		}
		return r.discardSub(n)
	}
}

// discardSub drops the next n bytes of this message as one nested field.
func (r *reader) discardSub(n int64) error {
	sub, err := r.sub(n)
	if err != nil {
		return err
	}
	return sub.discard()
}

// ints reads a repeated int32 field: packed (a length-delimited run of
// varints) or a single unpacked varint. At most max values are accepted.
func (r *reader) ints(wt wireType, out []int32, max int) ([]int32, error) {
	if wt == wireVarint {
		v, err := r.varint()
		if err != nil {
			return out, err
		}
		if len(out) >= max {
			return out, malformed("repeated int32 field holds more than " + strconv.Itoa(max) + " values")
		}
		return append(out, int32(v)), nil
	}
	if wt != wireBytes {
		return out, malformed("repeated int32 field has an unexpected wire type")
	}
	n, err := r.length()
	if err != nil {
		return out, err
	}
	sub, err := r.sub(n)
	if err != nil {
		return out, err
	}
	for !sub.done() {
		v, err := sub.varint()
		if err != nil {
			return out, err
		}
		if len(out) >= max {
			return out, malformed("repeated int32 field holds more than " + strconv.Itoa(max) + " values")
		}
		out = append(out, int32(v))
	}
	return out, nil
}

// readError types a stream failure: a stream that ends inside a message is
// malformed provider output; any other error (a CAS integrity failure, a
// cancellation) is already typed and passes through.
func readError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return malformed("index ends inside a message")
	}
	return err
}

func malformed(msg string) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: "scip index is malformed: " + msg}
}

func overLimit(what string, size, limit int64) *model.Error {
	return (&model.Error{Code: model.CodeResourceLimit,
		Message: fmt.Sprintf("scip %s of %d bytes exceeds the %d-byte bound", what, size, limit)}).
		WithDetail("limit", what).WithDetail("record_bytes", strconv.FormatInt(size, 10)).WithDetail("limit_bytes", strconv.FormatInt(limit, 10))
}
