package link

import (
	"cmp"
	"context"
	"slices"

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
// or ECMAScript unit re-derives only the region of nodes (sites and file
// nodes) whose recorded reads or lookups changed, or whose member moved
// (resolveFlow, below), whatever moved. The bindings are then
// diffed against the predecessor's: a file whose facts of any kind changed is
// republished, and so is a file the set of whose targeted declarations,
// modules or parameters changed, since a target's occurrence is published in
// its own file and a carried relation into a newly targeted node needs it
// there. A record's counts are a function of its file's table and facts, so
// a carried file's record holds the counts a fresh read would. Every other file's facts are
// carried, a re-run that changes nothing about a file stopping there.

// The keys of the manifest fields.
const (
	// fieldRustCrate + a crate's name is that crate's root module
	// (eachRustCrate).
	fieldRustCrate = "Cargo.toml\x00crate\x00"
)

// manifestFields is every manifest field the unit's resolution reads, sorted
// and each once: a Go module's path; a Rust crate's name and root; an
// ECMAScript unit's configs after `extends`, its package.json fields and its
// workspace globs (jsManifestFields). No other manifest is read by the
// linker: the others decide only the planner's membership, which a refresh
// compares member by member.
func manifestFields(ctx context.Context, u *unit) ([]manifestField, error) {
	var out []manifestField
	switch u.family {
	case FamilyGo:
		fs, err := goManifestFields(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	case FamilyJavaScript:
		fs, err := jsManifestFields(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	case FamilyRust:
		err := eachRustCrate(ctx, u, func(name, root string) error {
			out = append(out, manifestField{Key: fieldRustCrate + name, Value: root})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	entries, err := entryFields(ctx, u)
	if err != nil {
		return nil, err
	}
	out = append(out, entries...)
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
	// moved collects the members whose table digest moved (a table that is
	// gone included) or that are new with a table, whose own sites resolve
	// again whatever they read; gone the predecessor's members that are no
	// longer members.
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
				if err := rf.moved.Add(fv.ID); err != nil {
					return err
				}
				if digest == "" {
					rf.layout = false
				} else if err := rf.sameInterface(ctx, fv, old); err != nil {
					return err
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
// fixed point over the unit (infer.go), decomposed into nodes: a site node
// owns one site's rules and facts, a file node its member's returns, field
// writes, imports and hierarchy facts. The state keeps, named by identity
// (flowstate.go), every value each node put into a variable (its
// contributions), every variable each node read, and every key of the
// hierarchy and the module index each node looked up, found or not (its
// traces). A refresh maintains the fixed point by delete-and-rederive at
// the granularity of nodes (Gupta, Mumick and Subrahmanian, "Maintaining
// Views Incrementally", SIGMOD 1993), re-running a region Z of nodes from
// the stored contributions of the nodes outside it.
//
// Z starts as every node of each member whose table moved, that is new or
// that is gone, and every node that looked up a key whose group changed (a
// declaration, parameter, export or module added, removed or changed under
// it: the hierarchy and module index are rebuilt and diffed group by group
// when the layout broke, and kept as stored when it holds). Then, until it
// settles:
//
//   - U is every node the stored graph reaches from Z (a flowNode reaches the
//     readers of each variable it contributed to), outside Z; the other
//     nodes are trusted, since nothing they read depends on Z.
//   - Z runs on the trusted nodes' contributions alone (isolated). Each
//     value Z contributed before that neither the isolated run nor a
//     trusted node now puts into its variable (or puts there only in a
//     later stratum) is a removal: its readers outside Z, and everything
//     they reach, join Z, which runs again. A value that cycled from Z
//     through untouched nodes back into Z is not an input of the isolated
//     run, so it cannot support itself once its source is gone.
//   - With no removal, Z runs on every node's contributions outside it
//     (the isolated run stands when U sent nothing into a variable Z
//     read). Each variable whose values, or the stratum a value arrives
//     at, now differ from the stored ones names its readers outside Z,
//     which join Z, and Z runs again; with none the refresh is done.
//
// Every node outside the final Z read exactly what it read before and
// looked up keys that hold what they held, so its stored facts,
// contributions, reads and traces are what a full build gives it, and they
// are carried; Z's are found again. A body edit whose values come out the
// same costs its own member's sites only, and reads no other table.

// Observer, when the Tables a link reads also implement it, is told the
// member path of each site of the flow family the link resolves, as it
// resolves it: the work a build does, site by site.
type Observer interface {
	Resolved(path string)
}

// resolveFlow is a flow-family unit's resolution, in full or, when rf has a
// predecessor's flow state, of the region of nodes its refresh re-derives
// (above).
func resolveFlow(ctx context.Context, u *unit, rf *refresh) (*resolution, error) {
	res := &resolution{}
	ok := false
	defer func() {
		if !ok {
			res.close()
		}
	}()
	var err error
	if res.reads, err = emptyOf(newReadSort(u.dir, compareRead)); err != nil {
		return nil, err
	}
	if res.taken, err = emptyOf(newTakenSort(u.dir, compareTakenFrom)); err != nil {
		return nil, err
	}
	prev := rf.prev
	incremental := prev != nil && prev.h != nil && prev.mods != nil
	// keys collects the changed groups' keys of a refresh.
	var keys *pagination.ExternalSort[string]
	if incremental {
		if keys, err = newKeySort(u.dir); err != nil {
			return nil, err
		}
		defer keys.Close()
	}
	if incremental && rf.layout {
		res.h, res.mods, res.groups = prev.h, prev.mods, prev.groups
	} else {
		if res.h, err = buildHierarchy(ctx, u); err != nil {
			return nil, err
		}
		if res.mods, err = buildModules(ctx, u, res.h); err != nil {
			return nil, err
		}
		if res.groups, err = flowGroups(u.dir, res.h, res.mods); err != nil {
			return nil, err
		}
		if incremental {
			err = diffGroups(prev.groups, res.groups, func(g group) string { return g.Key },
				func(a, b group) bool { return a == b }, keys.Add)
			if err != nil {
				return nil, err
			}
		}
	}

	var pass *flowPass
	var region *pagination.SortedRun[flowNode]
	var fr *flowRefresh
	if incremental {
		if fr, err = newFlowRefresh(u, rf, res.h); err != nil {
			return nil, err
		}
		defer fr.close()
		seeds, err := fr.seeds(keys)
		if err != nil {
			return nil, err
		}
		pass, region, err = fr.rederive(ctx, seeds)
		if err != nil {
			return nil, err
		}
		defer region.Close()
	} else if pass, err = runFlow(ctx, u, res.h, nil, nil); err != nil {
		return nil, err
	}
	defer pass.close()

	facts, err := newIDBindingSort(u.dir)
	if err != nil {
		return nil, err
	}
	defer facts.Close()
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
		return facts.Add(id)
	}
	e := &flowEmit{h: res.h, mods: res.mods, t: pass.t, emit: emit, trace: pass.note}
	if o, ok := u.tables.(Observer); ok {
		e.observe = o.Resolved
	}
	if fr != nil {
		if e.only, err = fr.ordinals(region); err != nil {
			return nil, err
		}
		defer e.only.Close()
	}
	emitFacts := emitECMAScript
	if u.family == FamilyPython {
		emitFacts = emitPython
	}
	if err := emitFacts(ctx, u, e); err != nil {
		return nil, err
	}

	contributions, err := newSort0(u.dir, idContributionCodec, earliestID)
	if err != nil {
		return nil, err
	}
	defer contributions.Close()
	reads, err := newSort0(u.dir, idReadCodec, keepFirst[idRead])
	if err != nil {
		return nil, err
	}
	defer reads.Close()
	traces, err := newSort0(u.dir, nodeTraceCodec, keepFirst[nodeTrace])
	if err != nil {
		return nil, err
	}
	defer traces.Close()
	if fr != nil {
		if err := fr.carry(region, facts, contributions, reads, traces); err != nil {
			return nil, err
		}
	}
	n := &namer{ids: ids}
	err = pass.t.contributions.Each(func(c contribution) error {
		id, err := n.contribution(c)
		if err != nil {
			return err
		}
		return contributions.Add(id)
	})
	if err != nil {
		return nil, err
	}
	err = pass.t.reads.Each(func(r flowRead) error {
		id, err := n.read(r)
		if err != nil {
			return err
		}
		return reads.Add(id)
	})
	if err != nil {
		return nil, err
	}
	looked, err := pass.traces.Sorted()
	if err != nil {
		return nil, err
	}
	defer looked.Close()
	err = looked.Each(func(t ordTrace) error {
		at, err := n.node(t.Node)
		if err != nil {
			return err
		}
		return traces.Add(nodeTrace{Key: t.Key, Node: at})
	})
	if err != nil {
		return nil, err
	}
	if res.bindings, err = facts.Sorted(); err != nil {
		return nil, err
	}
	if res.flowContributions, err = contributions.Sorted(); err != nil {
		return nil, err
	}
	if res.flowReads, err = reads.Sorted(); err != nil {
		return nil, err
	}
	if res.flowTraces, err = traces.Sorted(); err != nil {
		return nil, err
	}
	ok = true
	return res, nil
}

// flowPass is one run of the inference over a region, with the keys its
// nodes looked up, which the emission of the same region adds to.
type flowPass struct {
	t      *types
	traces *pagination.ExternalSort[ordTrace]
}

func (p *flowPass) note(n Ref, k TraceKey) error {
	return p.traces.Add(ordTrace{Key: k.hash(), Node: n})
}

func (p *flowPass) close() {
	if p == nil {
		return
	}
	if p.t != nil {
		p.t.close()
	}
	p.traces.Close()
}

// runFlow runs the inference over the nodes only selects (every node when
// nil), from inputs (stored contributions by ordinal, sorted by entry; none
// when nil), recording every key a flowNode looks up.
func runFlow(ctx context.Context, u *unit, h *hierarchy, only *pagination.SortedRun[Ref],
	inputs *pagination.SortedRun[contribution]) (*flowPass, error) {
	p := &flowPass{}
	var err error
	if p.traces, err = newSort0(u.dir, ordTraceCodec, keepFirst[ordTrace]); err != nil {
		return nil, err
	}
	var in flowInputs
	if inputs != nil {
		in = func(wants *pagination.SortedRun[variable]) (*pagination.SortedRun[contribution], error) {
			return collect(u.dir, contributionCodec, earliest, func(add func(contribution) error) error {
				return join(inputs, wants, func(c contribution, v variable) int { return cmpVar(c.Entry, v) },
					func(c contribution, read []variable) error {
						if c.Entry.Kind != varReturn && len(read) == 0 {
							return nil
						}
						return add(c)
					})
			})
		}
	}
	if p.t, err = inferTypes(ctx, u, h, only, in, p.note); err != nil {
		p.close()
		return nil, err
	}
	return p, nil
}

// flowRefresh is one refresh of a flow-family unit: the predecessor's
// contributions by contributing node, and the identities of this build's
// members and declarations with their ordinals.
type flowRefresh struct {
	u      *unit
	rf     *refresh
	h      *hierarchy
	byNode *pagination.SortedRun[idContribution]
	idMap  *pagination.SortedRun[idRef]
}

func newFlowRefresh(u *unit, rf *refresh, h *hierarchy) (*flowRefresh, error) {
	fr := &flowRefresh{u: u, rf: rf, h: h}
	var err error
	if fr.byNode, err = resort(u.dir, rf.prev.flowContributions, contributionsByNode, contributionsByNode.cmp); err != nil {
		return nil, err
	}
	if fr.idMap, err = idMapOf(u.dir, h.ids); err != nil {
		fr.close()
		return nil, err
	}
	return fr, nil
}

func (fr *flowRefresh) close() {
	fr.byNode.Close()
	fr.idMap.Close()
}

// seeds is every node of each member whose table moved, that is new with a
// table, or that is gone, and every node that looked up one of keys (the
// changed groups).
func (fr *flowRefresh) seeds(keys *pagination.ExternalSort[string]) (*pagination.SortedRun[flowNode], error) {
	changed, err := keys.Sorted()
	if err != nil {
		return nil, err
	}
	defer changed.Close()
	return collect(fr.u.dir, nodeCodec, keepFirst[flowNode], func(add func(flowNode) error) error {
		for _, s := range []*pagination.ExternalSort[model.FileID]{fr.rf.moved, fr.rf.gone} {
			run, err := s.Sorted()
			if err != nil {
				return err
			}
			err = run.Each(func(id model.FileID) error { return add(flowNode{File: id, Index: wholeFile}) })
			run.Close()
			if err != nil {
				return err
			}
		}
		return join(fr.rf.prev.flowTraces, changed, func(t nodeTrace, k string) int { return cmp.Compare(t.Key, k) },
			func(t nodeTrace, hit []string) error {
				if len(hit) == 0 {
					return nil
				}
				return add(t.Node)
			})
	})
}

// rederive re-derives the region z (above) until it settles, and answers
// the last run, which binds z's sites, with z.
func (fr *flowRefresh) rederive(ctx context.Context, z *pagination.SortedRun[flowNode]) (*flowPass, *pagination.SortedRun[flowNode], error) {
	dir := fr.u.dir
	fail := func(err error) (*flowPass, *pagination.SortedRun[flowNode], error) {
		z.Close()
		return nil, nil, err
	}
	if z.Len() == 0 {
		pass, err := fr.run(ctx, z, nil)
		if err != nil {
			return fail(err)
		}
		return pass, z, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		down, err := fr.closure(z)
		if err != nil {
			return fail(err)
		}
		u, err := minus(dir, down, z)
		down.Close()
		if err != nil {
			return fail(err)
		}
		withU, err := unionNodes(dir, z, u)
		if err != nil {
			u.Close()
			return fail(err)
		}
		iso, stale, err := fr.inputs(withU)
		withU.Close()
		if err != nil {
			u.Close()
			return fail(err)
		}
		if grown, err := fr.absorb(z, stale); err != nil || grown != nil {
			u.Close()
			iso.Close()
			if err != nil {
				return fail(err)
			}
			z = grown
			continue
		}
		pass, err := fr.run(ctx, z, iso)
		iso.Close()
		if err != nil {
			u.Close()
			return fail(err)
		}
		removed, err := fr.removals(z, u, pass.t)
		if err != nil {
			u.Close()
			pass.close()
			return fail(err)
		}
		readers, err := fr.readersOutside(removed, z)
		removed.Close()
		if err != nil {
			u.Close()
			pass.close()
			return fail(err)
		}
		if readers.Len() > 0 {
			u.Close()
			pass.close()
			grown, err := fr.closure(readers)
			readers.Close()
			if err != nil {
				return fail(err)
			}
			next, err := unionNodes(dir, z, grown)
			grown.Close()
			if err != nil {
				return fail(err)
			}
			z.Close()
			z = next
			continue
		}
		readers.Close()
		withheld, err := fr.withheld(u, pass.t)
		u.Close()
		if err != nil {
			pass.close()
			return fail(err)
		}
		if withheld {
			pass.close()
			all, stale, err := fr.inputs(z)
			if err != nil {
				return fail(err)
			}
			if grown, err := fr.absorb(z, stale); err != nil || grown != nil {
				all.Close()
				if err != nil {
					return fail(err)
				}
				z = grown
				continue
			}
			pass, err = fr.run(ctx, z, all)
			all.Close()
			if err != nil {
				return fail(err)
			}
		}
		changed, err := fr.changes(z, pass.t)
		if err != nil {
			pass.close()
			return fail(err)
		}
		readers, err = fr.readersOutside(changed, z)
		changed.Close()
		if err != nil {
			pass.close()
			return fail(err)
		}
		if readers.Len() == 0 {
			readers.Close()
			return pass, z, nil
		}
		pass.close()
		next, err := unionNodes(dir, z, readers)
		readers.Close()
		if err != nil {
			return fail(err)
		}
		z.Close()
		z = next
	}
}

// run runs the inference over the nodes of z this build has, from inputs.
func (fr *flowRefresh) run(ctx context.Context, z *pagination.SortedRun[flowNode],
	inputs *pagination.SortedRun[contribution]) (*flowPass, error) {
	only, err := fr.ordinals(z)
	if err != nil {
		return nil, err
	}
	defer only.Close()
	return runFlow(ctx, fr.u, fr.h, only, inputs)
}

var refCodec = codec[Ref]{put: func(w *wbuf, r Ref) { w.ref(r) }, get: func(r *rbuf) Ref { return r.ref() }, cmp: compareRef}

// ordinals is z by this build's ordinals, the nodes of a member that is
// gone left out. Ordinals ascend with FileIDs.
func (fr *flowRefresh) ordinals(z *pagination.SortedRun[flowNode]) (*pagination.SortedRun[Ref], error) {
	return collect(fr.u.dir, refCodec, keepFirst[Ref], func(add func(Ref) error) error {
		next, stop := pull(fr.idMap)
		defer stop()
		cur, ok, err := next()
		if err != nil {
			return err
		}
		return z.Each(func(n flowNode) error {
			for ok && cur.File < n.File {
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			if !ok || cur.File != n.File || cur.Decl != "" {
				return nil
			}
			return add(Ref{File: cur.Ref.File, Index: n.Index})
		})
	})
}

// inputs is every stored contribution of a flowNode excluded does not hold, by
// this build's ordinals and sorted by entry, with the nodes whose stored
// contributions name a member or declaration this build does not have
// (stale), which must re-run.
func (fr *flowRefresh) inputs(excluded *pagination.SortedRun[flowNode]) (*pagination.SortedRun[contribution],
	*pagination.SortedRun[flowNode], error) {
	dir := fr.u.dir
	kept, err := collect(dir, idContributionCodec, nil, func(add func(idContribution) error) error {
		return filterNodes(fr.byNode, excluded, func(c idContribution) flowNode { return c.From }, false, add)
	})
	if err != nil {
		return nil, nil, err
	}
	defer kept.Close()
	stale, err := newSort0(dir, nodeCodec, keepFirst[flowNode])
	if err != nil {
		return nil, nil, err
	}
	defer stale.Close()
	out, err := ordinalOf(dir, fr.idMap, kept, stale.Add)
	if err != nil {
		return nil, nil, err
	}
	gone, err := stale.Sorted()
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return out, gone, nil
}

// absorb is z grown by stale when stale holds a flowNode (z and stale then
// closed), else nil (stale closed).
func (fr *flowRefresh) absorb(z, stale *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	defer stale.Close()
	if stale.Len() == 0 {
		return nil, nil
	}
	grown, err := unionNodes(fr.u.dir, z, stale)
	if err != nil {
		return nil, err
	}
	z.Close()
	return grown, nil
}

// idEV is one value an entry holds, Early when some contribution sends it
// in the first stratum.
type idEV struct {
	Entry idVar
	X     idValue
	Early bool
}

var idEVCodec = codec[idEV]{
	put: func(w *wbuf, v idEV) { w.idVar(v.Entry); w.idValue(v.X); w.flag(v.Early) },
	get: func(r *rbuf) idEV { return idEV{Entry: r.idVar(), X: r.idValue(), Early: r.flag()} },
	cmp: func(a, b idEV) int { return cmp.Or(cmpIDVar(a.Entry, b.Entry), cmpIDValue(a.X, b.X)) },
}

func earlyEV(a, b idEV) (idEV, error) {
	a.Early = a.Early || b.Early
	return a, nil
}

func evOf(c idContribution) idEV { return idEV{Entry: c.Entry, X: c.X, Early: !c.Late} }

// values is what the stored contributions of the nodes set holds (inside)
// or does not hold (!inside) put into each entry, with t's contributions
// when t is set, by entry and value.
func (fr *flowRefresh) values(set *pagination.SortedRun[flowNode], inside bool, t *types) (*pagination.SortedRun[idEV], error) {
	return collect(fr.u.dir, idEVCodec, earlyEV, func(add func(idEV) error) error {
		err := filterNodes(fr.byNode, set, func(c idContribution) flowNode { return c.From }, inside,
			func(c idContribution) error { return add(evOf(c)) })
		if err != nil || t == nil {
			return err
		}
		n := &namer{ids: fr.h.ids}
		return t.contributions.Each(func(c contribution) error {
			id, err := n.contribution(c)
			if err != nil {
				return err
			}
			return add(evOf(id))
		})
	})
}

// removals is every entry a value z contributed before is gone from, or
// arrives at only in a later stratum, when z's values come from t (an
// isolated run) and the stored contributions of the nodes neither z nor u
// holds.
func (fr *flowRefresh) removals(z, u *pagination.SortedRun[flowNode], t *types) (*pagination.SortedRun[idVar], error) {
	dir := fr.u.dir
	old, err := fr.values(z, true, nil)
	if err != nil {
		return nil, err
	}
	defer old.Close()
	zu, err := unionNodes(dir, z, u)
	if err != nil {
		return nil, err
	}
	defer zu.Close()
	now, err := fr.values(zu, false, t)
	if err != nil {
		return nil, err
	}
	defer now.Close()
	return collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return join(old, now, idEVCodec.cmp, func(o idEV, n []idEV) error {
			if len(n) == 0 || o.Early && !n[0].Early {
				return add(o.Entry)
			}
			return nil
		})
	})
}

// changes is every entry whose values, each with its stratum, differ
// between the stored contributions and those of the nodes z does not hold
// with t's.
func (fr *flowRefresh) changes(z *pagination.SortedRun[flowNode], t *types) (*pagination.SortedRun[idVar], error) {
	dir := fr.u.dir
	old, err := collect(dir, idEVCodec, earlyEV, func(add func(idEV) error) error {
		return fr.rf.prev.flowContributions.Each(func(c idContribution) error { return add(evOf(c)) })
	})
	if err != nil {
		return nil, err
	}
	defer old.Close()
	now, err := fr.values(z, false, t)
	if err != nil {
		return nil, err
	}
	defer now.Close()
	return collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return diffEV(old, now, add)
	})
}

// diffEV merges two runs of entry values sorted by entry and value and
// calls changed for each entry whose values, each with its stratum, differ
// between them.
func diffEV(a, b *pagination.SortedRun[idEV], changed func(idVar) error) error {
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
		var c int
		switch {
		case !yok:
			c = -1
		case !xok:
			c = 1
		default:
			c = idEVCodec.cmp(x, y)
		}
		switch {
		case c < 0:
			err = changed(x.Entry)
		case c > 0:
			err = changed(y.Entry)
		case x.Early != y.Early:
			err = changed(x.Entry)
		}
		if err != nil {
			return err
		}
		if c <= 0 {
			if x, xok, err = na(); err != nil {
				return err
			}
		}
		if c >= 0 {
			if y, yok, err = nb(); err != nil {
				return err
			}
		}
	}
	return nil
}

// withheld reports that a flowNode of u contributed before to an entry t's
// run read, which the isolated run did not hand it.
func (fr *flowRefresh) withheld(u *pagination.SortedRun[flowNode], t *types) (bool, error) {
	dir := fr.u.dir
	sent, err := collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return filterNodes(fr.byNode, u, func(c idContribution) flowNode { return c.From }, true,
			func(c idContribution) error { return add(c.Entry) })
	})
	if err != nil {
		return false, err
	}
	defer sent.Close()
	if sent.Len() == 0 {
		return false, nil
	}
	n := &namer{ids: fr.h.ids}
	read, err := collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return t.reads.Each(func(r flowRead) error {
			v, err := n.variable(r.Entry)
			if err != nil {
				return err
			}
			return add(v)
		})
	})
	if err != nil {
		return false, err
	}
	defer read.Close()
	found := false
	err = join(sent, read, cmpIDVar, func(_ idVar, hit []idVar) error {
		found = found || len(hit) > 0
		return nil
	})
	return found, err
}

