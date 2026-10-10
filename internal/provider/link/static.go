package link

import (
	"cmp"
	"context"
	"errors"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// The resolution of a Java, Go, Rust, C or C++ unit: direct binding, the
// signature-keyed address-taken × indirect-site cross-product without a
// reachability fixed point, and class-hierarchy analysis, for Java over
// classes and interfaces and for the others over interfaces and traits.
//
// Every binding is justified by a scope, an import, a type or a signature:
// a bare name by the scope the language reads it in (Java's and C++'s
// enclosing class, Go's package, C's and C++'s linkage scope and enclosing
// namespaces, Rust's file), a qualified name by the type, package, namespace
// or module it names, a member by its receiver's declared type, and an
// indirect call by its declared function type. The cross-product is one
// ordered merge: the address-taken functions and the indirect sites sorted
// together by signature.
//
// A member's sites and values are resolved together, from its table, the
// hierarchy and the cross-product, and every read beyond its own table is
// recorded (trace.go). A refresh re-runs only the members whose table moved,
// that are new, or whose recorded reads changed: a group of the rebuilt
// indexes that differs from the predecessor's, a manifest field, or the
// address-taken functions of a signature. Every other member keeps its
// predecessor's bindings, reads and address-taken functions.

// taken is one function a member takes as a value, under its signature.
type taken struct {
	Sig    string
	From   model.FileID
	Target target
}

func compareTakenFrom(a, b taken) int {
	return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.Sig, b.Sig), compareTarget(a.Target, b.Target))
}

func compareTakenSig(a, b taken) int {
	return cmp.Or(cmp.Compare(a.Sig, b.Sig), compareTarget(a.Target, b.Target), cmp.Compare(a.From, b.From))
}

func encodeTaken(t taken) ([]byte, error) {
	out := appendString(appendString(nil, t.Sig), string(t.From))
	return appendString(appendString(out, string(t.Target.File)), string(t.Target.Node)), nil
}

func decodeTaken(raw []byte) (taken, error) {
	b := byteReader{b: raw}
	t := taken{Sig: b.str(), From: model.FileID(b.str()), Target: target{File: model.FileID(b.str()), Node: model.NodeID(b.str())}}
	if b.err != nil || len(b.b) != 0 {
		return taken{}, errors.New("an address-taken function is malformed")
	}
	return t, nil
}

func newTakenSort(dir string, order func(a, b taken) int) (*pagination.ExternalSort[taken], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeTaken, decodeTaken, order)
	if err != nil {
		return nil, err
	}
	return s.WithFold(func(a, _ taken) (taken, error) { return a, nil }), nil
}

// sigUse is a site that joins the address-taken functions of Sig: an
// indirect site, which binds each of them, exact, or a Java call of an
// abstract method, which binds the method (Site's target and detail),
// virtual, when there is none. Site is the call fact the join completes.
type sigUse struct {
	Sig      string
	Abstract bool
	Site     idBinding
}

func encodeSigUse(u sigUse) ([]byte, error) {
	b, err := encodeIDBinding(u.Site)
	if err != nil {
		return nil, err
	}
	flag := byte(0)
	if u.Abstract {
		flag = 1
	}
	return append(appendString(nil, u.Sig), append([]byte{flag}, b...)...), nil
}

func decodeSigUse(raw []byte) (sigUse, error) {
	r := byteReader{b: raw}
	u := sigUse{Sig: r.str()}
	flag := r.byte()
	if r.err != nil || flag > 1 {
		return sigUse{}, errors.New("a signature join is malformed")
	}
	site, err := decodeIDBinding(r.b)
	if err != nil {
		return sigUse{}, err
	}
	u.Abstract, u.Site = flag == 1, site
	return u, nil
}

// resolution is what one build's resolution leaves: the bindings by site;
// and, for the static family, the index groups by key, the recorded reads by
// key, the address-taken functions by the member taking them, and the
// hierarchy, which a later build of the same layout reads instead of reading
// every table. A family that records no reads has the three runs empty and
// no hierarchy.
type resolution struct {
	bindings *pagination.SortedRun[idBinding]
	groups   *pagination.SortedRun[group]
	reads    *pagination.SortedRun[read]
	taken    *pagination.SortedRun[taken]
	h        *hierarchy
}

// close releases r. A run or hierarchy r shares with the predecessor's
// state is released by whichever closes first; both closes are idempotent.
func (r *resolution) close() {
	if r != nil {
		r.bindings.Close()
		r.groups.Close()
		r.reads.Close()
		r.taken.Close()
		if r.h != nil {
			_ = r.h.close()
		}
	}
}

