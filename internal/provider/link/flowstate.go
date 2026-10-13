package link

import (
	"cmp"
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The flow family's stored state names everything by identity, so a member
// added or removed renumbers nothing (ADR-0014 decision 3): a flowNode by its
// member's FileID and its index (a site's table index, or fileNode); a
// parameter by its callable's FileID and declaration node and its position;
// a site's result by its FileID and site index; a callable's returns by its
// FileID and declaration node; a field by its name; a value's declaration by
// its FileID and node, a module value by its path. A site index is only ever
// read back for a member whose table did not move: every node of a member
// whose table moved re-runs (refresh.go).
//
// The inference runs on one build's ordinals (Ref), so what it answers is
// named by identity through the build's identity table (namer), and what a
// refresh hands it back (the stored contributions it starts from) is named
// by ordinal again through a merge join with the same table (ordinalOf).

// node is a flowNode of the fixed point (types) by identity.
type flowNode struct {
	File  model.FileID
	Index int32
}

func cmpNode(a, b flowNode) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Index, b.Index))
}

func splitNode(n flowNode) (model.FileID, int32) { return n.File, n.Index }

// idVar is a variable by identity.
type idVar struct {
	Kind  byte
	File  model.FileID
	Decl  model.NodeID
	Index int32
	Name  string
}

func cmpIDVar(a, b idVar) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.File, b.File), cmp.Compare(a.Decl, b.Decl),
		cmp.Compare(a.Index, b.Index), cmp.Compare(a.Name, b.Name))
}

// idValue is a value by identity.
type idValue struct {
	Kind byte
	File model.FileID
	Decl model.NodeID
	Path string
}

func cmpIDValue(a, b idValue) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.File, b.File), cmp.Compare(a.Decl, b.Decl),
		cmp.Compare(a.Path, b.Path))
}

// idContribution is a contribution by identity.
type idContribution struct {
	Entry idVar
	X     idValue
	From  flowNode
	Late  bool
}

// idRead is a flowRead by identity.
type idRead struct {
	Entry idVar
	Node  flowNode
}

// nodeTrace is one key of the hierarchy or the module index a flowNode looked
// up, the key's hash (TraceKey.hash).
type nodeTrace struct {
	Key  string
	Node flowNode
}

func (w *wbuf) node(n flowNode) { w.str(string(n.File)); w.i32(n.Index) }
func (w *wbuf) idVar(v idVar) {
	w.u8(v.Kind)
	w.str(string(v.File))
	w.str(string(v.Decl))
	w.i32(v.Index)
	w.str(v.Name)
}
func (w *wbuf) idValue(x idValue) {
	w.u8(x.Kind)
	w.str(string(x.File))
	w.str(string(x.Decl))
	w.str(x.Path)
}

func (r *rbuf) node() flowNode { return flowNode{File: model.FileID(r.str()), Index: r.i32()} }
func (r *rbuf) idVar() idVar {
	return idVar{Kind: r.u8(), File: model.FileID(r.str()), Decl: model.NodeID(r.str()), Index: r.i32(), Name: r.str()}
}
func (r *rbuf) idValue() idValue {
	return idValue{Kind: r.u8(), File: model.FileID(r.str()), Decl: model.NodeID(r.str()), Path: r.str()}
}

var nodeCodec = codec[flowNode]{put: func(w *wbuf, n flowNode) { w.node(n) }, get: func(r *rbuf) flowNode { return r.node() }, cmp: cmpNode}

var idVarCodec = codec[idVar]{put: func(w *wbuf, v idVar) { w.idVar(v) }, get: func(r *rbuf) idVar { return r.idVar() },
	cmp: cmpIDVar}

