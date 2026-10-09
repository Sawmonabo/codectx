package link

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Publication. A binding names its target by member ordinal and table
// index, and the target's table is another file's, so publication is three
// ordered merges over the members and never holds more than one table:
//
//  1. the bindings, in site order, are merged with the members to mark what
//     each site's file needs (siteUse), and go into a sort by target;
//  2. those, sorted by target, are merged with the tables in member order;
//     each target's endpoint is read from its table, its occurrence node is
//     published, and every binding whose site is used goes on, carrying the
//     endpoint, into a sort by site;
//  3. those, sorted by site, are merged with the tables again; each site's
//     caller is read from its table, and the site publishes its `calls`
//     occurrence, or its ambiguous callee and candidates.
//
// Every target's node is handed over in the second merge, before any
// relation, and every caller's and callee's in the third, before the
// relations of its site, so no relation reaches the sink ahead of an
// identity it names.
//
// A unit that inherits its predecessor's resolution (publisher.edited set)
// republishes only the facts whose evidence lies in an edited member: the
// occurrences of the edited members' declarations and the facts of their
// sites. Its predecessor's other facts are carried over by the delta build,
// and a table is read only when what is republished needs it: an edited
// member's, and an untouched member's that a republished site targets.

// occurrence is a structural node a published edge names, with the file it
// is declared in.
type occurrence struct {
	Endpoint
	File     model.FileID `json:"file"`
	Hash     string       `json:"hash"`
	Language string       `json:"language"`
}

// resolved is a binding with its target's endpoint, and whether its site is
// published or only counted.
type resolved struct {
	Binding
	To      occurrence `json:"to"`
	Publish bool       `json:"publish,omitempty"`
}

// siteUse is what publication does with a binding's site.
type siteUse byte

const (
	// siteCarried: the site's file is untouched, so its facts are carried.
	siteCarried siteUse = iota
	// siteCounted: the site's file is untouched, but its path is long
	// enough that an ambiguous site of it may have no key, so its sites
	// are walked to count those and publish nothing.
	siteCounted
	// sitePublished: the site's facts are published.
	sitePublished
)

// marked is a binding with its site's use.
type marked struct {
	Binding
	Use siteUse
}

func encodeMarked(m marked) ([]byte, error) {
	b, err := encodeBinding(m.Binding)
	return append(b, byte(m.Use)), err
}

func decodeMarked(raw []byte) (marked, error) {
	if len(raw) == 0 || raw[len(raw)-1] > byte(sitePublished) {
		return marked{}, errors.New("a marked binding is malformed")
	}
	b, err := decodeBinding(raw[:len(raw)-1])
	return marked{Binding: b, Use: siteUse(raw[len(raw)-1])}, err
}

// publisher publishes one unit's bindings.
type publisher struct {
	ctx  context.Context
	req  provider.UnitRequest
	sink provider.Sink
	u    *unit
	// edited, when set, streams the FileIDs of the members whose content
	// changed since the predecessor whose resolution the unit inherits, in
	// ascending order (it may name files that are not members): only their
	// facts are published, and cov is the predecessor's. Nil publishes every
	// fact and takes cov from the tables.
	edited func(yield func(model.FileID) error) error
	// members is the unit's member count, from which the members with no
	// table are counted.
	members int64
	cov     coverage
	// unkeyed counts the ambiguous sites whose call-site key does not fit a
	// native key, which have no identity to mint and are not published.
	unkeyed int64
	records uint64
}