// staticRun resolves members of a static-family unit one at a time,
// collecting what each yields.
type staticRun struct {
	ctx context.Context
	u   *unit
	h   *hierarchy
	// bindings, reads, taken and uses are what the members run so far
	// yielded: their bindings other than the cross-product's, their reads,
	// the functions they take and their sites that join a signature.
	bindings *pagination.ExternalSort[idBinding]
	reads    *pagination.ExternalSort[read]
	taken    *pagination.ExternalSort[taken]
	uses     *pagination.ExternalSort[sigUse]
}

func (s *staticRun) open() error {
	var err error
	if s.bindings, err = newIDBindingSort(s.u.dir); err != nil {
		return err
	}
	if s.reads, err = newReadSort(s.u.dir, compareRead); err != nil {
		return err
	}
	if s.taken, err = newTakenSort(s.u.dir, compareTakenFrom); err != nil {
		return err
	}
	s.uses, err = pagination.NewExternalSort(s.u.dir, 0, encodeSigUse, decodeSigUse,
		func(a, b sigUse) int { return cmp.Compare(a.Sig, b.Sig) })
	return err
}

func (s *staticRun) close() {
	for _, c := range []interface{ Close() error }{s.bindings, s.reads, s.taken, s.uses} {
		if c != nil {
			c.Close()
		}
	}
}

// member resolves the sites and values of the member at ordinal file, whose
// table is t, recording each read beyond t.
func (s *staticRun) member(file int32, fv model.FileVersion, t *Table) error {
	seen := map[string]bool{}
	note := func(key TraceKey) error {
		k := key.hash()
		if seen[k] {
			return nil
		}
		seen[k] = true
		return s.reads.Add(read{Key: k, File: fv.ID})
	}
	s.h.record(note)
	defer s.h.record(nil)
	emit := func(b Binding) error {
		if b.Site.File != file {
			return internalErr("a resolver emitted a fact about member " + strconv.Itoa(int(b.Site.File)) +
				" while resolving member " + strconv.Itoa(int(file)))
		}
		id, ok, err := s.h.ids.identify(fv.ID, b)
		if err != nil || !ok {
			return err
		}
		return s.bindings.Add(id)
	}
	signature := func(e entry) error {
		switch e.Tag {
		case 0:
			to, err := s.h.ids.node(e.Ref)
			if err != nil {
				return err
			}
			return s.taken.Add(taken{Sig: e.Key, From: fv.ID, Target: to})
		case 1:
			if err := note(signatureKey(e.Key)); err != nil {
				return err
			}
			return s.uses.Add(sigUse{Sig: e.Key, Site: idBinding{Kind: FactCall, SiteFile: fv.ID, Site: e.Ref.Index,
				Detail: DetailAddressTaken, Dispatch: model.DispatchExact}})
		}
		if err := note(signatureKey(e.Key)); err != nil {
			return err
		}
		to, err := s.h.ids.node(refOf(e.Aux))
		if err != nil {
			return err
		}
		if _, ok := detailCode(e.Sig); !ok {
			return internalErr("a resolver joined an abstract method with the unknown technique " + strconv.Quote(e.Sig))
		}
		return s.uses.Add(sigUse{Sig: e.Key, Abstract: true, Site: idBinding{Kind: FactCall, SiteFile: fv.ID,
			Site: e.Ref.Index, TargetFile: to.File, Target: to.Node, Detail: e.Sig, Dispatch: model.DispatchVirtual}})
	}
	r := staticFile{h: s.h, ctx: s.ctx, file: file, t: t, fc: contextOf(file, t), emit: emit, returns: map[int32][]returned{}}
	for i := range t.Sites {
		if err := r.site(int32(i), signature); err != nil {
			return err
		}
	}
	for i := range t.Values {
		v := &t.Values[i]
		if v.Type.Kind != wire.TypeFunction {
			continue
		}
		err := r.function(v.Caller, v.Type.Name, func(e entry) error {
			if e.Sig == "" {
				return nil
			}
			sig := e.Sig
			if r.fc.family == "go" && v.Type.Index > 0 {
				sig = goMethodExpression(v.Type.Name, sig)
			}
			return signature(entry{Key: sig, Ref: e.Ref})
		})
		if err != nil {
			return err
		}
	}
	if err := r.includeFacts(); err != nil {
		return err
	}
	return s.h.hierarchyFacts(s.ctx, file, t, emit)
}

