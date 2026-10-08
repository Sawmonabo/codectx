package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Function is one callable's dependence facts: what the worker's lowering
// and the flow core computed for it, projected onto the callable's named
// variables and carried as byte ranges and indices only. The parent mints
// every identity, validates every range against the pinned bytes and
// publishes the four file-local families from it (see
// docs/providers-treesitter.md, Dependence facts).
//
// Vars are the callable's named variables, its parameters and locals, each
// as the byte range of the identifier that declares it; an index into Vars
// is a variable of this message and of no other. A variable the lowering
// owns and binds to no name never appears. Nodes are the evidence ranges the
// facts below cite: the span of the control-flow node the fact was observed
// at; an index into Nodes is a node of this message.
//
// A failed function carries its span, Failed and Cause, and nothing else: its
// analysis panicked, the worker recovered it, and none of its facts is
// published.
type Function struct {
	// Span is the callable's byte range.
	Span Span
	// Failed reports a recovered panic in this function's lowering or
	// analysis; Cause is the recovered value's text.
	Failed bool
	Cause  string
	// Unresolved counts the jumps whose target names no open frame or label,
	// which only an ill-formed or error-recovered source holds.
	Unresolved uint32
	Vars       []Span
	Nodes      []Span
	// Control holds From control_depends_on To: From a variable of the
	// dependent node, To a variable the controlling node's evaluation reads,
	// Node the dependent node.
	Control []Pair
	// Flows holds From data_flows_to To: a value of From reaches the node
	// Node, which defines or may define To.
	Flows []Pair
	// Reads holds Var read at Node; Writes holds Var defined at Node, May
	// when the definition is a may-definition.
	Reads  []Access
	Writes []Access
}

// Span is a byte range [Start, End) of the source.
type Span struct{ Start, End uint32 }

// Pair is one relation between two variables of a Function, evidenced at
// one of its nodes.
type Pair struct{ From, To, Node uint32 }

// Access is one read or definition of a variable of a Function at one of its
// nodes.
type Access struct {
	Var, Node uint32
	May       bool
}

// The function message's status byte.
const (
	functionAnalysed byte = 0
	functionFailed   byte = 1
)

// AppendFunction appends f's encoding to dst. Every integer is a big-endian
// uint32 and every list is its length followed by its elements:
//
//	span        start, end
//	status      one byte: 0 analysed, 1 failed
//	failed:     cause as length and UTF-8 bytes; nothing follows
//	analysed:   unresolved
//	            vars    n × (start, end)
//	            nodes   n × (start, end)
//	            control n × (from, to, node)
//	            flows   n × (from, to, node)
//	            reads   n × (var, node)
//	            writes  n × (var, node, may as one byte 0 or 1)
func AppendFunction(dst []byte, f *Function) []byte {
	dst = binary.BigEndian.AppendUint32(dst, f.Span.Start)
	dst = binary.BigEndian.AppendUint32(dst, f.Span.End)
	if f.Failed {
		dst = append(dst, functionFailed)
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Cause)))
		return append(dst, f.Cause...)
	}
	dst = append(dst, functionAnalysed)
	dst = binary.BigEndian.AppendUint32(dst, f.Unresolved)
	for _, spans := range [...][]Span{f.Vars, f.Nodes} {
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(spans)))
		for _, s := range spans {
			dst = binary.BigEndian.AppendUint32(dst, s.Start)
			dst = binary.BigEndian.AppendUint32(dst, s.End)
		}
	}
	for _, pairs := range [...][]Pair{f.Control, f.Flows} {
		dst = binary.BigEndian.AppendUint32(dst, uint32(len(pairs)))
		for _, p := range pairs {
			dst = binary.BigEndian.AppendUint32(dst, p.From)
			dst = binary.BigEndian.AppendUint32(dst, p.To)
			dst = binary.BigEndian.AppendUint32(dst, p.Node)
		}
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Reads)))
	for _, a := range f.Reads {
		dst = binary.BigEndian.AppendUint32(dst, a.Var)
		dst = binary.BigEndian.AppendUint32(dst, a.Node)
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(f.Writes)))
	for _, a := range f.Writes {
		dst = binary.BigEndian.AppendUint32(dst, a.Var)
		dst = binary.BigEndian.AppendUint32(dst, a.Node)
		may := byte(0)
		if a.May {
			may = 1
		}
		dst = append(dst, may)
	}
	return dst
}