// publish runs the three merges over run, the unit's folded bindings in site
// order.
func (p *publisher) publish(run *pagination.SortedRun[Binding]) error {
	byTarget, err := pagination.NewExternalSort(p.u.dir, 0, encodeMarked, decodeMarked,
		func(a, b marked) int { return compareTargetSite(a.Binding, b.Binding) })
	if err != nil {
		return err
	}
	defer byTarget.Close()
	if err := p.markSites(run, byTarget); err != nil {
		return err
	}
	targets, err := byTarget.Sorted()
	if err != nil {
		return err
	}
	defer targets.Close()
	bySite, err := pagination.NewExternalSort(p.u.dir, 0,
		func(r resolved) ([]byte, error) { return json.Marshal(r) },
		func(b []byte) (resolved, error) {
			var r resolved
			err := json.Unmarshal(b, &r)
			return r, err
		},
		func(a, b resolved) int { return compareSiteTarget(a.Binding, b.Binding) })
	if err != nil {
		return err
	}
	defer bySite.Close()
	if err := p.joinTargets(targets, bySite); err != nil {
		return err
	}
	sites, err := bySite.Sorted()
	if err != nil {
		return err
	}
	defer sites.Close()
	return p.publishSites(sites)
}

// markSites is the first merge. It reads no table.
func (p *publisher) markSites(run *pagination.SortedRun[Binding], out *pagination.ExternalSort[marked]) error {
	next, stop := pull(run)
	defer stop()
	edited, stopEdited := p.editedCursor()
	defer stopEdited()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	var file int32 = -1
	err = p.u.members(func(fv model.FileVersion) error {
		file++
		if !ok || cur.Site.File != file {
			return nil
		}
		use := siteCarried
		e, err := edited(fv.ID)
		switch {
		case err != nil:
			return err
		case e:
			use = sitePublished
		case mayBeUnkeyed(fv.Path):
			use = siteCounted
		}
		for ok && cur.Site.File == file {
			if err := out.Add(marked{Binding: cur, Use: use}); err != nil {
				return err
			}
			if cur, ok, err = next(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if ok {
		return untabledRef("site", cur.Site)
	}
	return nil
}

// joinTargets is the second merge. Publishing every fact, it also takes the
// unit's coverage, since it is then the one walk that reads every table.
func (p *publisher) joinTargets(targets *pagination.SortedRun[marked], out *pagination.ExternalSort[resolved]) error {
	next, stop := pull(targets)
	defer stop()
	edited, stopEdited := p.editedCursor()
	defer stopEdited()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	var tabled int64
	var file int32 = -1
	err = p.u.members(func(fv model.FileVersion) error {
		file++
		var t *Table
		load := func() error {
			if t != nil {
				return nil
			}
			var has bool
			var err error
			if t, has, err = p.u.tables.Table(p.ctx, p.u.gen, fv); err != nil || !has {
				return err
			}
			tabled++
			p.cov.dropped += int64(t.Dropped)
			return nil
		}
		if p.edited == nil {
			if err := load(); err != nil {
				return err
			}
		}
		if !ok || cur.Target.File != file {
			return nil
		}
		e, err := edited(fv.ID)
		if err != nil {
			return err
		}
		var nodes []model.NodeFact
		last := int32(-1)
		for ok && cur.Target.File == file {
			if e || cur.Use != siteCarried {
				if err := load(); err != nil {
					return err
				}
				if t == nil {
					return untabledRef("target", cur.Target)
				}
				if cur.Target.Index >= int32(len(t.Decls)) {
					return outOfTable("target", cur.Target, fv.Path)
				}
				o := occurrenceOf(fv, t, t.Decls[cur.Target.Index].Endpoint)
				if e && cur.Target.Index != last {
					nodes = append(nodes, p.occurrenceFact(o))
					last = cur.Target.Index
				}
				if cur.Use != siteCarried {
					if err := out.Add(resolved{Binding: cur.Binding, To: o, Publish: cur.Use == sitePublished}); err != nil {
						return err
					}
				}
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
		return untabledRef("target", cur.Target)
	}
	if p.edited == nil {
		p.cov.untabled = p.members - tabled
	}
	return nil
}

// publishSites is the third merge. It reads the table of a member only when
// one of its sites is used.
func (p *publisher) publishSites(sites *pagination.SortedRun[resolved]) error {
	next, stop := pull(sites)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	var group []resolved
	var file int32 = -1
	err = p.u.members(func(fv model.FileVersion) error {
		file++
		if !ok || cur.Site.File != file {
			return nil
		}
		t, has, err := p.u.tables.Table(p.ctx, p.u.gen, fv)
		if err != nil {
			return err
		}
		if !has {
			return untabledRef("site", cur.Site)
		}
		f := filePublication{p: p, fv: fv, t: t, publish: cur.Publish, seen: map[model.NodeID]bool{}}
		for ok && cur.Site.File == file {
			site := cur.Site
			if site.Index >= int32(len(t.Sites)) {
				return outOfTable("site", site, fv.Path)
			}
			group = group[:0]
			for ok && cur.Site == site {
				group = append(group, cur)
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			if err := f.site(&t.Sites[site.Index], group); err != nil {
				return err
			}
		}
		return p.put(f.nodes, f.rels)
	})
	if err != nil {
		return err
	}
	if ok {
		return untabledRef("site", cur.Site)
	}
	return nil
}

// editedCursor answers, for members asked in ascending FileID order, whether
// each was edited, by a merge with p.edited; every member is edited when
// p.edited is nil.
func (p *publisher) editedCursor() (func(model.FileID) (bool, error), func()) {
	if p.edited == nil {
		return func(model.FileID) (bool, error) { return true, nil }, func() {}
	}
	var walkErr error
	next, stop := iter.Pull(func(yield func(model.FileID) bool) {
		walkErr = p.edited(func(id model.FileID) error {
			if !yield(id) {
				return errStopped
			}
			return nil
		})
	})
	cur, ok := next()
	return func(id model.FileID) (bool, error) {
		for ok && cur < id {
			cur, ok = next()
		}
		if !ok && walkErr != nil && !errors.Is(walkErr, errStopped) {
			return false, walkErr
		}
		return ok && cur == id, nil
	}, stop
}

// mayBeUnkeyed reports whether an ambiguous site of path can have a `link:`
// key over model.MaxNativeKeyBytes: whether the key at the widest offsets a
// file can have does not fit.
func mayBeUnkeyed(path string) bool {
	widest := strconv.FormatUint(uint64(wire.MaxSourceOffset)+1, 10)
	return len("link:callsite:"+path+":"+widest+"-"+widest) > model.MaxNativeKeyBytes
}

// filePublication is what one file's sites publish, handed to the sink once
// the file's last site is done.
type filePublication struct {
	p  *publisher
	fv model.FileVersion
	t  *Table
	// publish is false for a file whose sites are only counted.
	publish bool
	seen    map[model.NodeID]bool
	nodes   []model.NodeFact
	rels    []model.RelationFact
}

// site publishes one site bound to the declarations of group: one `calls`
// occurrence to a single target, or, for two or more, the ambiguous family
// the structural provider publishes its own ambiguous calls into, with every
// candidate.
func (f *filePublication) site(s *Site, group []resolved) error {
	caller := f.t.Module
	if s.Caller >= 0 {
		if s.Caller >= int32(len(f.t.Decls)) {
			return internalErr("a link table site of " + f.fv.Path + " names a caller that is not a declaration of its file")
		}
		caller = f.t.Decls[s.Caller].Endpoint
	}
	// Two declarations that are occurrences of one node are one target, so
	// a repeated identity is neither a second candidate nor an ambiguity.
	targets := make([]resolved, 0, len(group))
	distinct := make(map[model.NodeID]bool, len(group))
	for _, r := range group {
		if !distinct[r.To.Node] {
			distinct[r.To.Node] = true
			targets = append(targets, r)
		}
	}
	rng := located(s.Range)
	key := callsiteKey(f.fv.Path, s.NameStart, s.NameEnd)
	if len(targets) == 1 {
		if f.publish {
			f.node(occurrenceOf(f.fv, f.t, caller))
			f.relation(caller.Node, model.RelCalls, targets[0].To.Node, rng, key, targets[0].Detail)
		}
		return nil
	}
	native := "link:" + key
	if key == "" || len(native) > model.MaxNativeKeyBytes {
		f.p.unkeyed++
		return nil
	}
	if !f.publish {
		return nil
	}
	f.node(occurrenceOf(f.fv, f.t, caller))
	kind := model.NodeFunction
	if s.Shape == wire.ShapeMember || s.Shape == wire.ShapeStatic {
		kind = model.NodeMethod
	}
	res, err := f.p.req.Resolver.Resolve(f.p.ctx, model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + f.fv.Path,
		NativeKey: native, Kind: kind, Language: f.t.Language, Name: s.Callee})
	if err != nil {
		return err
	}
	// The callee is located in the site's file, so its fact shares that
	// file's retention bucket: a delta that republishes the file replaces it,
	// and one that carries the file carries it.
	callee := res.Node
	callee.FileID, callee.ContentHash, callee.Range = f.fv.ID, f.fv.ContentHash, rng
	meta, err := json.Marshal(map[string]any{"resolution": "ambiguous", "candidates": len(targets), "callee": s.Callee})
	if err != nil {
		return internalErr("ambiguous callee metadata: " + err.Error())
	}
	callee.Metadata = meta
	if !f.seen[callee.ID] {
		f.seen[callee.ID] = true
		f.nodes = append(f.nodes, model.NodeFact{Node: callee, CanonicalKey: res.CanonicalKey,
			Evidence: []model.Evidence{f.p.evidence(callee.ID, "", f.fv.ID, f.fv.ContentHash, rng, native, "")}})
	}
	f.relation(caller.Node, model.RelCalls, callee.ID, rng, key, DetailAmbiguous)
	for _, r := range targets {
		f.relation(caller.Node, model.RelMayReferTo, r.To.Node, rng, key, DetailAmbiguousTarget)
	}
	return nil
}

// node publishes o's occurrence once per file.
func (f *filePublication) node(o occurrence) {
	if f.seen[o.Node] {
		return
	}
	f.seen[o.Node] = true
	f.nodes = append(f.nodes, f.p.occurrenceFact(o))
}

// relation publishes one occurrence of an edge at the site's range. Two
// sites of one caller and callee publish one canonical edge, each with its
// own evidence row.
func (f *filePublication) relation(from model.NodeID, kind model.RelationKind, to model.NodeID, rng *model.SourceRange, key, detail string) {
	id := model.NewRelationID(f.p.req.Binding.RepositoryID, from, kind, to)
	f.rels = append(f.rels, model.RelationFact{Relation: model.Relation{ID: id, From: from, Kind: kind, To: to},
		Evidence: []model.Evidence{f.p.evidence("", id, f.fv.ID, f.fv.ContentHash, rng, key, detail)}})
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
		Evidence: []model.Evidence{p.evidence(o.Node, "", o.File, o.Hash, rng, key, "")}}
}

// evidence is one static-analysis evidence row of this unit and run.
func (p *publisher) evidence(node model.NodeID, rel model.RelationID, file model.FileID, hash string, rng *model.SourceRange, key, detail string) model.Evidence {
	e := model.Evidence{UnitID: p.req.Unit.ID, ProviderID: p.req.Unit.ProviderID, ProviderVersion: p.req.Unit.ProviderVersion,
		OriginRunID: p.req.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionStaticAnalysis,
		FileID: file, ContentHash: hash, Range: rng, NativeKey: key, Detail: detail}
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
// [start,end): "callsite:" + path + ":" + the first and last byte, one-based
// and inclusive, or empty when it does not fit a native key.
func callsiteKey(path string, start, end uint32) string {
	key := "callsite:" + path + ":" + strconv.FormatUint(uint64(start)+1, 10) + "-" + strconv.FormatUint(uint64(end), 10)
	if len(key) > model.MaxNativeKeyBytes {
		return ""
	}
	return key
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

// untabledRef is a binding naming a member with no table, which no resolver
// can have read and no state of an equal digest can hold.
func untabledRef(what string, r Ref) error {
	return internalErr("a binding's " + what + " names member " + strconv.Itoa(int(r.File)) + ", which has no link table")
}

// outOfTable is a binding naming an entry past its member's table.
func outOfTable(what string, r Ref, path string) error {
	return internalErr("a binding's " + what + " names entry " + strconv.Itoa(int(r.Index)) + " past the link table of " + path)
}