// members runs every member with a table that run (FileIDs, ascending)
// names, or every member with a table when run is nil, reading only those
// members' tables.
func (s *staticRun) members(run *pagination.SortedRun[model.FileID]) error {
	has := func(model.FileID) (bool, error) { return true, nil }
	if run != nil {
		c, err := newCursor(run)
		if err != nil {
			return err
		}
		defer c.stop()
		has = c.has
	}
	var file int32 = -1
	return s.u.members(func(fv model.FileVersion) error {
		file++
		if err := s.ctx.Err(); err != nil {
			return err
		}
		ok, err := has(fv.ID)
		if err != nil || !ok {
			return err
		}
		t, ok, err := s.u.tables.Table(s.ctx, s.u.gen, fv)
		if err != nil || !ok {
			return err
		}
		return s.member(file, fv, t)
	})
}

// crossProduct binds each signature use of uses (by signature) against the
// address-taken functions of its signature in taken (by signature), into
// out: an indirect site to every one of them, each a call fact of dispatch
// exact (the function the value holds is the one that runs), a Java call of
// an abstract method to that method, virtual, only when there is none. Only
// one signature's functions are held at a time.
func crossProduct(taken *pagination.SortedRun[taken], uses *pagination.SortedRun[sigUse], out *pagination.ExternalSort[idBinding]) error {
	next, stop := pull(taken)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	sig := ""
	var functions []target
	have := false
	return uses.Each(func(u sigUse) error {
		if !have || u.Sig != sig {
			sig, functions, have = u.Sig, functions[:0], true
			for ok && cur.Sig < sig {
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			for ok && cur.Sig == sig {
				if n := len(functions); n == 0 || functions[n-1] != cur.Target {
					functions = append(functions, cur.Target)
				}
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
		}
		if u.Abstract {
			if len(functions) > 0 {
				return nil
			}
			return out.Add(u.Site)
		}
		for _, f := range functions {
			b := u.Site
			b.TargetFile, b.Target = f.File, f.Node
			if err := out.Add(b); err != nil {
				return err
			}
		}
		return nil
	})
}

// resolveStatic is a static-family unit's resolution: of every member on a
// full build, else of the members rf finds must re-run, with every other
// member's taken from rf.prev.
//
// A refresh whose layout is the predecessor's (refresh.layout) has the
// predecessor's hierarchy and groups exactly, since the hierarchy is a
// function of the members' interfaces and the manifest fields alone: it
// reads them from the state and reads no table but those of the members that
// re-run. Any other build builds the hierarchy from every table.
func resolveStatic(ctx context.Context, u *unit, rf *refresh, fields []manifestField) (*resolution, error) {
	res := &resolution{}
	ok := false
	defer func() {
		if !ok {
			res.close()
		}
	}()
	if rf.prev != nil && rf.layout && rf.prev.h != nil {
		res.h, res.groups = rf.prev.h, rf.prev.groups
	} else {
		var err error
		if res.h, err = buildHierarchy(ctx, u); err != nil {
			return nil, err
		}
		if res.groups, err = indexGroups(u.dir, res.h, fields); err != nil {
			return nil, err
		}
	}
	h := res.h
	s := &staticRun{ctx: ctx, u: u, h: h}
	defer s.close()
	if err := s.open(); err != nil {
		return nil, err
	}
	var gone *pagination.SortedRun[model.FileID]
	var err error
	if rf.prev == nil {
		if err := s.members(nil); err != nil {
			return nil, err
		}
	} else {
		if gone, err = rf.rerunStatic(s, res.groups); err != nil {
			return nil, err
		}
		defer gone.Close()
	}
	if err := s.finish(rf.prev, gone, res); err != nil {
		return nil, err
	}
	ok = true
	return res, nil
}

// indexGroups is every group h's indexes hold, with one per manifest field
// key.
func indexGroups(dir string, h *hierarchy, fields []manifestField) (*pagination.SortedRun[group], error) {
	groups, err := newGroupSort(dir)
	if err != nil {
		return nil, err
	}
	defer groups.Close()
	if err := h.ids.groups(h.decls, scopeDecls, groups); err != nil {
		return nil, err
	}
	if err := h.ids.groups(h.links, scopeLinks, groups); err != nil {
		return nil, err
	}
	// fields is sorted by key; two manifests may give one key two values
	// (two crates of one name), which are one group.
	for i := 0; i < len(fields); {
		j := i
		var values []string
		for ; j < len(fields) && fields[j].Key == fields[i].Key; j++ {
			values = append(values, fields[j].Value)
		}
		if err := groups.Add(group{Key: manifestKey(fields[i].Key).hash(), Hash: model.H(groupDomain, values...)}); err != nil {
			return nil, err
		}
		i = j
	}
	return groups.Sorted()
}

// finish completes res from what s ran and, on a refresh, from prev's
// bindings, reads and address-taken functions of every member not in gone
// (the members that re-ran and those that are no longer members).
func (s *staticRun) finish(prev *previous, gone *pagination.SortedRun[model.FileID], res *resolution) error {
	if prev != nil {
		if err := keepExcept(prev.bindings, gone, func(b idBinding) model.FileID { return b.SiteFile }, s.bindings.Add); err != nil {
			return err
		}
		if err := keepExcept(prev.taken, gone, func(t taken) model.FileID { return t.From }, s.taken.Add); err != nil {
			return err
		}
		byFile, err := newReadSort(s.u.dir, func(a, b read) int { return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Key, b.Key)) })
		if err != nil {
			return err
		}
		defer byFile.Close()
		if err := prev.reads.Each(byFile.Add); err != nil {
			return err
		}
		reads, err := byFile.Sorted()
		if err != nil {
			return err
		}
		defer reads.Close()
		if err := keepExcept(reads, gone, func(r read) model.FileID { return r.File }, s.reads.Add); err != nil {
			return err
		}
	}
	var err error
	if res.taken, err = s.taken.Sorted(); err != nil {
		return err
	}
	bySig, err := newTakenSort(s.u.dir, compareTakenSig)
	if err != nil {
		return err
	}
	defer bySig.Close()
	if err := res.taken.Each(bySig.Add); err != nil {
		return err
	}
	takenBySig, err := bySig.Sorted()
	if err != nil {
		return err
	}
	defer takenBySig.Close()
	uses, err := s.uses.Sorted()
	if err != nil {
		return err
	}
	defer uses.Close()
	if err := crossProduct(takenBySig, uses, s.bindings); err != nil {
		return err
	}
	if res.reads, err = s.reads.Sorted(); err != nil {
		return err
	}
	res.bindings, err = s.bindings.Sorted()
	return err
}