// ErrMalformedFunction reports a function message no writer of this protocol
// produces: a short or overlong payload, an unknown status or flag byte, a
// cause that is not UTF-8, or an index past the variables or nodes it
// indexes.
var ErrMalformedFunction = errors.New("wire: malformed function message")

// DecodeFunction decodes one KindFunction payload. A list's length is
// checked against the bytes that remain before anything is allocated for
// it, so a corrupt count costs nothing beyond the payload already read.
// Byte ranges are not checked against any source here: the parent validates
// them against the pinned bytes.
func DecodeFunction(payload []byte) (Function, error) {
	d := decoder{b: payload}
	var f Function
	f.Span = Span{d.u32(), d.u32()}
	switch d.u8() {
	case functionFailed:
		f.Failed = true
		n := d.count(1)
		f.Cause = string(d.take(n))
		if d.err == nil && !utf8.ValidString(f.Cause) {
			d.fail("the cause is not UTF-8")
		}
	case functionAnalysed:
		f.Unresolved = d.u32()
		f.Vars = d.spans()
		f.Nodes = d.spans()
		f.Control = d.pairs(len(f.Vars), len(f.Nodes))
		f.Flows = d.pairs(len(f.Vars), len(f.Nodes))
		f.Reads = d.accesses(len(f.Vars), len(f.Nodes), false)
		f.Writes = d.accesses(len(f.Vars), len(f.Nodes), true)
	default:
		d.fail("an unknown status byte")
	}
	if d.err == nil && len(d.b) != 0 {
		d.fail(fmt.Sprintf("%d bytes past the message's end", len(d.b)))
	}
	if d.err != nil {
		return Function{}, d.err
	}
	return f, nil
}

// decoder reads a function payload front to back; the first failure sticks
// and every later read answers zero.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) fail(msg string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %s", ErrMalformedFunction, msg)
	}
	d.b = nil
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n > len(d.b) {
		d.fail("the payload ends early")
		return nil
	}
	out := d.b[:n]
	d.b = d.b[n:]
	return out
}

func (d *decoder) u32() uint32 {
	if b := d.take(4); b != nil {
		return binary.BigEndian.Uint32(b)
	}
	return 0
}

func (d *decoder) u8() byte {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}

// count reads a list length whose elements are elem bytes each, refusing one
// the remaining payload cannot hold.
func (d *decoder) count(elem int) int {
	n := d.u32()
	if d.err == nil && uint64(n)*uint64(elem) > uint64(len(d.b)) {
		d.fail("a list longer than the payload")
		return 0
	}
	return int(n)
}

func (d *decoder) spans() []Span {
	n := d.count(8)
	out := make([]Span, 0, n)
	for range n {
		out = append(out, Span{d.u32(), d.u32()})
	}
	return out
}

func (d *decoder) index(v uint32, bound int) uint32 {
	if d.err == nil && uint64(v) >= uint64(bound) {
		d.fail("an index past the list it indexes")
	}
	return v
}

func (d *decoder) pairs(vars, nodes int) []Pair {
	n := d.count(12)
	out := make([]Pair, 0, n)
	for range n {
		out = append(out, Pair{From: d.index(d.u32(), vars), To: d.index(d.u32(), vars), Node: d.index(d.u32(), nodes)})
	}
	return out
}

func (d *decoder) accesses(vars, nodes int, may bool) []Access {
	elem := 8
	if may {
		elem = 9
	}
	n := d.count(elem)
	out := make([]Access, 0, n)
	for range n {
		a := Access{Var: d.index(d.u32(), vars), Node: d.index(d.u32(), nodes)}
		if may {
			switch d.u8() {
			case 0:
			case 1:
				a.May = true
			default:
				d.fail("a may flag that is neither 0 nor 1")
			}
		}
		out = append(out, a)
	}
	return out
}
