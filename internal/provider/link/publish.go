package link

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strconv"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Publication. Every fact names its entry and its target by identity
// (idBinding), and the members are walked in FileID order, so publication is
// the external nodes, then two ordered merges of the facts with the members,
// each reading only the tables of the members it republishes, one at a time:
//
//  0. every external node any fact of the unit targets, carried or fresh,
//     with evidence that names no file: the node is no member's, so the
//     build replaces the unit's index-level bucket and it is published again
//     on every run (externalsOf);
//  1. the nodes the facts target, by target, with the members: a republished
//     member's targeted declarations, module and parameters are found in its
//     table by node and their occurrences published;
//  2. the facts, by entry, with the members: a republished member's call
//     sites publish their `calls` (or their open-site node and the caller's
//     `calls` to it), their argument flows and the occurrences of the
//     variables those read; its imports their `imports` and its dynamic
//     imports their open import sites; its declarations their `extends`,
//     `implements` and `overrides`. Every member's record is taken, from its
//     fresh table when it is republished and from the predecessor's state
//     otherwise.
//
// Every target's node is handed over before any relation of the second
// merge, and a file's own nodes before its relations, so no relation reaches
// the sink ahead of an identity it names. A fact's relation needs only its
// target's node, which the fact carries, so an entry is published without
// reading its target's table.
//
// The republished members are every member on a full build, and otherwise
// the ones a refresh found changed (refresh): content-changed, added, those
// whose facts changed and those whose set of targeted nodes changed. Their
// predecessor's facts are replaced; every other member's are carried over by
// the delta build.

// occurrence is a structural node a published edge names, with the file it
// is declared in.
type occurrence struct {
	Endpoint
	File     model.FileID
	Hash     string
	Language string
}

// publisher publishes one unit's facts.
type publisher struct {
	ctx  context.Context
	req  provider.UnitRequest
	sink provider.Sink
	u    *unit
	// republished, when set, is the FileIDs whose facts are published,
	// ascending, and prev the predecessor's records the other members keep;
	// nil publishes every member's facts.
	republished *pagination.SortedRun[model.FileID]
	prev        *pagination.SortedRun[record]
	// cov is the unit's coverage, summed over the members' records.
	cov     coverage
	records uint64
}

// publish publishes the external nodes, then runs the two merges:
// occurrences of targets, then entries, and returns every member's record,
// by FileID.
func (p *publisher) publish(bindings *pagination.SortedRun[idBinding], targets *pagination.SortedRun[target],
	externals *pagination.SortedRun[string]) (*pagination.SortedRun[record], error) {
	if err := p.publishExternals(externals); err != nil {
		return nil, err
	}
	if err := p.publishTargets(targets); err != nil {
		return nil, err
	}
	return p.publishSites(bindings)
}

// externalBatch is how many external node facts one hand-off to the sink
// carries.
const externalBatch = 256

// publishExternals publishes the node of every external key of run.
func (p *publisher) publishExternals(run *pagination.SortedRun[string]) error {
	var batch []model.NodeFact
	err := run.Each(func(key string) error {
		n, c, ck, err := p.externalNode(key)
		if err != nil {
			return err
		}
		batch = append(batch, model.NodeFact{Node: n, CanonicalKey: ck,
			Evidence: []model.Evidence{p.evidence(n.ID, "", "", "", nil, c.NativeKey, "", "", 0)}})
		if len(batch) < externalBatch {
			return nil
		}
		err = p.put(batch, nil)
		batch = nil
		return err
	})
	if err != nil {
		return err
	}
	return p.put(batch, nil)
}

// externalNode is the node of the external key key, as the request's
// resolver mints it from externalCandidate, with its candidate and canonical
// key.
func (p *publisher) externalNode(key string) (model.Node, model.NodeCandidate, string, error) {
	c, ok := externalCandidate(key)
	if !ok {
		return model.Node{}, c, "", internalErr("a fact targets the malformed external key " + strconv.Quote(key))
	}
	res, err := p.req.Resolver.Resolve(p.ctx, c)
	if err != nil {
		return model.Node{}, c, "", err
	}
	meta, err := json.Marshal(model.ExternalMetadata{Resolution: model.ResolutionExternal})
	if err != nil {
		return model.Node{}, c, "", internalErr("external node metadata: " + err.Error())
	}
	n := res.Node
	n.Metadata = meta
	return n, c, res.CanonicalKey, nil
}