// idContributionCodec orders contributions by entry, value and node, the
// stored order.
var idContributionCodec = codec[idContribution]{
	put: func(w *wbuf, c idContribution) { w.idVar(c.Entry); w.idValue(c.X); w.node(c.From); w.flag(c.Late) },
	get: func(r *rbuf) idContribution {
		return idContribution{Entry: r.idVar(), X: r.idValue(), From: r.node(), Late: r.flag()}
	},
	cmp: func(a, b idContribution) int {
		return cmp.Or(cmpIDVar(a.Entry, b.Entry), cmpIDValue(a.X, b.X), cmpNode(a.From, b.From))
	},
}

// contributionsByNode is idContributionCodec ordered by the contributing node first.
var contributionsByNode = codec[idContribution]{put: idContributionCodec.put, get: idContributionCodec.get,
	cmp: func(a, b idContribution) int { return cmp.Or(cmpNode(a.From, b.From), idContributionCodec.cmp(a, b)) }}

// idReadCodec orders reads by entry, then node, the stored order.
var idReadCodec = codec[idRead]{
	put: func(w *wbuf, x idRead) { w.idVar(x.Entry); w.node(x.Node) },
	get: func(r *rbuf) idRead { return idRead{Entry: r.idVar(), Node: r.node()} },
	cmp: func(a, b idRead) int { return cmp.Or(cmpIDVar(a.Entry, b.Entry), cmpNode(a.Node, b.Node)) },
}

var readsByNode = codec[idRead]{put: idReadCodec.put, get: idReadCodec.get,
	cmp: func(a, b idRead) int { return cmp.Or(cmpNode(a.Node, b.Node), cmpIDVar(a.Entry, b.Entry)) }}

// nodeTraceCodec orders traces by key, then node, the stored order.
var nodeTraceCodec = codec[nodeTrace]{
	put: func(w *wbuf, t nodeTrace) { w.str(t.Key); w.node(t.Node) },
	get: func(r *rbuf) nodeTrace { return nodeTrace{Key: r.str(), Node: r.node()} },
	cmp: func(a, b nodeTrace) int { return cmp.Or(cmp.Compare(a.Key, b.Key), cmpNode(a.Node, b.Node)) },
}

var tracesByNode = codec[nodeTrace]{put: nodeTraceCodec.put, get: nodeTraceCodec.get,
	cmp: func(a, b nodeTrace) int { return cmp.Or(cmpNode(a.Node, b.Node), cmp.Compare(a.Key, b.Key)) }}

// ordTrace is a nodeTrace of one build's ordinals.
type ordTrace struct {
	Key  string
	Node Ref
}

var ordTraceCodec = codec[ordTrace]{
	put: func(w *wbuf, t ordTrace) { w.str(t.Key); w.ref(t.Node) },
	get: func(r *rbuf) ordTrace { return ordTrace{Key: r.str(), Node: r.ref()} },
	cmp: func(a, b ordTrace) int { return cmp.Or(compareRef(a.Node, b.Node), cmp.Compare(a.Key, b.Key)) },
}

// collect is the sorted run of what fill adds, folding equal records by fold
// (none when nil).
func collect[T any](dir string, c codec[T], fold func(a, b T) (T, error), fill func(add func(T) error) error) (*pagination.SortedRun[T], error) {
	s, err := newSort0(dir, c, fold)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := fill(s.Add); err != nil {
		return nil, err
	}
	return s.Sorted()
}

// earliestID folds two contributions of one entry, value and node: early
// when either is.
func earliestID(a, b idContribution) (idContribution, error) {
	a.Late = a.Late && b.Late
	return a, nil
}

// namer names one build's ordinals by identity, through its identity table.
type namer struct {
	ids  *idTable
	ord  int32
	id   model.FileID
	have bool
}

func (n *namer) file(ord int32) (model.FileID, error) {
	if n.have && n.ord == ord {
		return n.id, nil
	}
	id, err := n.ids.file(ord)
	if err != nil {
		return "", err
	}
	n.ord, n.id, n.have = ord, id, true
	return id, nil
}

func (n *namer) node(r Ref) (flowNode, error) {
	f, err := n.file(r.File)
	return flowNode{File: f, Index: r.Index}, err
}

