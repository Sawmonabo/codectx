package link

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A refresh is one build of a unit against its predecessor's state.
//
// The members are classified against the predecessor's records, in one merge
// by FileID: unchanged (same content), content-changed (the bytes moved),
// added and removed; a content-changed member whose stored MemberDigest
// moved has a changed table. Two axes decide two things. A member whose
// content or table changed is republished, since its facts' ranges, content
// hash or endpoints moved, and named replaced. The bindings are found again only when what the
// resolution reads moved: a table, the membership, or a manifest field the
// linker reads (manifestFields). Otherwise the predecessor's bindings stand
// as they are, named by identity, and a body edit that leaves its table's
// digest costs the republication of its own file and nothing else.
//
// When the bindings are found again, a Go, Java, Rust, C or C++ unit
// re-runs only the members whose table moved, that are new, or whose
// recorded reads changed (static.go, trace.go), and keeps every other
// member's bindings; when the membership and manifest fields hold and every
// moved table keeps its interface (layout), it also keeps the predecessor's
// hierarchy and reads only the tables of the members that re-run. A Python
// or ECMAScript unit whose layout holds re-derives only the region of
// members its moved boundary values reach (resolveFlow, below), and any
// other resolves every site. The bindings are then
// diffed against the predecessor's: a file whose facts of any kind changed is
// republished, and so is a file the set of whose targeted declarations,
// modules or parameters changed, since a target's occurrence is published in
// its own file and a carried relation into a newly targeted node needs it
// there. A record's counts are a function of its file's table and facts, so
// a carried file's record holds the counts a fresh read would. Every other file's facts are
// carried, a re-run that changes nothing about a file stopping there.

// The keys of the manifest fields.
const (
	// fieldGoModule is a Go unit's module path (goModulePath).
	fieldGoModule = "go.mod\x00module"
	// fieldRustCrate + a crate's name is that crate's root module
	// (eachRustCrate).
	fieldRustCrate = "Cargo.toml\x00crate\x00"
)