// readers yields the nodes the stored reads say read an entry of entries.
func (fr *flowRefresh) readers(entries *pagination.SortedRun[idVar], yield func(flowNode) error) error {
	return join(entries, fr.rf.prev.flowReads, func(v idVar, r idRead) int { return cmpIDVar(v, r.Entry) },
		func(_ idVar, rs []idRead) error {
			for _, r := range rs {
				if err := yield(r.Node); err != nil {
					return err
				}
			}
			return nil
		})
}

// readersOutside is the readers of entries z does not hold.
func (fr *flowRefresh) readersOutside(entries *pagination.SortedRun[idVar], z *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	all, err := collect(fr.u.dir, nodeCodec, keepFirst[flowNode], func(add func(flowNode) error) error {
		return fr.readers(entries, add)
	})
	if err != nil {
		return nil, err
	}
	defer all.Close()
	return minus(fr.u.dir, all, z)
}

// forward is the nodes that read an entry a flowNode of frontier contributed to
// before.
func (fr *flowRefresh) forward(frontier *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	dir := fr.u.dir
	entries, err := collect(dir, idVarCodec, keepFirst[idVar], func(add func(idVar) error) error {
		return filterNodes(fr.byNode, frontier, func(c idContribution) flowNode { return c.From }, true,
			func(c idContribution) error { return add(c.Entry) })
	})
	if err != nil {
		return nil, err
	}
	defer entries.Close()
	return collect(dir, nodeCodec, keepFirst[flowNode], func(add func(flowNode) error) error {
		return fr.readers(entries, add)
	})
}