// cursor answers, for FileIDs asked in ascending order, whether run holds
// each.
type cursor struct {
	next func() (model.FileID, bool, error)
	stop func()
	cur  model.FileID
	ok   bool
}

func newCursor(run *pagination.SortedRun[model.FileID]) (*cursor, error) {
	next, stop := pull(run)
	c := &cursor{next: next, stop: stop}
	var err error
	c.cur, c.ok, err = next()
	if err != nil {
		stop()
		return nil, err
	}
	return c, nil
}

func (c *cursor) has(id model.FileID) (bool, error) {
	for c.ok && c.cur < id {
		var err error
		if c.cur, c.ok, err = c.next(); err != nil {
			return false, err
		}
	}
	return c.ok && c.cur == id, nil
}

// republishes returns the answer, member by member in ascending order, to
// whether the member's facts are published, and the function that stops it.
func (p *publisher) republishes() (func(model.FileID) (bool, error), func(), error) {
	if p.republished == nil {
		return func(model.FileID) (bool, error) { return true, nil }, func() {}, nil
	}
	c, err := newCursor(p.republished)
	if err != nil {
		return nil, nil, err
	}
	return c.has, c.stop, nil
}

// targetable is every node of t a fact can target, by node: its
// declarations (the first of each node), its module and each parameter with
// a node.
func targetable(t *Table) map[model.NodeID]Endpoint {
	byNode := make(map[model.NodeID]Endpoint, len(t.Decls)+1)
	add := func(e Endpoint) {
		if _, ok := byNode[e.Node]; !ok && e.Node != "" {
			byNode[e.Node] = e
		}
	}
	for i := range t.Decls {
		add(t.Decls[i].Endpoint)
	}
	add(t.Module)
	for i := range t.Decls {
		for _, pa := range t.Decls[i].Parameters {
			add(pa.Endpoint)
		}
	}
	return byNode
}

// publishTargets is the first merge.
func (p *publisher) publishTargets(targets *pagination.SortedRun[target]) error {
	next, stop := pull(targets)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	republished, stopRep, err := p.republishes()
	if err != nil {
		return err
	}
	defer stopRep()
	err = p.u.members(func(fv model.FileVersion) error {
		if ok && cur.File < fv.ID {
			return notMember("target", cur.File)
		}
		if !ok || cur.File != fv.ID {
			return nil
		}
		publish, err := republished(fv.ID)
		if err != nil {
			return err
		}
		var t *Table
		var byNode map[model.NodeID]Endpoint
		if publish {
			var has bool
			if t, has, err = p.u.tables.Table(p.ctx, p.u.gen, fv); err != nil {
				return err
			}
			if !has {
				return internalErr("a fact targets a node of " + fv.Path + ", which has no link table")
			}
			byNode = targetable(t)
		}
		var nodes []model.NodeFact
		for ok && cur.File == fv.ID {
			if publish {
				e, found := byNode[cur.Node]
				if !found {
					return internalErr("a fact targets node " + string(cur.Node) + ", which the link table of " + fv.Path + " does not declare")
				}
				nodes = append(nodes, p.occurrenceFact(occurrenceOf(fv, t, e)))
			}
			if cur, ok, err = next(); err != nil {
				return err
			}
		}
		return p.put(nodes, nil)
	})
	if err != nil {
		return err
	}
	if ok {
		return notMember("target", cur.File)
	}
	return nil
}

