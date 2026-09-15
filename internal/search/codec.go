package search

// L4 owns this file: the wire format of one candidate inside the query's own
// external sort runs.

import (
	"encoding/binary"
	"maps"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The candidate codec for the two-pass external sort. These records never
// leave the process -- a run file is written and read by one query, deleted
// with its spool directory, and carries no version marker because nothing can
// ever read a run this build did not write. JSON was the first codec for that
// reason; it cost reflection, a field name on every value and a base64 hop for
// nothing, and the encode/decode pair is the second-largest frame of
// Service.rank on a wide query (QPERF-4 §5).
//
// The trade-off this accepts: a packed codec CAN drift from the struct, where
// JSON could not. TestCandidateCodecRoundTrip is what pays for that -- it
// round-trips every field of scored, including the pointer and map fields
// whose absent/empty distinction the JSON codec collapsed, and a new field
// left out of encodeScored fails it.
//
// Two JSON behaviours are reproduced deliberately rather than improved on,
// because the sort must answer exactly what it answered before:
//
//   - `omitempty` made a nil slice and an empty slice indistinguishable, and
//     likewise a nil and an empty map. A zero count decodes to nil here.
//   - Everything else is exact, including the difference between a nil
//     *SourceRange and a pointer to the zero value, which `omitempty` on a
//     pointer already preserved.

// putString appends a length-prefixed string.
func putString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// cursor reads a packed record. err latches: once a read has run past the end
// of the buffer every later read is a no-op, so decodeScored checks once.
type cursor struct {
	b   []byte
	at  int
	bad bool
}

func (c *cursor) uvarint() uint64 {
	if c.bad {
		return 0
	}
	v, n := binary.Uvarint(c.b[c.at:])
	if n <= 0 {
		c.bad = true
		return 0
	}
	c.at += n
	return v
}

func (c *cursor) varint() int64 {
	if c.bad {
		return 0
	}
	v, n := binary.Varint(c.b[c.at:])
	if n <= 0 {
		c.bad = true
		return 0
	}
	c.at += n
	return v
}

// str reads a length-prefixed string. The result is a copy, so no decoded
// candidate aliases the merge reader's reusable record buffer.
func (c *cursor) str() string {
	n := int(c.uvarint())
	if c.bad || n < 0 || c.at+n > len(c.b) {
		c.bad = true
		return ""
	}
	s := string(c.b[c.at : c.at+n])
	c.at += n
	return s
}

// strings reads a length-prefixed list. A zero count answers nil, which is
// what the JSON codec's `omitempty` produced for both nil and empty.
func (c *cursor) strings() []string {
	n := int(c.uvarint())
	if c.bad || n <= 0 {
		return nil
	}
	out := make([]string, 0, min(n, 64))
	for i := 0; i < n && !c.bad; i++ {
		out = append(out, c.str())
	}
	return out
}

// putRange appends a presence byte and, when present, the source range.
func putRange(b []byte, r *model.SourceRange) []byte {
	if r == nil {
		return append(b, 0)
	}
	b = append(b, 1)
	for _, p := range [2]model.Position{r.Start, r.End} {
		b = binary.AppendUvarint(b, p.Byte)
		b = binary.AppendUvarint(b, uint64(p.Line))
		b = binary.AppendUvarint(b, uint64(p.Column))
	}
	return b
}

func (c *cursor) sourceRange() *model.SourceRange {
	if c.uvarint() == 0 {
		return nil
	}
	var r model.SourceRange
	for _, p := range []*model.Position{&r.Start, &r.End} {
		p.Byte = c.uvarint()
		p.Line = uint32(c.uvarint())
		p.Column = uint32(c.uvarint())
	}
	return &r
}

// encodeScored packs one candidate. Keys are written in a fixed order so the
// record is self-delimiting without any field tag.
func encodeScored(v scored) ([]byte, error) {
	b := make([]byte, 0, 192+len(v.Path)+len(v.SearchKey)+len(v.Hit.QualifiedName)+len(v.Hit.Signature))

	b = putString(b, string(v.Tier))
	b = binary.AppendVarint(b, v.ScoreMicros)
	b = putString(b, v.Path)
	b = binary.AppendUvarint(b, v.StartByte)
	b = putString(b, string(v.NodeID))
	b = putString(b, v.SearchKey)
	b = binary.AppendVarint(b, v.RowID)
	b = binary.AppendVarint(b, v.Occurrences)

	b = binary.AppendUvarint(b, uint64(len(v.Reasons)))
	for _, r := range v.Reasons {
		b = putString(b, r)
	}

	h := v.Hit
	b = putString(b, string(h.NodeID))
	b = putString(b, string(h.FileID))
	b = putString(b, h.Path)
	b = putString(b, string(h.Kind))
	b = putString(b, h.Name)
	b = putString(b, h.QualifiedName)
	b = putString(b, h.Signature)
	b = putString(b, string(h.Tier))
	b = binary.AppendVarint(b, h.ScoreMicros)
	b = putRange(b, h.Range)
	b = binary.AppendVarint(b, h.OccurrenceCount)
	b = binary.AppendUvarint(b, uint64(len(h.Reasons)))
	for _, r := range h.Reasons {
		b = putString(b, r)
	}
	// Sorted keys: a map's range order is random, and a record whose bytes
	// depend on that order would make an otherwise identical run file differ
	// run to run.
	b = binary.AppendUvarint(b, uint64(len(h.UnresolvedFields)))
	for _, k := range slices.Sorted(maps.Keys(h.UnresolvedFields)) {
		b = putString(b, k)
		b = putString(b, h.UnresolvedFields[k])
	}

	if v.Span == nil {
		b = append(b, 0)
	} else {
		b = append(b, 1)
		b = binary.AppendUvarint(b, v.Span.Start)
		b = binary.AppendUvarint(b, v.Span.End)
	}
	b = binary.AppendVarint(b, v.Folded)
	return b, nil
}

// decodeScored is encodeScored's exact inverse. A record that does not read
// back is a corrupt run file, which is the same class the JSON codec reported.
func decodeScored(b []byte) (scored, error) {
	c := &cursor{b: b}
	var v scored

	v.Tier = model.SearchTier(c.str())
	v.ScoreMicros = c.varint()
	v.Path = c.str()
	v.StartByte = c.uvarint()
	v.NodeID = model.NodeID(c.str())
	v.SearchKey = c.str()
	v.RowID = c.varint()
	v.Occurrences = c.varint()
	v.Reasons = c.strings()

	h := &v.Hit
	h.NodeID = model.NodeID(c.str())
	h.FileID = model.FileID(c.str())
	h.Path = c.str()
	h.Kind = model.NodeKind(c.str())
	h.Name = c.str()
	h.QualifiedName = c.str()
	h.Signature = c.str()
	h.Tier = model.SearchTier(c.str())
	h.ScoreMicros = c.varint()
	h.Range = c.sourceRange()
	h.OccurrenceCount = c.varint()
	h.Reasons = c.strings()
	if n := int(c.uvarint()); n > 0 && !c.bad {
		h.UnresolvedFields = make(map[string]string, min(n, 64))
		for i := 0; i < n && !c.bad; i++ {
			k := c.str()
			h.UnresolvedFields[k] = c.str()
		}
	}

	if c.uvarint() != 0 {
		var span model.ByteRange
		span.Start = c.uvarint()
		span.End = c.uvarint()
		v.Span = &span
	}
	v.Folded = c.varint()

	if c.bad || c.at != len(b) {
		return scored{}, &model.Error{Code: model.CodeStorageCorrupt, Message: "search: a spilled candidate is not readable"}
	}
	return v, nil
}