// keepExcept adds every entry of run (FileID of each ascending) whose file
// gone does not hold.
func keepExcept[T any](run *pagination.SortedRun[T], gone *pagination.SortedRun[model.FileID], file func(T) model.FileID, add func(T) error) error {
	c, err := newCursor(gone)
	if err != nil {
		return err
	}
	defer c.stop()
	return run.Each(func(v T) error {
		out, err := c.has(file(v))
		if err != nil || out {
			return err
		}
		return add(v)
	})
}

// rerunStatic runs, in s, the members a refresh of a static-family unit must
// resolve again, given groups, the rebuilt indexes' groups, and returns the
// FileIDs whose predecessor's bindings, reads and address-taken functions
// are superseded: those it ran and those that are no longer members.
//
// The members run in two rounds. The first is every member whose table moved
// or that is new, and every member that read a key whose group differs from
// the predecessor's (a declaration added, removed or changed under it, a
// manifest field moved). A member of the first round may take different
// functions as values, so the second is every other member that read the
// address-taken functions of a signature whose takers in the first round,
// or among the members that are gone, changed. A member's values read the
// hierarchy alone, so a second-round member takes the same functions it
// took, and no third round can be needed.
func (rf *refresh) rerunStatic(s *staticRun, groups *pagination.SortedRun[group]) (*pagination.SortedRun[model.FileID], error) {
	dir := rf.u.dir
	keys, err := newKeySort(dir)
	if err != nil {
		return nil, err
	}
	defer keys.Close()
	err = diffGroups(rf.prev.groups, groups, func(g group) string { return g.Key },
		func(a, b group) bool { return a == b }, keys.Add)
	if err != nil {
		return nil, err
	}
	first, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer first.Close()
	moved, err := rf.moved.Sorted()
	if err != nil {
		return nil, err
	}
	defer moved.Close()
	if err := moved.Each(first.Add); err != nil {
		return nil, err
	}
	if err := readersOf(keys, rf.prev.reads, first.Add); err != nil {
		return nil, err
	}
	round1, err := first.Sorted()
	if err != nil {
		return nil, err
	}
	defer round1.Close()
	if err := s.members(round1); err != nil {
		return nil, err
	}

	// The functions the first round took, against those its members and the
	// members that are gone took before.
	fresh, err := s.taken.Sorted()
	s.taken.Close()
	s.taken = nil
	if err != nil {
		return nil, err
	}
	defer fresh.Close()
	if s.taken, err = newTakenSort(dir, compareTakenFrom); err != nil {
		return nil, err
	}
	if err := fresh.Each(s.taken.Add); err != nil {
		return nil, err
	}
	gone, err := rf.gone.Sorted()
	if err != nil {
		return nil, err
	}
	defer gone.Close()
	before, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer before.Close()
	if err := round1.Each(before.Add); err != nil {
		return nil, err
	}
	if err := gone.Each(before.Add); err != nil {
		return nil, err
	}
	replaced, err := before.Sorted()
	if err != nil {
		return nil, err
	}
	defer replaced.Close()
	oldTaken, err := newTakenSort(dir, compareTakenSig)
	if err != nil {
		return nil, err
	}
	defer oldTaken.Close()
	if err := keepOnly(rf.prev.taken, replaced, func(t taken) model.FileID { return t.From }, oldTaken.Add); err != nil {
		return nil, err
	}
	newTaken, err := newTakenSort(dir, compareTakenSig)
	if err != nil {
		return nil, err
	}
	defer newTaken.Close()
	if err := fresh.Each(newTaken.Add); err != nil {
		return nil, err
	}
	oldRun, err := oldTaken.Sorted()
	if err != nil {
		return nil, err
	}
	defer oldRun.Close()
	newRun, err := newTaken.Sorted()
	if err != nil {
		return nil, err
	}
	defer newRun.Close()
	sigs, err := newKeySort(dir)
	if err != nil {
		return nil, err
	}
	defer sigs.Close()
	err = diffGroups(oldRun, newRun, func(t taken) string { return t.Sig }, func(a, b taken) bool { return a == b },
		func(sig string) error { return sigs.Add(signatureKey(sig).hash()) })
	if err != nil {
		return nil, err
	}
	second, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer second.Close()
	// readersOf yields in key order, not FileID order, so the first round's
	// members are dropped from the sorted readers below.
	if err := readersOf(sigs, rf.prev.reads, second.Add); err != nil {
		return nil, err
	}
	readers, err := second.Sorted()
	if err != nil {
		return nil, err
	}
	defer readers.Close()
	round2, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer round2.Close()
	if err := keepExcept(readers, round1, func(id model.FileID) model.FileID { return id }, round2.Add); err != nil {
		return nil, err
	}
	run2, err := round2.Sorted()
	if err != nil {
		return nil, err
	}
	defer run2.Close()
	if err := s.members(run2); err != nil {
		return nil, err
	}
	all, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer all.Close()
	for _, run := range []*pagination.SortedRun[model.FileID]{replaced, run2} {
		if err := run.Each(all.Add); err != nil {
			return nil, err
		}
	}
	return all.Sorted()
}