// publishSites is the second merge.
func (p *publisher) publishSites(bindings *pagination.SortedRun[idBinding]) (*pagination.SortedRun[record], error) {
	next, stop := pull(bindings)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return nil, err
	}
	republished, stopRep, err := p.republishes()
	if err != nil {
		return nil, err
	}
	defer stopRep()
	var prev func() (record, bool, error)
	var old record
	var hasOld bool
	if p.prev != nil {
		var stopPrev func()
		prev, stopPrev = pull(p.prev)
		defer stopPrev()
		if old, hasOld, err = prev(); err != nil {
			return nil, err
		}
	}
	records, err := newRecordSort(p.u.dir)
	if err != nil {
		return nil, err
	}
	defer records.Close()
	var group []idBinding
	err = p.u.members(func(fv model.FileVersion) error {
		if ok && cur.SiteFile < fv.ID {
			return notMember("entry", cur.SiteFile)
		}
		for hasOld && old.File < fv.ID {
			if old, hasOld, err = prev(); err != nil {
				return err
			}
		}
		publish, err := republished(fv.ID)
		if err != nil {
			return err
		}
		if !publish {
			if !hasOld || old.File != fv.ID {
				return internalErr("the member " + fv.Path + " is carried over but the predecessor's state holds no record of it")
			}
			for ok && cur.SiteFile == fv.ID {
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			return p.keep(records, old)
		}
		rec := record{File: fv.ID, Content: fv.ContentHash}
		t, has, err := p.u.tables.Table(p.ctx, p.u.gen, fv)
		if err != nil {
			return err
		}
		if !has {
			if ok && cur.SiteFile == fv.ID {
				return internalErr("a fact names an entry of " + fv.Path + ", which has no link table")
			}
			return p.keep(records, rec)
		}
		if rec.Interface, err = t.InterfaceDigest(); err != nil {
			return err
		}
		if rec.Sites, err = t.SitesDigest(); err != nil {
			return err
		}
		rec.Digest, rec.Tabled, rec.Dropped = memberDigest(rec.Interface, rec.Sites, t.Dropped), true, int64(t.Dropped)
		f := filePublication{p: p, fv: fv, t: t, seen: map[model.NodeID]bool{}}
		// in reports that the next fact is this file's, about an entry of
		// the entry table entry.
		in := func(entry int) bool { return ok && cur.SiteFile == fv.ID && cur.Kind.entry() == entry }
		// groupOf gathers this file's facts about entry i of the entry table
		// entry, which the order puts next.
		groupOf := func(entry int, i int32) error {
			group = group[:0]
			for in(entry) && cur.Site == i {
				group = append(group, cur)
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			return nil
		}
		// past refuses a fact of this file about an entry past the end n of
		// the entry table entry.
		past := func(entry, n int) error {
			if in(entry) {
				return internalErr("a fact names entry " + strconv.Itoa(int(cur.Site)) + " of a table of " +
					strconv.Itoa(n) + " entries in the link table of " + fv.Path)
			}
			return nil
		}
		for i := range t.Sites {
			if err := groupOf(0, int32(i)); err != nil {
				return err
			}
			if err := f.site(int32(i), group); err != nil {
				return err
			}
		}
		if err := past(0, len(t.Sites)); err != nil {
			return err
		}
		for in(1) && cur.Site < int32(len(t.Imports)) {
			i := cur.Site
			if err := groupOf(1, i); err != nil {
				return err
			}
			f.imports(&t.Imports[i], group)
		}
		if err := past(1, len(t.Imports)); err != nil {
			return err
		}
		if err := f.dynamicImports(); err != nil {
			return err
		}
		for in(2) && cur.Site < int32(len(t.Decls)) {
			i := cur.Site
			if err := groupOf(2, i); err != nil {
				return err
			}
			if err := f.hierarchy(&t.Decls[i], group); err != nil {
				return err
			}
		}
		if err := past(2, len(t.Decls)); err != nil {
			return err
		}
		rec.Counts = f.counts
		if err := p.put(f.nodes, f.rels); err != nil {
			return err
		}
		return p.keep(records, rec)
	})
	if err != nil {
		return nil, err
	}
	if ok {
		return nil, notMember("entry", cur.SiteFile)
	}
	return records.Sorted()
}

// keep adds rec to records and to the unit's coverage.
func (p *publisher) keep(records *pagination.ExternalSort[record], rec record) error {
	if !rec.Tabled {
		p.cov.untabled++
	}
	p.cov.dropped += rec.Dropped
	p.cov.add(rec.Counts)
	return records.Add(rec)
}

// filePublication is what one file's entries publish, handed to the sink
// once the file's last entry is done, and the counts its record keeps.
type filePublication struct {
	p      *publisher
	fv     model.FileVersion
	t      *Table
	seen   map[model.NodeID]bool
	nodes  []model.NodeFact
	rels   []model.RelationFact
	counts counts
}

// site publishes the facts of site i, group: one `calls` per distinct
// target (a declaration or an external method), each with the occurrence's
// dispatch and, when there are several, the set size; or, when the site has
// no call fact and no FactExternalSite, its open-site node and the caller's
// `calls` to it; and, for each FactFlow, one `data_flows_to` from each
// variable the argument reads to the parameter. It counts the site bound,
// external, computed or open.
func (f *filePublication) site(i int32, group []idBinding) error {
	s := &f.t.Sites[i]
	caller, err := f.caller(s.Caller)
	if err != nil {
		return err
	}
	// Two facts of one target are one target, so a repeated identity is
	// neither a second target nor a larger set.
	type to struct {
		node     model.NodeID
		external string
	}
	var calls []idBinding
	distinct := map[to]bool{}
	external, bound := false, false
	reason := model.OpenSiteReason("")
	for _, b := range group {
		switch b.Kind {
		case FactCall:
			if k := (to{b.Target, b.External}); !distinct[k] {
				distinct[k] = true
				calls = append(calls, b)
				bound = bound || b.Target != ""
			}
		case FactExternalSite:
			external = true
		case FactOpenSite:
			reason = b.Reason
		}
	}
	f.counts.Sites++
	switch {
	case bound:
		f.counts.Bound++
	case len(calls) > 0 || external:
		f.counts.External++
	case s.Computed:
		f.counts.Computed++
	default:
		f.counts.Open++
	}
	key := callsiteKey(f.fv.Path, s.NameStart, s.NameEnd)
	switch {
	case len(calls) > 0:
		f.node(occurrenceOf(f.fv, f.t, caller))
		var set int64
		if len(calls) >= 2 {
			set = int64(len(calls))
		}
		for _, b := range calls {
			target, err := f.target(b)
			if err != nil {
				return err
			}
			f.relation(caller.Node, model.RelCalls, target, located(s.Range), key, b.Detail, b.Dispatch, set)
		}
	case !external:
		if err := f.openSite(s, caller, cmp.Or(reason, openReason(s))); err != nil {
			return err
		}
	}
	for _, b := range group {
		if b.Kind != FactFlow {
			continue
		}
		if b.Arg >= int32(len(s.Arguments)) {
			return internalErr("a flow names argument " + strconv.Itoa(int(b.Arg)) + " past the arguments of a site of " + f.fv.Path)
		}
		a := &s.Arguments[b.Arg]
		read := map[model.NodeID]bool{}
		for _, r := range a.Reads {
			if r.Node == "" || read[r.Node] {
				continue
			}
			read[r.Node] = true
			f.node(occurrenceOf(f.fv, f.t, r))
			f.relation(r.Node, model.RelDataFlowsTo, b.Target, located(a.Range), key, b.Detail, "", 0)
		}
	}
	return nil
}

// target is the node b targets: its target's, or its external node's.
func (f *filePublication) target(b idBinding) (model.NodeID, error) {
	if b.External == "" {
		return b.Target, nil
	}
	n, _, _, err := f.p.externalNode(b.External)
	return n.ID, err
}

// caller is the declaration at index c of the file's table, -1 its module.
func (f *filePublication) caller(c int32) (Endpoint, error) {
	if c < 0 {
		return f.t.Module, nil
	}
	if c >= int32(len(f.t.Decls)) {
		return Endpoint{}, internalErr("a link table entry of " + f.fv.Path + " names a caller that is not a declaration of its file")
	}
	return f.t.Decls[c].Endpoint, nil
}

// openReason is the reason a site no fact binds is open, by its form: a
// computed callee is computed; a member or static call's receiver has no
// recovered type; a call through a value is a value the analysis does not
// follow; a bare name or a construction names nothing a visible scope
// declares.
func openReason(s *Site) model.OpenSiteReason {
	switch {
	case s.Computed:
		return model.ReasonComputed
	case s.Shape == wire.ShapeMember || s.Shape == wire.ShapeStatic:
		return model.ReasonReceiverUntyped
	case s.Shape == wire.ShapeIndirect:
		return model.ReasonValueUntracked
	}
	return model.ReasonGlobalUnbound
}

// openShape is the call shape of an open site's metadata.
func openShape(s *Site) model.CallShape {
	switch {
	case s.Computed || s.Shape == wire.ShapeIndirect:
		return model.CallShapeValue
	case s.Shape == wire.ShapeMember || s.Shape == wire.ShapeStatic:
		return model.CallShapeMember
	case s.Shape == wire.ShapeConstructor:
		return model.CallShapeConstruct
	}
	return model.CallShapeBare
}

// openSite publishes site s's open-site node, located at the site and named
// by its callee as written, and the caller's `calls` to it, with reason. The
// node is keyed in the file's scope by "open:" and the site's call-site key,
// or, for a computed callee, which has no callee token, "open:computed:", the
// path and the site's bytes; the edge's evidence carries the call-site key,
// and for a computed callee the node's key.
func (f *filePublication) openSite(s *Site, caller Endpoint, reason model.OpenSiteReason) error {
	path := f.fv.Path
	key := callsiteKey(path, s.NameStart, s.NameEnd)
	native := fitKey("open:" + rawCallsiteKey(path, s.NameStart, s.NameEnd))
	if s.Computed {
		native = fitKey("open:computed:" + path + ":" + byteSpan(s.Range))
		key = native
	}
	kind := model.NodeFunction
	if s.Shape == wire.ShapeMember || s.Shape == wire.ShapeStatic {
		kind = model.NodeMethod
	}
	meta, err := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved, Reason: reason,
		Shape: openShape(s), Arguments: int64(max(len(s.Args), len(s.Arguments)))})
	if err != nil {
		return internalErr("open site metadata: " + err.Error())
	}
	n, err := f.located(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + path, NativeKey: native,
		Kind: kind, Language: f.t.Language, Name: nodeName(s.Callee, "call")}, s.Range, meta)
	if err != nil {
		return err
	}
	f.node(occurrenceOf(f.fv, f.t, caller))
	f.relation(caller.Node, model.RelCalls, n, located(s.Range), key, "", "", 0)
	return nil
}

