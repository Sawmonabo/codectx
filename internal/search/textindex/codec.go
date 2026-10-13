package textindex

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The buffers that cross the boundary, in the layouts textindex.h states.

func putUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

func putVarint(b []byte, v int64) []byte { return binary.AppendVarint(b, v) }

func putString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// encodeUnit lays out one unit's documents for cx_index_add.
func encodeUnit(unitKey string, docs []Document) []byte {
	size := len(unitKey) + 16
	for i := range docs {
		d := &docs[i]
		size += 64 + len(d.SearchKey) + len(d.NodeID) + len(d.FileID) + len(d.Path) + len(d.Kind) +
			len(d.Name) + len(d.QualifiedName) + len(d.Signature) + len(d.Body)
	}
	b := make([]byte, 0, size)
	b = putString(b, unitKey)
	b = putUvarint(b, uint64(len(docs)))
	for i := range docs {
		d := &docs[i]
		b = putUvarint(b, d.DocID)
		b = putString(b, d.SearchKey)
		b = putString(b, string(d.NodeID))
		b = putString(b, string(d.FileID))
		b = putString(b, d.Path)
		b = putString(b, string(d.Kind))
		b = putString(b, d.Name)
		b = putString(b, d.QualifiedName)
		b = putString(b, d.Signature)
		b = putUvarint(b, d.Bytes.Start)
		b = putUvarint(b, d.Bytes.End)
		b = putString(b, d.Body)
	}
	return b
}

// encodeRequest lays out one search request for cx_view_search.
func encodeRequest(r Request) []byte {
	b := make([]byte, 0, 64+len(r.Query))
	b = putString(b, r.Query)
	b = putUvarint(b, uint64(len(r.Kinds)))
	for _, k := range r.Kinds {
		b = putString(b, string(k))
	}
	b = putUvarint(b, uint64(len(r.PathPrefixes)))
	for _, p := range r.PathPrefixes {
		b = putString(b, p)
	}
	b = putUvarint(b, uint64(r.Depth))
	b = append(b, byte(r.LengthRule))
	b = putUvarint(b, uint64(max(r.MaxTerms, 0)))
	b = putUvarint(b, uint64(max(r.MaxBytes, 0)))
	b = putUvarint(b, uint64(max(r.ResultBytes, 0)))
	b = putUvarint(b, uint64(max(r.MarkedBound, 0)))
	var flags byte
	if r.IgnoreSummaries {
		flags |= 1
	}
	if r.After == nil {
		return append(b, flags)
	}
	b = append(b, flags|2)
	b = putVarint(b, r.After.ScoreMicros)
	return putUvarint(b, r.After.DocID)
}

// reader decodes a library buffer. Errors latch: after the first short read
// every later read yields zero, and err reports the first failure once.
type reader struct {
	b   []byte
	at  int
	bad bool
}

func (r *reader) uvarint() uint64 {
	if r.bad {
		return 0
	}
	v, n := binary.Uvarint(r.b[r.at:])
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.at += n
	return v
}

func (r *reader) varint() int64 {
	if r.bad {
		return 0
	}
	v, n := binary.Varint(r.b[r.at:])
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.at += n
	return v
}

func (r *reader) byte() byte {
	if r.bad || r.at >= len(r.b) {
		r.bad = true
		return 0
	}
	v := r.b[r.at]
	r.at++
	return v
}

func (r *reader) bool() bool { return r.byte() != 0 }

func (r *reader) string() string {
	n := r.uvarint()
	if r.bad || n > uint64(len(r.b)-r.at) {
		r.bad = true
		return ""
	}
	s := string(r.b[r.at : r.at+int(n)])
	r.at += int(n)
	return s
}

func (r *reader) fixed64() uint64 {
	if r.bad || len(r.b)-r.at < 8 {
		r.bad = true
		return 0
	}
	v := binary.LittleEndian.Uint64(r.b[r.at:])
	r.at += 8
	return v
}

func (r *reader) err() error {
	if r.bad || r.at != len(r.b) {
		return errMalformed
	}
	return nil
}

var errMalformed = &model.Error{Code: model.CodeInternal,
	Message: "textindex: the library returned a malformed buffer"}

// decodePage reads a cx_view_search page.
func decodePage(b []byte) (Page, error) {
	r := &reader{b: b}
	var p Page
	p.Terms = int(r.uvarint())
	p.More = r.bool()
	p.Canceled = r.bool()
	p.Matches = int64(r.uvarint())
	p.MatchesExact = r.bool()
	p.Stats.Segments = int64(r.uvarint())
	p.Stats.SegmentsWithoutSummary = int64(r.uvarint())
	p.Stats.BlocksTotal = int64(r.uvarint())
	p.Stats.BlocksScored = int64(r.uvarint())
	p.Stats.DocsScored = int64(r.uvarint())
	p.Stats.KeySkips = int64(r.uvarint())
	p.Stats.KeyLookups = int64(r.uvarint())
	p.Stats.Heaps = int64(r.uvarint())
	p.Stats.Cap = int64(r.uvarint())
	p.Stats.Marked = int64(r.uvarint())
	p.Stats.NewMarks = int64(r.uvarint())
	p.Stats.Bytes = int64(r.uvarint())
	n := r.uvarint()
	if n > uint64(len(b)) {
		return Page{}, errMalformed
	}
	p.Hits = make([]Hit, 0, n)
	for i := uint64(0); i < n && !r.bad; i++ {
		var h Hit
		h.DocID = r.uvarint()
		h.SearchKey = r.string()
		h.NodeID = model.NodeID(r.string())
		h.FileID = model.FileID(r.string())
		h.Path = r.string()
		h.Kind = model.NodeKind(r.string())
		h.Name = r.string()
		h.QualifiedName = r.string()
		h.Signature = r.string()
		h.Bytes.Start = r.uvarint()
		h.Bytes.End = r.uvarint()
		h.ScoreMicros = r.varint()
		h.Occurrences = int64(r.uvarint())
		h.OccurrencesExact = r.bool()
		p.Hits = append(p.Hits, h)
	}
	if err := r.err(); err != nil {
		return Page{}, err
	}
	return p, nil
}

// scoreInput is one row of the cross-language arithmetic check.
type scoreInput struct {
	Docs, DF uint64
	WTF      float64
	DL       uint64
	AvgDL    float64
}

// scoreOutput is the library's answer for one scoreInput.
type scoreOutput struct {
	IDF    float64
	Score  float64
	Micros int64
}

func encodeScoreInputs(in []scoreInput) []byte {
	b := make([]byte, 0, 40*len(in))
	for _, v := range in {
		b = binary.LittleEndian.AppendUint64(b, v.Docs)
		b = binary.LittleEndian.AppendUint64(b, v.DF)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v.WTF))
		b = binary.LittleEndian.AppendUint64(b, v.DL)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v.AvgDL))
	}
	return b
}

func decodeScoreOutputs(b []byte, n int) ([]scoreOutput, error) {
	if len(b) != 24*n {
		return nil, errors.Join(errMalformed, errors.New("score vector length"))
	}
	r := &reader{b: b}
	out := make([]scoreOutput, n)
	for i := range out {
		out[i].IDF = math.Float64frombits(r.fixed64())
		out[i].Score = math.Float64frombits(r.fixed64())
		out[i].Micros = int64(r.fixed64())
	}
	if err := r.err(); err != nil {
		return nil, err
	}
	return out, nil
}