// newKeySort is a sort of trace key hashes, each once.
func newKeySort(dir string) (*pagination.ExternalSort[string], error) {
	s, err := pagination.NewExternalSort(dir, 0, func(k string) ([]byte, error) { return []byte(k), nil },
		func(b []byte) (string, error) { return string(b), nil }, cmp.Compare[string])
	if err != nil {
		return nil, err
	}
	return s.WithFold(func(a, _ string) (string, error) { return a, nil }), nil
}

// readersOf yields the member of every read in reads (by key) whose key keys
// holds.
func readersOf(keys *pagination.ExternalSort[string], reads *pagination.SortedRun[read], yield func(model.FileID) error) error {
	run, err := keys.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	next, stop := pull(run)
	defer stop()
	key, ok, err := next()
	if err != nil {
		return err
	}
	return reads.Each(func(r read) error {
		for ok && key < r.Key {
			if key, ok, err = next(); err != nil {
				return err
			}
		}
		if ok && key == r.Key {
			return yield(r.File)
		}
		return nil
	})
}

// keepOnly adds every entry of run (FileID of each ascending) whose file
// only holds.
func keepOnly[T any](run *pagination.SortedRun[T], only *pagination.SortedRun[model.FileID], file func(T) model.FileID, add func(T) error) error {
	c, err := newCursor(only)
	if err != nil {
		return err
	}
	defer c.stop()
	return run.Each(func(v T) error {
		in, err := c.has(file(v))
		if err != nil || !in {
			return err
		}
		return add(v)
	})
}