// imports publishes the file module's `imports` to the module of each
// distinct member group resolves imp to, each with the set size when there
// are several, located at the import, and counts imp resolved when there is
// one.
func (f *filePublication) imports(imp *Import, group []idBinding) {
	var targets []idBinding
	distinct := map[model.NodeID]bool{}
	for _, b := range group {
		if b.Kind == FactImport && !distinct[b.Target] {
			distinct[b.Target] = true
			targets = append(targets, b)
		}
	}
	if len(targets) == 0 {
		return
	}
	f.counts.Imports++
	f.node(occurrenceOf(f.fv, f.t, f.t.Module))
	var set int64
	if len(targets) >= 2 {
		set = int64(len(targets))
	}
	for _, b := range targets {
		f.relation(f.t.Module.Node, model.RelImports, b.Target, located(imp.Range), "", b.Detail, "", set)
	}
}

// dynamicImports publishes each dynamic import's open import site, located at
// the import, named "import" and keyed in the file's scope by "openimport:",
// the path and the import's bytes, and the file module's `imports` to it.
func (f *filePublication) dynamicImports() error {
	path := f.fv.Path
	for i := range f.t.DynamicImports {
		d := &f.t.DynamicImports[i]
		f.counts.OpenImports++
		meta, err := openImportMetadata(d.Fragments)
		if err != nil {
			return err
		}
		n, err := f.located(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + path,
			NativeKey: fitKey("openimport:" + path + ":" + byteSpan(d.Range)), Kind: model.NodeModule,
			Language: f.t.Language, Name: "import"}, d.Range, meta)
		if err != nil {
			return err
		}
		f.node(occurrenceOf(f.fv, f.t, f.t.Module))
		f.relation(f.t.Module.Node, model.RelImports, n, located(d.Range), "", "", "", 0)
	}
	return nil
}

