package link

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The occurrences publication's first merge (publishTargets) needs of a
// member's table are its targetable endpoints and its language, about a
// quarter of its stored form. The resolution reads each table it resolves anyway, so
// those reads also spill, once per member, every targetable endpoint of the
// table into a sorted run by (FileID, node), and the first merge joins the
// targets with that run instead of reading the table a third time. A member
// the resolution did not read (a refresh republishes members whose targeted
// set changed without re-running them) has no endpoints in the run, and the
// merge reads its table as before.

// spilledEndpoint is one targetable endpoint of one member's table, with the
// member's language.
type spilledEndpoint struct {
	File     model.FileID
	Language string
	Endpoint
}

func compareSpilledEndpoint(a, b spilledEndpoint) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Node, b.Node))
}

func appendPosition(out []byte, p model.Position) []byte {
	out = binary.AppendUvarint(out, p.Byte)
	out = binary.AppendUvarint(out, uint64(p.Line))
	return binary.AppendUvarint(out, uint64(p.Column))
}

func (r *byteReader) position() model.Position {
	p := model.Position{Byte: r.uvarint()}
	line, col := r.uvarint(), r.uvarint()
	if line > 1<<32-1 || col > 1<<32-1 {
		r.fail()
	}
	p.Line, p.Column = uint32(line), uint32(col)
	return p
}

func encodeSpilledEndpoint(e spilledEndpoint) ([]byte, error) {
	out := appendString(nil, string(e.File))
	out = appendString(out, e.Language)
	out = appendString(out, string(e.Node))
	out = appendString(out, e.CanonicalKey)
	out = appendString(out, string(e.Kind))
	out = appendString(out, e.Name)
	out = appendString(out, e.Qualified)
	out = appendPosition(appendPosition(out, e.Range.Start), e.Range.End)
	if e.NameRange == nil {
		return append(out, 0), nil
	}
	return appendPosition(appendPosition(append(out, 1), e.NameRange.Start), e.NameRange.End), nil
}

func decodeSpilledEndpoint(raw []byte) (spilledEndpoint, error) {
	r := byteReader{b: raw}
	e := spilledEndpoint{File: model.FileID(r.str()), Language: r.str()}
	e.Node, e.CanonicalKey, e.Kind = model.NodeID(r.str()), r.str(), model.NodeKind(r.str())
	e.Name, e.Qualified = r.str(), r.str()
	e.Range = model.SourceRange{Start: r.position(), End: r.position()}
	switch r.byte() {
	case 0:
	case 1:
		nr := model.SourceRange{Start: r.position(), End: r.position()}
		e.NameRange = &nr
	default:
		r.fail()
	}
	if r.err != nil || len(r.b) != 0 || e.File == "" || e.Node == "" {
		return spilledEndpoint{}, errMalformedEndpoint
	}
	return e, nil
}

var errMalformedEndpoint = errors.New("link: malformed spilled endpoint")

// targetSpill is the unit's Tables with every table the resolution reads
// spilling its targetable endpoints, each member's once: reads come in
// ascending FileID order within a pass, and a member at or below the last one
// spilled is not spilled again.
type targetSpill struct {
	inner Tables
	mu    sync.Mutex
	sort  *pagination.ExternalSort[spilledEndpoint]
	last  model.FileID
	err   error
}

// observedSpill is a targetSpill over Tables that also observe the sites a
// link resolves (Observer), which it keeps observing.
type observedSpill struct {
	*targetSpill
	Observer
}

// spillTargets wraps tables so that the tables read until sorted is called
// spill their targetable endpoints into dir.
func spillTargets(dir string, tables Tables) (Tables, *targetSpill, error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeSpilledEndpoint, decodeSpilledEndpoint, compareSpilledEndpoint)
	if err != nil {
		return nil, nil, err
	}
	ts := &targetSpill{inner: tables, sort: s}
	if o, ok := tables.(Observer); ok {
		return observedSpill{targetSpill: ts, Observer: o}, ts, nil
	}
	return ts, ts, nil
}

func (ts *targetSpill) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	t, ok, err := ts.inner.Table(ctx, gen, fv)
	if err != nil || !ok {
		return t, ok, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.sort == nil || ts.err != nil || ts.last != "" && fv.ID <= ts.last {
		return t, true, ts.err
	}
	ts.last = fv.ID
	for _, e := range targetable(t) {
		if err := ts.sort.Add(spilledEndpoint{File: fv.ID, Language: t.Language, Endpoint: e}); err != nil {
			ts.err = err
			return nil, false, err
		}
	}
	return t, true, nil
}

// sorted stops the spilling and returns the run, which the caller closes.
func (ts *targetSpill) sorted() (*pagination.SortedRun[spilledEndpoint], error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	s := ts.sort
	ts.sort = nil
	defer s.Close()
	if ts.err != nil {
		return nil, ts.err
	}
	return s.Sorted()
}

// close releases the sort when sorted was never called.
func (ts *targetSpill) close() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.sort != nil {
		ts.sort.Close()
		ts.sort = nil
	}
}

// spilledTargets answers, for members asked in ascending FileID order, the
// targetable endpoints by node and the language of a member the resolution
// spilled, and false for any other member.
type spilledTargets struct {
	next func() (spilledEndpoint, bool, error)
	stop func()
	cur  spilledEndpoint
	ok   bool
}

func newSpilledTargets(run *pagination.SortedRun[spilledEndpoint]) (*spilledTargets, error) {
	st := &spilledTargets{}
	if run == nil {
		return st, nil
	}
	st.next, st.stop = pull(run)
	var err error
	if st.cur, st.ok, err = st.next(); err != nil {
		st.stop()
		return nil, err
	}
	return st, nil
}

func (st *spilledTargets) close() {
	if st.stop != nil {
		st.stop()
	}
}

func (st *spilledTargets) of(id model.FileID) (byNode map[model.NodeID]Endpoint, language string, ok bool, err error) {
	for st.ok && st.cur.File < id {
		if st.cur, st.ok, err = st.next(); err != nil {
			return nil, "", false, err
		}
	}
	if !st.ok || st.cur.File != id {
		return nil, "", false, nil
	}
	byNode = map[model.NodeID]Endpoint{}
	language = st.cur.Language
	for st.ok && st.cur.File == id {
		byNode[st.cur.Node] = st.cur.Endpoint
		if st.cur, st.ok, err = st.next(); err != nil {
			return nil, "", false, err
		}
	}
	return byNode, language, true, nil
}

// targetableOf is the targetable endpoints by node and the language of the
// member fv, which publication republishes: from sp when the resolution
// spilled them, and otherwise from its table.
func (p *publisher) targetableOf(sp *spilledTargets, fv model.FileVersion) (map[model.NodeID]Endpoint, string, error) {
	byNode, language, ok, err := sp.of(fv.ID)
	if err != nil || ok {
		return byNode, language, err
	}
	t, has, err := p.u.tables.Table(p.ctx, p.u.gen, fv)
	if err != nil {
		return nil, "", err
	}
	if !has {
		return nil, "", internalErr("a fact targets a node of " + fv.Path + ", which has no link table")
	}
	return targetable(t), t.Language, nil
}