// closure is seeds and every node forward reaches from them, transitively.
func (fr *flowRefresh) closure(seeds *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	dir := fr.u.dir
	reached, err := unionNodes(dir, seeds, nil)
	if err != nil {
		return nil, err
	}
	frontier, err := unionNodes(dir, seeds, nil)
	if err != nil {
		reached.Close()
		return nil, err
	}
	for frontier.Len() > 0 {
		stepped, err := fr.forward(frontier)
		frontier.Close()
		if err != nil {
			reached.Close()
			return nil, err
		}
		frontier, err = minus(dir, stepped, reached)
		stepped.Close()
		if err != nil {
			reached.Close()
			return nil, err
		}
		grown, err := unionNodes(dir, reached, frontier)
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

// bindingNode is the node a stored fact belongs to: a site's, or its
// member's file node for an import or a declaration's fact.
func bindingNode(b idBinding) flowNode {
	if b.Kind.entry() == 0 {
		return flowNode{File: b.SiteFile, Index: b.Site}
	}
	return flowNode{File: b.SiteFile, Index: fileNode}
}

// carry adds to the fresh relations what the nodes outside region keep of
// the predecessor: their facts, contributions, reads and traces.
func (fr *flowRefresh) carry(region *pagination.SortedRun[flowNode], facts *pagination.ExternalSort[idBinding],
	contributions *pagination.ExternalSort[idContribution], reads *pagination.ExternalSort[idRead],
	traces *pagination.ExternalSort[nodeTrace]) error {
	dir, prev := fr.u.dir, fr.rf.prev
	if err := filterNodes(prev.bindings, region, bindingNode, false, facts.Add); err != nil {
		return err
	}
	if err := filterNodes(fr.byNode, region, func(c idContribution) flowNode { return c.From }, false, contributions.Add); err != nil {
		return err
	}
	byNode, err := resort(dir, prev.flowReads, readsByNode, readsByNode.cmp)
	if err != nil {
		return err
	}
	defer byNode.Close()
	if err := filterNodes(byNode, region, func(r idRead) flowNode { return r.Node }, false, reads.Add); err != nil {
		return err
	}
	looked, err := resort(dir, prev.flowTraces, tracesByNode, tracesByNode.cmp)
	if err != nil {
		return err
	}
	defer looked.Close()
	return filterNodes(looked, region, func(t nodeTrace) flowNode { return t.Node }, false, traces.Add)
}

// filterNodes adds every record of run (whose nodes' files ascend) whose
// node set holds, when keep, or does not hold, otherwise. An Index of
// wholeFile in set holds every node of its member.
func filterNodes[T any](run *pagination.SortedRun[T], set *pagination.SortedRun[flowNode], at func(T) flowNode, keep bool,
	add func(T) error) error {
	sel, err := newSelections(set, splitNode)
	if err != nil {
		return err
	}
	defer sel.stop()
	return run.Each(func(v T) error {
		n := at(v)
		s, err := sel.of(n.File)
		if err != nil {
			return err
		}
		if s.has(n.Index) == keep {
			return add(v)
		}
		return nil
	})
}

// unionNodes is the nodes a and b hold (b may be nil).
func unionNodes(dir string, a, b *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	return collect(dir, nodeCodec, keepFirst[flowNode], func(add func(flowNode) error) error {
		if err := a.Each(add); err != nil {
			return err
		}
		return b.Each(add)
	})
}

// minus is the nodes of a that b does not hold.
func minus(dir string, a, b *pagination.SortedRun[flowNode]) (*pagination.SortedRun[flowNode], error) {
	return collect(dir, nodeCodec, nil, func(add func(flowNode) error) error {
		return filterNodes(a, b, func(n flowNode) flowNode { return n }, false, add)
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
