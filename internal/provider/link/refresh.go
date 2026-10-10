package link

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"

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
// hierarchy and reads only the tables of the members that re-run; a Python or ECMAScript unit resolves every site, its
// inference being one fixed point over the unit. The bindings are then
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

// halfBinding is a positional fact whose target is already named by
// identity, on its way to its entry's file: Site is its positional entry and
// ID the fact with every field but SiteFile set.
type halfBinding struct {
	Site Ref
	ID   idBinding
}

func encodeHalf(h halfBinding) ([]byte, error) {
	if h.Site.File < 0 || h.Site.Index < 0 {
		return nil, internalErr("a fact on its way to identity is malformed")
	}
	rest, err := encodeIDBinding(h.ID)
	if err != nil {
		return nil, err
	}
	out := binary.AppendUvarint(binary.AppendUvarint(nil, uint64(h.Site.File)), uint64(h.Site.Index))
	return append(out, rest...), nil
}

func decodeHalf(raw []byte) (halfBinding, error) {
	r := byteReader{b: raw}
	site := Ref{File: r.index(), Index: r.index()}
	if r.err != nil {
		return halfBinding{}, errors.New("a fact on its way to identity is malformed")
	}
	id, err := decodeIDBinding(r.b)
	if err != nil {
		return halfBinding{}, err
	}
	return halfBinding{Site: site, ID: id}, nil
}

// identify names run's positional facts (canonical, by compareBinding) by
// identity, by two ordered merges with the members: the facts with a target
// of the unit by target, whose table gives the target's node (a
// declaration's, the module's for a FactImport, the parameter's for a
// FactFlow), then every fact by entry, whose member gives its file. A table
// is read only for a member some fact targets, and is held only while its
// targets are named. A FactFlow into a parameter with no node is dropped.
//
// It is the flow family's hand-off from its resolver's positional facts to
// the facts publication and the state read (resolve): any resolution of the
// family that yields a sorted run of positional facts plugs in here.
func identify(ctx context.Context, u *unit, run *pagination.SortedRun[Binding]) (*pagination.SortedRun[idBinding], error) {
	byTarget, err := newBindingSort(u.dir, compareTargetSite)
	if err != nil {
		return nil, err
	}
	defer byTarget.Close()
	halves, err := pagination.NewExternalSort(u.dir, 0, encodeHalf, decodeHalf,
		func(a, b halfBinding) int { return cmp.Or(compareRef(a.Site, b.Site), compareIDSite(a.ID, b.ID)) })
	if err != nil {
		return nil, err
	}
	defer halves.Close()
	err = run.Each(func(b Binding) error {
		if b.External == "" && b.Kind.hasTarget() {
			return byTarget.Add(b)
		}
		return halves.Add(halfBinding{Site: b.Site, ID: unnamed("", b)})
	})
	if err != nil {
		return nil, err
	}
	targets, err := byTarget.Sorted()
	if err != nil {
		return nil, err
	}
	defer targets.Close()
	next, stop := pull(targets)
	cur, ok, err := next()
	if err != nil {
		stop()
		return nil, err
	}
	var file int32 = -1
	err = u.members(func(fv model.FileVersion) error {
		file++
		if !ok || cur.Target.File != file {
			return nil
		}
		t, has, err := u.tables.Table(ctx, u.gen, fv)
		if err != nil {
			return err
		}
		if !has {
			return untabledRef("target", cur.Target)
		}
		for ok && cur.Target.File == file {
			node, err := tableTarget(t, cur, fv.Path)
			if err != nil {
				return err
			}
			if node != "" {
				h := halfBinding{Site: cur.Site, ID: unnamed("", cur)}
				h.ID.TargetFile, h.ID.Target = fv.ID, node
				if err := halves.Add(h); err != nil {
					return err
				}
			}
			if cur, ok, err = next(); err != nil {
				return err
			}
		}
		return nil
	})
	stop()
	if err != nil {
		return nil, err
	}
	if ok {
		return nil, untabledRef("target", cur.Target)
	}
	sites, err := halves.Sorted()
	if err != nil {
		return nil, err
	}
	defer sites.Close()
	out, err := newIDBindingSort(u.dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	nextSite, stopSites := pull(sites)
	defer stopSites()
	h, more, err := nextSite()
	if err != nil {
		return nil, err
	}
	file = -1
	err = u.members(func(fv model.FileVersion) error {
		file++
		for more && h.Site.File == file {
			id := h.ID
			id.SiteFile = fv.ID
			if err := out.Add(id); err != nil {
				return err
			}
			if h, more, err = nextSite(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if more {
		return nil, untabledRef("site", h.Site)
	}
	return out.Sorted()
}

// tableTarget is the node of b's target in t, its member's table: the
// declaration's, the module's for a FactImport, the parameter's for a
// FactFlow, which is empty when the structural provider minted none.
func tableTarget(t *Table, b Binding, path string) (model.NodeID, error) {
	if b.Kind == FactImport {
		return t.Module.Node, nil
	}
	if b.Target.Index >= int32(len(t.Decls)) {
		return "", outOfTable("target", b.Target, path)
	}
	d := &t.Decls[b.Target.Index]
	if b.Kind != FactFlow {
		return d.Node, nil
	}
	if b.Param >= int32(len(d.Parameters)) {
		return "", internalErr("a fact names parameter " + strconv.Itoa(int(b.Param)) + " past the parameters of " +
			string(d.Node) + " in " + path)
	}
	return d.Parameters[b.Param].Node, nil
}