func (n *namer) decl(r Ref) (model.FileID, model.NodeID, error) {
	t, err := n.ids.node(r)
	return t.File, t.Node, err
}

func (n *namer) variable(v variable) (idVar, error) {
	switch v.Kind {
	case varParam, varReturn:
		f, d, err := n.decl(Ref{File: v.File, Index: v.Decl})
		return idVar{Kind: v.Kind, File: f, Decl: d, Index: v.Index}, err
	case varResult:
		f, err := n.file(v.File)
		return idVar{Kind: v.Kind, File: f, Index: v.Index}, err
	}
	return idVar{Kind: v.Kind, Name: v.Name}, nil
}

func (n *namer) value(x value) (idValue, error) {
	if x.Kind == valModule {
		return idValue{Kind: x.Kind, Path: x.Path}, nil
	}
	f, d, err := n.decl(x.Ref)
	return idValue{Kind: x.Kind, File: f, Decl: d}, err
}

func (n *namer) contribution(c contribution) (idContribution, error) {
	e, err := n.variable(c.Entry)
	if err != nil {
		return idContribution{}, err
	}
	x, err := n.value(c.X)
	if err != nil {
		return idContribution{}, err
	}
	from, err := n.node(c.From)
	return idContribution{Entry: e, X: x, From: from, Late: c.Late}, err
}

func (n *namer) read(r flowRead) (idRead, error) {
	e, err := n.variable(r.Entry)
	if err != nil {
		return idRead{}, err
	}
	at, err := n.node(r.Node)
	return idRead{Entry: e, Node: at}, err
}

// idRef is one identity of a build with the ordinal it denotes: a member
// (Decl empty, Ref{ordinal, fileNode}) or one of its declarations.
type idRef struct {
	File model.FileID
	Decl model.NodeID
	Ref  Ref
}

func cmpIDRef(a, b idRef) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Decl, b.Decl))
}

var idRefCodec = codec[idRef]{
	put: func(w *wbuf, x idRef) { w.str(string(x.File)); w.str(string(x.Decl)); w.ref(x.Ref) },
	get: func(r *rbuf) idRef {
		return idRef{File: model.FileID(r.str()), Decl: model.NodeID(r.str()), Ref: r.ref()}
	},
	cmp: cmpIDRef,
}