// openImportMetadata is an open import site's metadata with fragments, the
// path's literal parts in source order: every one, from the first, while the
// node's metadata bound holds them and each is within a path's bound, and an
// empty list, never a missing one, when none is.
func openImportMetadata(fragments []string) ([]byte, error) {
	m := model.OpenImportSiteMetadata{Resolution: model.ResolutionUnresolved, Reason: model.ReasonComputed,
		Fragments: []string{}}
	meta, err := json.Marshal(m)
	if err != nil {
		return nil, internalErr("open import site metadata: " + err.Error())
	}
	for _, frag := range fragments {
		if frag == "" || len(frag) > model.MaxPathBytes {
			break
		}
		m.Fragments = append(m.Fragments, frag)
		wider, err := json.Marshal(m)
		if err != nil {
			return nil, internalErr("open import site metadata: " + err.Error())
		}
		if len(wider) > model.MaxMetadataBytes {
			break
		}
		meta = wider
	}
	return meta, nil
}

// hierarchy publishes declaration d's `extends`, `implements` and
// `overrides`, group, each to its target's node or its external node,
// located at the declaration.
func (f *filePublication) hierarchy(d *Decl, group []idBinding) error {
	for _, b := range group {
		var kind model.RelationKind
		switch b.Kind {
		case FactExtends:
			kind = model.RelExtends
		case FactImplements:
			kind = model.RelImplements
		case FactOverrides:
			kind = model.RelOverrides
		default:
			continue
		}
		target, err := f.target(b)
		if err != nil {
			return err
		}
		f.node(occurrenceOf(f.fv, f.t, d.Endpoint))
		f.relation(d.Node, kind, target, located(d.Range), "", b.Detail, "", 0)
	}
	return nil
}

