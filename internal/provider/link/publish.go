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
// index, and the target's table is another file's, so publication is two
// ordered merges over the members' tables and never holds more than one
// table:
//
//  1. the bindings, sorted by target, are merged with the tables in member
//     order; each target's endpoint is read from its table, its occurrence
//     node is published, and the binding goes on, carrying the endpoint,
//     into a sort by site;
//  2. those, sorted by site, are merged with the tables again; each site's
//     caller is read from its table, and the site publishes its `calls`
//     occurrence, or its ambiguous callee and candidates.
//
// Every target's node is handed over in the first merge, before any
// relation, and every caller's and callee's in the second, before the
// relations of its site, so no relation reaches the sink ahead of an
// identity it names.

// occurrence is a structural node a published edge names, with the file it
// is declared in.
type occurrence struct {
	Endpoint
	File     model.FileID `json:"file"`
	Hash     string       `json:"hash"`
	Language string       `json:"language"`
}

// resolved is a binding with its target's endpoint.
type resolved struct {
	Binding
	To occurrence `json:"to"`
}

// publisher publishes one unit's bindings.
type publisher struct {
	ctx  context.Context
	req  provider.UnitRequest
	sink provider.Sink
	u    *unit
	// members is the unit's member count, from which the members with no
	// table are counted.
	members int64
	cov     coverage
	// unkeyed counts the ambiguous sites whose call-site key does not fit a
	// native key, which have no identity to mint and are not published.
	unkeyed int64
	records uint64
}

// publish runs both merges over run, the unit's folded bindings in site
// order.
func (p *publisher) publish(run *pagination.SortedRun[Binding]) error {
	byTarget, err := newBindingSort(p.u.dir, compareTargetSite)
	if err != nil {
		return err
	}
	defer byTarget.Close()
	if err := run.Each(byTarget.Add); err != nil {
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

// joinTargets is the first merge. It also takes the unit's coverage, since it
// is the one walk that reaches every table.
func (p *publisher) joinTargets(targets *pagination.SortedRun[Binding], out *pagination.ExternalSort[resolved]) error {
	next, stop := pull(targets)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	var tabled int64
	err = p.u.Files(p.ctx, func(file int32, fv model.FileVersion, t *Table) error {
		tabled++
		p.cov.dropped += int64(t.Dropped)
		if ok && cur.Target.File < file {
			return untabledRef("target", cur.Target)
		}
		var nodes []model.NodeFact
		last := int32(-1)
		for ok && cur.Target.File == file {
			if cur.Target.Index >= int32(len(t.Decls)) {
				return outOfTable("target", cur.Target, fv.Path)
			}
			o := occurrenceOf(fv, t, t.Decls[cur.Target.Index].Endpoint)
			if cur.Target.Index != last {
				nodes = append(nodes, p.occurrenceFact(o))
				last = cur.Target.Index
			}
			if err := out.Add(resolved{Binding: cur, To: o}); err != nil {
				return err
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
	p.cov.untabled = p.members - tabled
	return nil
}

// publishSites is the second merge.
func (p *publisher) publishSites(sites *pagination.SortedRun[resolved]) error {
	next, stop := pull(sites)
	defer stop()
	cur, ok, err := next()
	if err != nil {
		return err
	}
	var group []resolved
	err = p.u.Files(p.ctx, func(file int32, fv model.FileVersion, t *Table) error {
		if ok && cur.Site.File < file {
			return untabledRef("site", cur.Site)
		}
		f := filePublication{p: p, fv: fv, t: t, seen: map[model.NodeID]bool{}}
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

// filePublication is what one file's sites publish, handed to the sink once
// the file's last site is done.
type filePublication struct {
	p     *publisher
	fv    model.FileVersion
	t     *Table
	seen  map[model.NodeID]bool
	nodes []model.NodeFact
	rels  []model.RelationFact
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
		f.node(occurrenceOf(f.fv, f.t, caller))
		f.relation(caller.Node, model.RelCalls, targets[0].To.Node, rng, key, targets[0].Detail)
		return nil
	}
	native := "link:" + key
	if key == "" || len(native) > model.MaxNativeKeyBytes {
		f.p.unkeyed++
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
	callee := res.Node
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