// idMapOf is every member and declaration of ids by identity, sorted, the
// first of any two of one identity kept. It reads the identity table only.
func idMapOf(dir string, ids *idTable) (*pagination.SortedRun[idRef], error) {
	return collect(dir, idRefCodec, keepFirst[idRef], func(add func(idRef) error) error {
		return ids.each(func(ord int32, file model.FileID, decls int32) error {
			if err := add(idRef{File: file, Ref: Ref{File: ord, Index: fileNode}}); err != nil {
				return err
			}
			for i := int32(0); i < decls; i++ {
				t, err := ids.node(Ref{File: ord, Index: i})
				if err != nil {
					return err
				}
				if t.Node == "" {
					continue
				}
				if err := add(idRef{File: file, Decl: t.Node, Ref: Ref{File: ord, Index: i}}); err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// inputRec is a stored contribution part of the way to a build's ordinals.
type inputRec struct {
	C     idContribution
	Entry variable
	X     value
	From  Ref
}

var inputRecCodec = codec[inputRec]{
	put: func(w *wbuf, x inputRec) {
		idContributionCodec.put(w, x.C)
		w.variable(x.Entry)
		w.value(x.X)
		w.ref(x.From)
	},
	get: func(r *rbuf) inputRec {
		return inputRec{C: idContributionCodec.get(r), Entry: r.variable(), X: r.value(), From: r.ref()}
	},
}

// entryKey and valueKey are the identities an inputRec's entry and value are
// translated by, false when it names none (a field; a module value).
func entryKey(x inputRec) (idRef, bool) {
	switch x.C.Entry.Kind {
	case varParam, varReturn, varResult:
		return idRef{File: x.C.Entry.File, Decl: x.C.Entry.Decl}, true
	}
	return idRef{}, false
}

// fromKey is the identity an inputRec's contributing node's member is
// translated by.
func fromKey(x inputRec) (idRef, bool) { return idRef{File: x.C.From.File}, true }

func valueKey(x inputRec) (idRef, bool) {
	if x.C.X.Kind == valModule {
		return idRef{}, false
	}
	return idRef{File: x.C.X.File, Decl: x.C.X.Decl}, true
}

// ordinalOf is run, stored contributions, named by this build's ordinals
// (idMap, from idMapOf), each with its contributing node's (From), sorted
// by entry. A contribution naming a member or declaration the build does
// not have, or made by a node of a member it does not have, is left out and
// its node told to stale: what it derived names something that is gone.
func ordinalOf(dir string, idMap *pagination.SortedRun[idRef], run *pagination.SortedRun[idContribution],
	stale func(flowNode) error) (*pagination.SortedRun[contribution], error) {
	byKey := func(key func(inputRec) (idRef, bool)) codec[inputRec] {
		return codec[inputRec]{put: inputRecCodec.put, get: inputRecCodec.get, cmp: func(a, b inputRec) int {
			ka, _ := key(a)
			kb, _ := key(b)
			return cmp.Or(cmpIDRef(ka, kb), idContributionCodec.cmp(a.C, b.C))
		}}
	}
	first, err := collect(dir, byKey(entryKey), nil, func(add func(inputRec) error) error {
		return run.Each(func(c idContribution) error {
			x := inputRec{C: c, Entry: variable{Kind: c.Entry.Kind, Index: c.Entry.Index, Name: c.Entry.Name},
				X: value{Kind: c.X.Kind, Path: c.X.Path}}
			return add(x)
		})
	})
	if err != nil {
		return nil, err
	}
	defer first.Close()
	second, err := collect(dir, byKey(valueKey), nil, func(add func(inputRec) error) error {
		return translate(first, idMap, entryKey, func(x inputRec, r Ref) (inputRec, bool) {
			x.Entry.File = r.File
			if x.C.Entry.Kind == varResult {
				return x, r.Index == fileNode
			}
			x.Entry.Decl = r.Index
			return x, r.Index != fileNode
		}, add, stale)
	})
	if err != nil {
		return nil, err
	}
	defer second.Close()
	third, err := collect(dir, byKey(fromKey), nil, func(add func(inputRec) error) error {
		return translate(second, idMap, valueKey, func(x inputRec, r Ref) (inputRec, bool) {
			x.X.Ref = r
			return x, r.Index != fileNode
		}, add, stale)
	})
	if err != nil {
		return nil, err
	}
	defer third.Close()
	return collect(dir, contributionCodec, earliest, func(add func(contribution) error) error {
		return translate(third, idMap, fromKey, func(x inputRec, r Ref) (inputRec, bool) {
			x.From = Ref{File: r.File, Index: x.C.From.Index}
			return x, r.Index == fileNode
		}, func(x inputRec) error {
			return add(contribution{Entry: x.Entry, X: x.X, From: x.From, Late: x.C.Late})
		}, stale)
	})
}

// translate merges run, sorted by key, with idMap, sets each record's
// ordinal by set and adds it, or tells stale its node when idMap lacks its
// key or set refuses what the key denotes (a declaration's identity that
// is a member's, a declaration with no flowNode). A record naming no identity
// passes as it is.
func translate(run *pagination.SortedRun[inputRec], idMap *pagination.SortedRun[idRef], key func(inputRec) (idRef, bool),
	set func(inputRec, Ref) (inputRec, bool), add func(inputRec) error, stale func(flowNode) error) error {
	next, stop := pull(idMap)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	return run.Each(func(x inputRec) error {
		k, named := key(x)
		if !named {
			return add(x)
		}
		for ok && cmpIDRef(cur, k) < 0 {
			if cur, ok, err = next(); err != nil {
				return err
			}
		}
		if !ok || cmpIDRef(cur, k) != 0 {
			return stale(x.C.From)
		}
		y, fits := set(x, cur.Ref)
		if !fits {
			return stale(x.C.From)
		}
		return add(y)
	})
}

// The module index of a flow unit, beside its hierarchy: what an import or a
// site through a module looks a name up in, one point lookup each, which
// the looking node records like a hierarchy lookup (scopeModules).
const (
	// keyPyModule + a dotted module name + NUL + a qualified name: a Python
	// declaration that module declares under it, or with nothing after the
	// NUL the module's file (pyModuleEntry: the directory the name is read
	// from in Aux, the record's marks in Flags).
	keyPyModule = "pm\x00"
	// keyESModule + a module path without extension: the ECMAScript member
	// file an import may name by it (moduleBases), Ref{ordinal, fileNode}.
	keyESModule = "em\x00"
	// scopeModules is the trace scope of the module index's keys.
	scopeModules = "modules"
)

// buildModules writes a flow unit's module index: for Python, every
// member's declarations and module under each dotted module name its path
// can be imported as (indexPythonDecls), which reads every table; for
// ECMAScript, every member file under each module path an import may name
// it by, from the hierarchy's file contexts, which reads no table.
func buildModules(ctx context.Context, u *unit, h *hierarchy) (*index, error) {
	s, err := newEntrySort(u.dir)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if u.family == FamilyPython {
		err = u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
			if err := indexPythonRouting(file, t, s.Add); err != nil {
				return err
			}
			return indexPythonDecls(file, t, func(d pyRecord) error { return s.Add(pyModuleEntry(d)) })
		})
	} else {
		var file int32 = -1
		err = u.members(func(model.FileVersion) error {
			file++
			fc, found, err := h.fileContext(file)
			if err != nil || !found {
				return err
			}
			for _, base := range moduleBases(fc.path, false) {
				if err := s.Add(entry{Key: keyESModule + base, Ref: Ref{File: file, Index: fileNode}}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	return writeIndex(u.dir, s)
}

// flowGroups is every group of a flow unit's hierarchy indexes and its
// module index (trace.go).
func flowGroups(dir string, h *hierarchy, mods *index) (*pagination.SortedRun[group], error) {
	groups, err := newGroupSort(dir)
	if err != nil {
		return nil, err
	}
	defer groups.Close()
	for _, ix := range []struct {
		index *index
		scope string
	}{{h.decls, scopeDecls}, {h.links, scopeLinks}, {mods, scopeModules}} {
		if err := h.ids.groups(ix.index, ix.scope, groups); err != nil {
			return nil, err
		}
	}
	return groups.Sorted()
}

// flowEmit is one emission of a flow unit's facts: the nodes only selects
// (every node when nil), from the inference t over h and mods, to emit. When
// trace is set, every lookup of the hierarchy or the module index is told
// to it for the node being emitted, and observe, when set, is told each
// site's member path as the site is emitted.
type flowEmit struct {
	h       *hierarchy
	mods    *index
	t       *types
	only    *pagination.SortedRun[Ref]
	emit    func(Binding) error
	trace   nodeTracer
	observe func(path string)
	cur     Ref
}

// start points the lookups of h and mods at the node being emitted; stop
// ends it. Both may repeat.
func (e *flowEmit) start() {
	if e.trace == nil {
		return
	}
	note := func(k TraceKey) error { return e.trace(e.cur, k) }
	e.h.record(note)
	e.mods.trace = func(key string) error {
		k, err := e.h.ids.traceKey(scopeModules, key)
		if err != nil {
			return err
		}
		return note(k)
	}
}

func (e *flowEmit) stop() {
	if e.trace == nil {
		return
	}
	e.h.record(nil)
	e.mods.trace = nil
}

// at makes n the node being emitted.
func (e *flowEmit) at(n Ref) { e.cur = n }

// site makes the site n of the member at path the node being emitted.
func (e *flowEmit) site(n Ref, path string) {
	e.cur = n
	if e.observe != nil {
		e.observe(path)
	}
}