// located mints c as a node of this file located at r, with metadata meta,
// publishes it once and returns its identity. It is keyed by its scope and
// native key alone, so its identity is the site's and not its bytes'; it is
// located in the file, so its fact shares the file's retention bucket: a
// delta that republishes the file replaces it, and one that carries the file
// carries it.
func (f *filePublication) located(c model.NodeCandidate, r model.SourceRange, meta []byte) (model.NodeID, error) {
	res, err := f.p.req.Resolver.Resolve(f.p.ctx, c)
	if err != nil {
		return "", err
	}
	rng := located(r)
	n := res.Node
	n.FileID, n.ContentHash, n.Range, n.Metadata = f.fv.ID, f.fv.ContentHash, rng, meta
	if !f.seen[n.ID] {
		f.seen[n.ID] = true
		f.nodes = append(f.nodes, model.NodeFact{Node: n, CanonicalKey: res.CanonicalKey,
			Evidence: []model.Evidence{f.p.evidence(n.ID, "", f.fv.ID, f.fv.ContentHash, rng, c.NativeKey, "", "", 0)}})
	}
	return n.ID, nil
}

// node publishes o's occurrence once per file.
func (f *filePublication) node(o occurrence) {
	if f.seen[o.Node] {
		return
	}
	f.seen[o.Node] = true
	f.nodes = append(f.nodes, f.p.occurrenceFact(o))
}

// relation publishes one occurrence of an edge at rng, with its native key,
// detail, dispatch and set size. Two occurrences of one source, kind and
// target publish one canonical edge, each with its own evidence row.
func (f *filePublication) relation(from model.NodeID, kind model.RelationKind, to model.NodeID, rng *model.SourceRange,
	key, detail string, dispatch model.Dispatch, set int64) {
	id := model.NewRelationID(f.p.req.Binding.RepositoryID, from, kind, to)
	f.rels = append(f.rels, model.RelationFact{Relation: model.Relation{ID: id, From: from, Kind: kind, To: to},
		Evidence: []model.Evidence{f.p.evidence("", id, f.fv.ID, f.fv.ContentHash, rng, key, detail, dispatch, set)}})
}

// occurrenceOf is e as declared in the member fv.
func occurrenceOf(fv model.FileVersion, t *Table, e Endpoint) occurrence {
	return occurrence{Endpoint: e, File: fv.ID, Hash: fv.ContentHash, Language: t.Language}
}

// occurrenceFact is the linker's occurrence of a structural node: the node
// the structural provider resolved, with its own evidence at the
// declaration's range.
func (p *publisher) occurrenceFact(o occurrence) model.NodeFact {
	rng := located(o.Range)
	key := o.Qualified
	if len(key) > model.MaxNativeKeyBytes {
		key = ""
	}
	n := model.Node{ID: o.Node, Kind: o.Kind, Language: o.Language, Name: o.Name, QualifiedName: o.Qualified,
		FileID: o.File, ContentHash: o.Hash, Range: rng}
	return model.NodeFact{Node: n, CanonicalKey: o.CanonicalKey,
		Evidence: []model.Evidence{p.evidence(o.Node, "", o.File, o.Hash, rng, key, "", "", 0)}}
}