// manifestFields is every manifest field the unit's resolution reads, sorted
// and each once: a Go module's path, and a Rust crate's name and root. No
// other manifest is read by the linker: the others decide only the planner's
// membership, which a refresh compares member by member.
func manifestFields(ctx context.Context, u *unit) ([]manifestField, error) {
	var out []manifestField
	switch u.family {
	case FamilyGo:
		m, err := goModulePath(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, manifestField{Key: fieldGoModule, Value: m})
	case FamilyRust:
		err := eachRustCrate(ctx, u, func(name, root string) error {
			out = append(out, manifestField{Key: fieldRustCrate + name, Value: root})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, compareField)
	return slices.Compact(out), nil
}

// refresh is one build's account of what moved since its predecessor.
type refresh struct {
	u    *unit
	prev *previous
	// resolve reports that the bindings are found again.
	resolve bool
	// layout reports that the unit's hierarchy is its predecessor's: the
	// same members, the same manifest fields, and every member whose table
	// moved keeps its interface (InterfaceDigest), which is all a hierarchy
	// is built from. Only the members whose sites moved then re-run, and no
	// other table is read.
	layout bool
	// changed collects, folded, the FileIDs whose facts are republished
	// and those of the predecessor's members that are gone: every file the
	// carry-over names replaced.
	changed *pagination.ExternalSort[model.FileID]
	// moved collects the members whose table digest moved or that are new
	// with a table, whose own sites resolve again whatever they read; gone
	// the predecessor's members that are no longer members.
	moved, gone *pagination.ExternalSort[model.FileID]
}

// open makes rf's sorts in its unit's directory.
func (rf *refresh) open() error {
	var err error
	if rf.changed, err = newFileIDSort(rf.u.dir); err != nil {
		return err
	}
	if rf.moved, err = newFileIDSort(rf.u.dir); err != nil {
		return err
	}
	rf.gone, err = newFileIDSort(rf.u.dir)
	return err
}

func (rf *refresh) close() {
	for _, s := range []*pagination.ExternalSort[model.FileID]{rf.changed, rf.moved, rf.gone} {
		if s != nil {
			s.Close()
		}
	}
}

func newFileIDSort(dir string) (*pagination.ExternalSort[model.FileID], error) {
	s, err := pagination.NewExternalSort(dir, 0,
		func(id model.FileID) ([]byte, error) { return []byte(id), nil },
		func(b []byte) (model.FileID, error) { return model.FileID(b), nil }, cmp.Compare[model.FileID])
	if err != nil {
		return nil, err
	}
	return s.WithFold(func(a, _ model.FileID) (model.FileID, error) { return a, nil }), nil
}

// classify merges the members with the predecessor's records. digests reads
// each member's stored MemberDigest, empty for a member without a table.
func (rf *refresh) classify(ctx context.Context, digests Digests, fields []manifestField) error {
	rf.resolve = !slices.Equal(fields, rf.prev.fields)
	rf.layout = !rf.resolve
	next, stop := pull(rf.prev.records)
	defer stop()
	old, ok, err := next()
	if err != nil {
		return err
	}
	gone := func() error {
		rf.layout = false
		if old.Digest != "" {
			rf.resolve = true
		}
		if err := rf.changed.Add(old.File); err != nil {
			return err
		}
		if err := rf.gone.Add(old.File); err != nil {
			return err
		}
		old, ok, err = next()
		return err
	}
	err = rf.u.members(func(fv model.FileVersion) error {
		for ok && old.File < fv.ID {
			if err := gone(); err != nil {
				return err
			}
		}
		digest, _, err := digests.Digest(ctx, rf.u.gen, fv)
		if err != nil {
			return err
		}
		if ok && old.File == fv.ID {
			if old.Digest != digest {
				rf.resolve = true
				if digest == "" {
					rf.layout = false
				} else {
					if err := rf.moved.Add(fv.ID); err != nil {
						return err
					}
					if err := rf.sameInterface(ctx, fv, old); err != nil {
						return err
					}
				}
			}
			// A member whose table moved is republished even with its bytes
			// unchanged: its occurrences are published from its table.
			if old.Content != fv.ContentHash || old.Digest != digest {
				if err := rf.changed.Add(fv.ID); err != nil {
					return err
				}
			}
			old, ok, err = next()
			return err
		}
		rf.layout = false
		if digest != "" {
			rf.resolve = true
			if err := rf.moved.Add(fv.ID); err != nil {
				return err
			}
		}
		return rf.changed.Add(fv.ID)
	})
	for err == nil && ok {
		err = gone()
	}
	return err
}

// sameInterface clears rf.layout unless fv's fresh table keeps the interface
// old records. It reads the table only while the layout still holds: a
// member whose table moved re-runs and reads it anyway.
func (rf *refresh) sameInterface(ctx context.Context, fv model.FileVersion, old record) error {
	if !rf.layout {
		return nil
	}
	t, ok, err := rf.u.tables.Table(ctx, rf.u.gen, fv)
	if err != nil {
		return err
	}
	if !ok {
		rf.layout = false
		return nil
	}
	iface, err := t.InterfaceDigest()
	if err != nil {
		return err
	}
	rf.layout = iface == old.Interface
	return nil
}

// diff adds to changed every file whose facts differ between the
// predecessor's and fresh, both by entry: a fact of any kind added, removed
// or changed in its technique, dispatch or reason; and every file whose set
// of targeted nodes (declarations, modules, parameters) differs between the
// predecessor's facts and targets, since a target's occurrence is published
// from its own file. An external target is no file's: its node is published
// on every run.
func (rf *refresh) diff(fresh *pagination.SortedRun[idBinding], targets *pagination.SortedRun[target]) error {
	err := diffGroups(rf.prev.bindings, fresh, func(b idBinding) model.FileID { return b.SiteFile },
		func(a, b idBinding) bool { return a == b }, rf.changed.Add)
	if err != nil {
		return err
	}
	old, err := targetsOf(rf.u.dir, rf.prev.bindings)
	if err != nil {
		return err
	}
	defer old.Close()
	return diffGroups(old, targets, func(t target) model.FileID { return t.File },
		func(a, b target) bool { return a == b }, rf.changed.Add)
}

// diffGroups merges a and b, both sorted with key ascending, and calls changed
// once for each key whose run of entries differs between them: an entry in
// one only, or an entry that is not the same as the other's at its place.
func diffGroups[T any, K cmp.Ordered](a, b *pagination.SortedRun[T], key func(T) K, same func(x, y T) bool,
	changed func(K) error) error {
	na, sa := pull(a)
	defer sa()
	nb, sb := pull(b)
	defer sb()
	x, xok, err := na()
	if err != nil {
		return err
	}
	y, yok, err := nb()
	if err != nil {
		return err
	}
	for xok || yok {
		var k K
		switch {
		case !yok || xok && key(x) < key(y):
			k = key(x)
		default:
			k = key(y)
		}
		differ := false
		for {
			inA, inB := xok && key(x) == k, yok && key(y) == k
			if !inA && !inB {
				break
			}
			if inA != inB || !same(x, y) {
				differ = true
			}
			if inA {
				if x, xok, err = na(); err != nil {
					return err
				}
			}
			if inB {
				if y, yok, err = nb(); err != nil {
					return err
				}
			}
		}
		if differ {
			if err := changed(k); err != nil {
				return err
			}
		}
	}
	return nil
}

// The flow family's resolution (Python, ECMAScript) is one stratified least
// fixed point over the unit (infer.go), whose answer for a member depends on
// the values the other members send it. A refresh maintains it by
// delete-and-rederive at the granularity of members (Gupta, Mumick and
// Subrahmanian, "Maintaining Views Incrementally", SIGMOD 1993): every
// value a member sends across its boundary is stored as a contribution
// (contribution: a callable's returns, an argument into another member's
// parameter, a field write), with the boundary entries each member read
// (flowRead), and a refresh re-derives a region of members from the stored
// contributions of the members outside it.
//
// The region starts as the members whose table moved, closed under every
// path between them in the predecessor's boundary graph (a member sends to
// another when it contributed a value to an entry the other reads): a value
// that cycled from a moved member through untouched ones back to it is
// re-derived with the whole cycle, so nothing the moved member no longer
// produces can support itself through the stored contributions. Once the
// region is re-derived, every entry whose values (and the stratum each
// arrives at) differ from the predecessor's names its readers outside the
// region; when there are none the refresh stops there (an edited member
// whose boundary values come out equal costs its own sites only), and
// otherwise the region grows by those readers and everything they reach in
// the predecessor's graph, and is re-derived again. Every member outside
// the final region read exactly what it read before, so its stored
// bindings, contributions and reads stand.
//
// It applies when the refresh's layout is its predecessor's: then member
// ordinals, declaration indices and every answer of the hierarchy are the
// predecessor's (the hierarchy is a function of the interfaces alone), so
// the stored contributions, positional, mean the same thing, and the stored
// hierarchy is reused. Any other refresh resolves the unit in full.

// resolveFlow is a flow-family unit's resolution, in full or, when the
// refresh's layout holds, of the region its refresh re-derives (above).
func resolveFlow(ctx context.Context, u *unit, rf *refresh) (*resolution, error) {
	res := &resolution{}
	ok := false
	defer func() {
		if !ok {
			res.close()
		}
	}()
	incremental := rf.prev != nil && rf.layout && rf.prev.h != nil
	var err error
	if incremental {
		res.h = rf.prev.h
	} else if res.h, err = buildHierarchy(ctx, u); err != nil {
		return nil, err
	}
	if res.groups, err = emptyOf(newGroupSort(u.dir)); err != nil {
		return nil, err
	}
	if res.reads, err = emptyOf(newReadSort(u.dir, compareRead)); err != nil {
		return nil, err
	}
	if res.taken, err = emptyOf(newTakenSort(u.dir, compareTakenFrom)); err != nil {
		return nil, err
	}
	var fr *flowRefresh
	var t *types
	if incremental {
		if fr, err = newFlowRefresh(ctx, u, rf); err != nil {
			return nil, err
		}
		defer fr.close()
		if t, err = fr.rederive(ctx, res.h); err != nil {
			return nil, err
		}
	} else if t, err = inferTypes(ctx, u, res.h, nil, nil); err != nil {
		return nil, err
	}
	defer t.close()

	facts, err := newIDBindingSort(u.dir)
	if err != nil {
		return nil, err
	}
	defer facts.Close()
	// A resolver may emit from any goroutine; the sort has one producer.
	var mu sync.Mutex
	ids := res.h.ids
	emit := func(b Binding) error {
		file, err := ids.file(b.Site.File)
		if err != nil {
			return err
		}
		id, ok, err := ids.identify(file, b)
		if err != nil || !ok {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		return facts.Add(id)
	}
	emitFacts := emitECMAScript
	if u.family == FamilyPython {
		emitFacts = emitPython
	}
	var only *pagination.SortedRun[int32]
	if fr != nil {
		only = fr.region
	}
	if err := emitFacts(ctx, u, res.h, t, only, emit); err != nil {
		return nil, err
	}
	contributions, err := newSort0(u.dir, contributionCodec, earliest)
	if err != nil {
		return nil, err
	}
	defer contributions.Close()
	reads, err := newSort0(u.dir, flowReadCodec, keepFirst[flowRead])
	if err != nil {
		return nil, err
	}
	defer reads.Close()
	if fr != nil {
		if err := fr.carry(facts, contributions, reads); err != nil {
			return nil, err
		}
	}
	if err := t.contributions.Each(contributions.Add); err != nil {
		return nil, err
	}
	if err := t.reads.Each(reads.Add); err != nil {
		return nil, err
	}
	if res.bindings, err = facts.Sorted(); err != nil {
		return nil, err
	}
	if res.contributions, err = contributions.Sorted(); err != nil {
		return nil, err
	}
	if res.flowReads, err = reads.Sorted(); err != nil {
		return nil, err
	}
	ok = true
	return res, nil
}

// flowRefresh is one refresh of a flow-family unit: the predecessor's
// boundary graph, each relation sorted the ways the region's walks read it,
// and the region, the member ordinals re-derived, ascending.
type flowRefresh struct {
	u  *unit
	rf *refresh
	// byEntry and reads are the predecessor's contributions and reads as
	// stored, by entry; byFrom the contributions by contributing member and
	// readsByFile the reads by reading member.
	byEntry     *pagination.SortedRun[contribution]
	byFrom      *pagination.SortedRun[contribution]
	reads       *pagination.SortedRun[flowRead]
	readsByFile *pagination.SortedRun[flowRead]
	region      *pagination.SortedRun[int32]
}

func newFlowRefresh(ctx context.Context, u *unit, rf *refresh) (*flowRefresh, error) {
	fr := &flowRefresh{u: u, rf: rf, byEntry: rf.prev.contributions, reads: rf.prev.flowReads}
	ok := false
	defer func() {
		if !ok {
			fr.close()
		}
	}()
	var err error
	if fr.byFrom, err = resort(u.dir, fr.byEntry, contributionCodec, func(a, b contribution) int {
		return cmp.Or(cmp.Compare(a.From, b.From), contributionCodec.cmp(a, b))
	}); err != nil {
		return nil, err
	}
	if fr.readsByFile, err = resort(u.dir, fr.reads, flowReadCodec, func(a, b flowRead) int {
		return cmp.Or(cmp.Compare(a.File, b.File), flowReadCodec.cmp(a, b))
	}); err != nil {
		return nil, err
	}
	seeds, err := fr.seeds(ctx)
	if err != nil {
		return nil, err
	}
	defer seeds.Close()
	ahead, err := fr.closure(seeds, fr.forward)
	if err != nil {
		return nil, err
	}
	defer ahead.Close()
	behind, err := fr.closure(seeds, fr.backward)
	if err != nil {
		return nil, err
	}
	defer behind.Close()
	if fr.region, err = fr.both(ahead, behind); err != nil {
		return nil, err
	}
	ok = true
	return fr, nil
}

func (fr *flowRefresh) close() {
	fr.byFrom.Close()
	fr.readsByFile.Close()
	fr.region.Close()
}

// seeds is the ordinals of the members whose table moved (refresh.moved).
func (fr *flowRefresh) seeds(ctx context.Context) (*pagination.SortedRun[int32], error) {
	moved, err := fr.rf.moved.Sorted()
	if err != nil {
		return nil, err
	}
	defer moved.Close()
	c, err := newCursor(moved)
	if err != nil {
		return nil, err
	}
	defer c.stop()
	out, err := newOrdinalSort(fr.u.dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	var file int32 = -1
	err = fr.u.members(func(fv model.FileVersion) error {
		file++
		if err := ctx.Err(); err != nil {
			return err
		}
		in, err := c.has(fv.ID)
		if err != nil || !in {
			return err
		}
		return out.Add(file)
	})
	if err != nil {
		return nil, err
	}
	return out.Sorted()
}

// rederive re-derives the region (above) until no member outside it reads
// a boundary entry whose values moved, and answers the last inference,
// which binds the region's sites.
func (fr *flowRefresh) rederive(ctx context.Context, h *hierarchy) (*types, error) {
	for {
		inputs, err := fr.inputs()
		if err != nil {
			return nil, err
		}
		t, err := inferTypes(ctx, fr.u, h, fr.region, inputs)
		inputs.Close()
		if err != nil {
			return nil, err
		}
		readers, err := fr.dirty(t)
		if err != nil {
			t.close()
			return nil, err
		}
		if readers.Len() == 0 {
			readers.Close()
			return t, nil
		}
		t.close()
		grown, err := fr.closure(readers, fr.forward)
		readers.Close()
		if err != nil {
			return nil, err
		}
		region, err := unionOrdinals(fr.u.dir, fr.region, grown)
		grown.Close()
		if err != nil {
			return nil, err
		}
		fr.region.Close()
		fr.region = region
	}
}

// inputs is what the members outside the region send into it, by entry:
// every stored contribution of a member outside it into a parameter of a
// member inside it, into a field, or of a callable's returns.
func (fr *flowRefresh) inputs() (*pagination.SortedRun[contribution], error) {
	outside, err := newSort0(fr.u.dir, contributionCodec, earliest)
	if err != nil {
		return nil, err
	}
	defer outside.Close()
	if err := keepOutside(fr.byFrom, fr.region, func(c contribution) int32 { return c.From }, outside.Add); err != nil {
		return nil, err
	}
	run, err := outside.Sorted()
	if err != nil {
		return nil, err
	}
	defer run.Close()
	out, err := newSort0(fr.u.dir, contributionCodec, earliest)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	// By entry, a parameter's contributions come first and by owner, which
	// the region's cursor is asked in ascending order.
	in, err := newOrdinals(fr.region)
	if err != nil {
		return nil, err
	}
	defer in.stop()
	err = run.Each(func(c contribution) error {
		if c.Entry.Kind == varParam {
			inside, err := in.has(c.Entry.File)
			if err != nil || !inside {
				return err
			}
		}
		return out.Add(c)
	})
	if err != nil {
		return nil, err
	}
	return out.Sorted()
}

// keepOutside adds every record of run (sorted by member ascending) whose
// member region does not hold.
func keepOutside[T any](run *pagination.SortedRun[T], region *pagination.SortedRun[int32], member func(T) int32,
	add func(T) error) error {
	in, err := newOrdinals(region)
	if err != nil {
		return err
	}
	defer in.stop()
	return run.Each(func(v T) error {
		inside, err := in.has(member(v))
		if err != nil || inside {
			return err
		}
		return add(v)
	})
}

// entryValue is one value an entry holds, early when some contribution
// sends it in the first stratum.
type entryValue struct {
	Entry variable
	X     value
	Early bool
}

// dirty is the members outside the region that read an entry whose values
// t's region moved: the entry's values, each with its stratum, before
// (every stored contribution) and now (the stored ones of the members
// outside the region and t's), compared entry by entry. A parameter is read
// by its own member; any other entry by the members whose stored reads name
// it.
func (fr *flowRefresh) dirty(t *types) (*pagination.SortedRun[int32], error) {
	now, err := newSort0(fr.u.dir, contributionCodec, earliest)
	if err != nil {
		return nil, err
	}
	defer now.Close()
	if err := keepOutside(fr.byFrom, fr.region, func(c contribution) int32 { return c.From }, now.Add); err != nil {
		return nil, err
	}
	if err := t.contributions.Each(now.Add); err != nil {
		return nil, err
	}
	after, err := now.Sorted()
	if err != nil {
		return nil, err
	}
	defer after.Close()
	changed, err := newSort0(fr.u.dir, codec[variable]{put: func(w *wbuf, v variable) { w.variable(v) },
		get: func(r *rbuf) variable { return r.variable() }, cmp: cmpVar}, keepFirst[variable])
	if err != nil {
		return nil, err
	}
	defer changed.Close()
	if err := diffEntries(fr.byEntry, after, changed.Add); err != nil {
		return nil, err
	}
	entries, err := changed.Sorted()
	if err != nil {
		return nil, err
	}
	defer entries.Close()
	readers, err := newOrdinalSort(fr.u.dir)
	if err != nil {
		return nil, err
	}
	defer readers.Close()
	if err := fr.readersOf(entries, readers.Add); err != nil {
		return nil, err
	}
	all, err := readers.Sorted()
	if err != nil {
		return nil, err
	}
	defer all.Close()
	out, err := newOrdinalSort(fr.u.dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if err := keepOutside(all, fr.region, func(f int32) int32 { return f }, out.Add); err != nil {
		return nil, err
	}
	return out.Sorted()
}

// readersOf yields the readers of each entry of entries (ascending): a
// parameter's member, and the members whose stored reads name any other.
func (fr *flowRefresh) readersOf(entries *pagination.SortedRun[variable], yield func(int32) error) error {
	return join(entries, fr.reads, func(v variable, r flowRead) int { return cmpVar(v, r.Entry) },
		func(v variable, rs []flowRead) error {
			if v.Kind == varParam {
				return yield(v.File)
			}
			for _, r := range rs {
				if err := yield(r.File); err != nil {
					return err
				}
			}
			return nil
		})
}

// diffEntries merges two runs of contributions sorted by entry and value
// and calls changed for each entry (ascending, once) whose values, each
// with its stratum, differ between them.
func diffEntries(a, b *pagination.SortedRun[contribution], changed func(variable) error) error {
	na, sa := pull(a)
	defer sa()
	nb, sb := pull(b)
	defer sb()
	// next folds the contributions of one entry and value into one.
	next := func(n func() (contribution, bool, error), head *contribution, have *bool) (entryValue, bool, error) {
		if !*have {
			return entryValue{}, false, nil
		}
		v := entryValue{Entry: head.Entry, X: head.X, Early: !head.Late}
		for {
			c, ok, err := n()
			if err != nil {
				return entryValue{}, false, err
			}
			if !ok || cmpVar(c.Entry, v.Entry) != 0 || cmpValue(c.X, v.X) != 0 {
				*head, *have = c, ok
				return v, true, nil
			}
			v.Early = v.Early || !c.Late
		}
	}
	var ha, hb contribution
	var oka, okb bool
	var err error
	if ha, oka, err = na(); err != nil {
		return err
	}
	if hb, okb, err = nb(); err != nil {
		return err
	}
	x, xok, err := next(na, &ha, &oka)
	if err != nil {
		return err
	}
	y, yok, err := next(nb, &hb, &okb)
	if err != nil {
		return err
	}
	var last variable
	marked := false
	mark := func(v variable) error {
		if marked && cmpVar(last, v) == 0 {
			return nil
		}
		last, marked = v, true
		return changed(v)
	}
	for xok || yok {
		var c int
		switch {
		case !yok:
			c = -1
		case !xok:
			c = 1
		default:
			c = cmp.Or(cmpVar(x.Entry, y.Entry), cmpValue(x.X, y.X))
		}
		switch {
		case c < 0:
			err = mark(x.Entry)
		case c > 0:
			err = mark(y.Entry)
		case x.Early != y.Early:
			err = mark(x.Entry)
		}
		if err != nil {
			return err
		}
		if c <= 0 {
			if x, xok, err = next(na, &ha, &oka); err != nil {
				return err
			}
		}
		if c >= 0 {
			if y, yok, err = next(nb, &hb, &okb); err != nil {
				return err
			}
		}
	}
	return nil
}

// forward yields the members that read an entry a member of frontier
// contributed to in the predecessor.
func (fr *flowRefresh) forward(frontier *pagination.SortedRun[int32], yield func(int32) error) error {
	entries, err := fr.entrySort()
	if err != nil {
		return err
	}
	defer entries.Close()
	err = keepInside(fr.byFrom, frontier, func(c contribution) int32 { return c.From },
		func(c contribution) error { return entries.Add(c.Entry) })
	if err != nil {
		return err
	}
	run, err := entries.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	return fr.readersOf(run, yield)
}

// backward yields the members that contributed in the predecessor to an
// entry a member of frontier read: its stored reads and its parameters.
func (fr *flowRefresh) backward(frontier *pagination.SortedRun[int32], yield func(int32) error) error {
	entries, err := fr.entrySort()
	if err != nil {
		return err
	}
	defer entries.Close()
	err = keepInside(fr.readsByFile, frontier, func(r flowRead) int32 { return r.File },
		func(r flowRead) error { return entries.Add(r.Entry) })
	if err != nil {
		return err
	}
	// By entry, the parameters' contributions come first and by owner.
	in, err := newOrdinals(frontier)
	if err != nil {
		return err
	}
	err = fr.byEntry.Each(func(c contribution) error {
		if c.Entry.Kind != varParam {
			return nil
		}
		inside, err := in.has(c.Entry.File)
		if err != nil || !inside {
			return err
		}
		return entries.Add(c.Entry)
	})
	in.stop()
	if err != nil {
		return err
	}
	run, err := entries.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	return join(run, fr.byEntry, func(v variable, c contribution) int { return cmpVar(v, c.Entry) },
		func(_ variable, cs []contribution) error {
			for _, c := range cs {
				if err := yield(c.From); err != nil {
					return err
				}
			}
			return nil
		})
}

func (fr *flowRefresh) entrySort() (*pagination.ExternalSort[variable], error) {
	return newSort0(fr.u.dir, codec[variable]{put: func(w *wbuf, v variable) { w.variable(v) },
		get: func(r *rbuf) variable { return r.variable() }, cmp: cmpVar}, keepFirst[variable])
}

// closure is seeds and every member step reaches from them, transitively:
// each round steps from the members the last one added, until a round adds
// none.
func (fr *flowRefresh) closure(seeds *pagination.SortedRun[int32],
	step func(*pagination.SortedRun[int32], func(int32) error) error) (*pagination.SortedRun[int32], error) {
	dir := fr.u.dir
	reached, err := unionOrdinals(dir, seeds, nil)
	if err != nil {
		return nil, err
	}
	frontier, err := unionOrdinals(dir, seeds, nil)
	if err != nil {
		reached.Close()
		return nil, err
	}
	for frontier.Len() > 0 {
		next, err := newOrdinalSort(dir)
		if err == nil {
			err = step(frontier, next.Add)
		}
		var stepped *pagination.SortedRun[int32]
		if err == nil {
			stepped, err = next.Sorted()
		}
		if next != nil {
			next.Close()
		}
		frontier.Close()
		if err != nil {
			reached.Close()
			return nil, err
		}
		fresh, err := newOrdinalSort(dir)
		if err == nil {
			err = keepOutside(stepped, reached, func(f int32) int32 { return f }, fresh.Add)
		}
		stepped.Close()
		if err == nil {
			frontier, err = fresh.Sorted()
		}
		if fresh != nil {
			fresh.Close()
		}
		if err != nil {
			reached.Close()
			return nil, err
		}
		grown, err := unionOrdinals(dir, reached, frontier)
		reached.Close()
		if err != nil {
			frontier.Close()
			return nil, err
		}
		reached = grown
	}
	frontier.Close()
	return reached, nil
}

// both is the members a and b both hold.
func (fr *flowRefresh) both(a, b *pagination.SortedRun[int32]) (*pagination.SortedRun[int32], error) {
	out, err := newOrdinalSort(fr.u.dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	if err := keepInside(a, b, func(f int32) int32 { return f }, out.Add); err != nil {
		return nil, err
	}
	return out.Sorted()
}

// carry adds to the fresh relations what the members outside the region
// keep of the predecessor: their facts, contributions and reads.
func (fr *flowRefresh) carry(facts *pagination.ExternalSort[idBinding], contributions *pagination.ExternalSort[contribution],
	reads *pagination.ExternalSort[flowRead]) error {
	ids, err := newFileIDSort(fr.u.dir)
	if err != nil {
		return err
	}
	defer ids.Close()
	in, err := newOrdinals(fr.region)
	if err != nil {
		return err
	}
	var file int32 = -1
	err = fr.u.members(func(fv model.FileVersion) error {
		file++
		inside, err := in.has(file)
		if err != nil || !inside {
			return err
		}
		return ids.Add(fv.ID)
	})
	in.stop()
	if err != nil {
		return err
	}
	region, err := ids.Sorted()
	if err != nil {
		return err
	}
	defer region.Close()
	if err := keepExcept(fr.rf.prev.bindings, region, func(b idBinding) model.FileID { return b.SiteFile }, facts.Add); err != nil {
		return err
	}
	if err := keepOutside(fr.byFrom, fr.region, func(c contribution) int32 { return c.From }, contributions.Add); err != nil {
		return err
	}
	return keepOutside(fr.readsByFile, fr.region, func(r flowRead) int32 { return r.File }, reads.Add)
}

// keepInside adds every record of run (sorted by member ascending) whose
// member set holds.
func keepInside[T any](run *pagination.SortedRun[T], set *pagination.SortedRun[int32], member func(T) int32,
	add func(T) error) error {
	in, err := newOrdinals(set)
	if err != nil {
		return err
	}
	defer in.stop()
	return run.Each(func(v T) error {
		inside, err := in.has(member(v))
		if err != nil || !inside {
			return err
		}
		return add(v)
	})
}

// resort is run sorted by order instead, in dir.
func resort[T any](dir string, run *pagination.SortedRun[T], c codec[T], order func(a, b T) int) (*pagination.SortedRun[T], error) {
	s, err := newSort0(dir, codec[T]{put: c.put, get: c.get, cmp: order}, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := run.Each(s.Add); err != nil {
		return nil, err
	}
	return s.Sorted()
}

// newSort0 is a sort of c's records in dir, folding equal ones by fold
// (none when nil), outside any inference's lifetime.
func newSort0[T any](dir string, c codec[T], fold func(a, b T) (T, error)) (*pagination.ExternalSort[T], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeWith(c), decodeWith(c, "a spilled boundary record"), c.cmp)
	if err != nil {
		return nil, err
	}
	if fold != nil {
		s.WithFold(fold)
	}
	return s, nil
}

// newOrdinalSort is a sort of member ordinals, each once.
func newOrdinalSort(dir string) (*pagination.ExternalSort[int32], error) {
	s, err := pagination.NewExternalSort(dir, 0,
		func(v int32) ([]byte, error) { return binary.BigEndian.AppendUint32(nil, uint32(v)), nil },
		func(b []byte) (int32, error) {
			if len(b) != 4 {
				return 0, errors.New("a member ordinal is malformed")
			}
			return int32(binary.BigEndian.Uint32(b)), nil
		}, cmp.Compare[int32])
	if err != nil {
		return nil, err
	}
	return s.WithFold(func(a, _ int32) (int32, error) { return a, nil }), nil
}

// unionOrdinals is the ordinals a and b hold (b may be nil).
func unionOrdinals(dir string, a, b *pagination.SortedRun[int32]) (*pagination.SortedRun[int32], error) {
	s, err := newOrdinalSort(dir)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := a.Each(s.Add); err != nil {
		return nil, err
	}
	if err := b.Each(s.Add); err != nil {
		return nil, err
	}
	return s.Sorted()
}

// ordinals answers membership in a run of ordinals for ordinals asked in
// ascending order, in one forward pass.
type ordinals struct {
	next func() (int32, bool, error)
	stop func()
	cur  int32
	ok   bool
}

func newOrdinals(run *pagination.SortedRun[int32]) (*ordinals, error) {
	next, stop := pull(run)
	o := &ordinals{next: next, stop: stop}
	var err error
	if o.cur, o.ok, err = next(); err != nil {
		stop()
		return nil, err
	}
	return o, nil
}

func (o *ordinals) has(k int32) (bool, error) {
	for o.ok && o.cur < k {
		var err error
		if o.cur, o.ok, err = o.next(); err != nil {
			return false, err
		}
	}
	return o.ok && o.cur == k, nil
}