// evidence is one static-analysis evidence row of this unit and run; a
// relation occurrence's carries its dispatch and set size.
func (p *publisher) evidence(node model.NodeID, rel model.RelationID, file model.FileID, hash string, rng *model.SourceRange,
	key, detail string, dispatch model.Dispatch, set int64) model.Evidence {
	e := model.Evidence{UnitID: p.req.Unit.ID, ProviderID: p.req.Unit.ProviderID, ProviderVersion: p.req.Unit.ProviderVersion,
		OriginRunID: p.req.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionStaticAnalysis,
		FileID: file, ContentHash: hash, Range: rng, NativeKey: key, Detail: detail, Dispatch: dispatch, SetSize: set}
	e.ID = model.NewEvidenceID(e)
	return e
}

// put hands nodes, then relations, to the sink.
func (p *publisher) put(nodes []model.NodeFact, rels []model.RelationFact) error {
	if len(nodes) > 0 {
		if err := p.sink.PutNodes(p.ctx, nodes); err != nil {
			return err
		}
	}
	if len(rels) > 0 {
		if err := p.sink.PutRelations(p.ctx, rels); err != nil {
			return err
		}
	}
	p.records += uint64(len(nodes) + len(rels))
	return nil
}

// callsiteKey is the structural provider's call-site key of the callee token
// [start,end) (rawCallsiteKey), or empty when it does not fit a native key.
func callsiteKey(path string, start, end uint32) string {
	key := rawCallsiteKey(path, start, end)
	if len(key) > model.MaxNativeKeyBytes {
		return ""
	}
	return key
}

// rawCallsiteKey is "callsite:" + path + ":" + the first and last byte of the
// callee token [start,end), one-based and inclusive.
func rawCallsiteKey(path string, start, end uint32) string {
	return "callsite:" + path + ":" + strconv.FormatUint(uint64(start)+1, 10) + "-" + strconv.FormatUint(uint64(end), 10)
}

// byteSpan is r's first and last byte, one-based and inclusive, as a
// call-site key spells a token's.
func byteSpan(r model.SourceRange) string {
	return strconv.FormatUint(r.Start.Byte+1, 10) + "-" + strconv.FormatUint(r.End.Byte, 10)
}

// nativeKeyDomain is the hash domain of a native key too long to be one.
const nativeKeyDomain = "link.native_key"

// fitKey is key when it fits a native key, else "digest:" and its digest: a
// key the linker mints for a node of its own (an open site, an open import
// site, an external node) names that node however long the path or name it
// spells.
func fitKey(key string) string {
	if len(key) <= model.MaxNativeKeyBytes {
		return key
	}
	return "digest:" + model.H(nativeKeyDomain, key)
}

// nodeName is name as a node's name: cut to the name bound at a character
// boundary, or fallback when it is empty.
func nodeName(name, fallback string) string {
	if name == "" {
		return fallback
	}
	if len(name) <= model.MaxNameBytes {
		return name
	}
	cut := model.MaxNameBytes
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut]
}

// located is r as a fact's range, or none when r is not a valid range.
func located(r model.SourceRange) *model.SourceRange {
	if r.Validate("range") != nil {
		return nil
	}
	return &r
}

// errStopped ends a sorted run's walk when its puller is stopped early.
var errStopped = errors.New("stopped")

// pull turns a sorted run's walk into a pull, so it can be merged with the
// walk of the tables.
func pull[T any](run *pagination.SortedRun[T]) (func() (T, bool, error), func()) {
	var walkErr error
	next, stop := iter.Pull(func(yield func(T) bool) {
		walkErr = run.Each(func(v T) error {
			if !yield(v) {
				return errStopped
			}
			return nil
		})
	})
	return func() (T, bool, error) {
		v, ok := next()
		if !ok && walkErr != nil && !errors.Is(walkErr, errStopped) {
			return v, false, walkErr
		}
		return v, ok, nil
	}, stop
}

// notMember is a binding naming a site or target in a file that is not a
// member of the unit, which no resolution of it can have found.
func notMember(what string, file model.FileID) error {
	return internalErr("a binding's " + what + " is in file " + string(file) + ", which is not a member of the unit")
}

// untabledRef is a binding naming a member with no table, which no resolver
// can have read and no state of an equal digest can hold.
func untabledRef(what string, r Ref) error {
	return internalErr("a binding's " + what + " names member " + strconv.Itoa(int(r.File)) + ", which has no link table")
}

// outOfTable is a binding naming an entry past its member's table.
func outOfTable(what string, r Ref, path string) error {
	return internalErr("a binding's " + what + " names entry " + strconv.Itoa(int(r.Index)) + " past the link table of " + path)
}
