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

// Publication. Every binding names its site and its target by identity
// (idBinding), and the members are walked in FileID order, so publication is
// two ordered merges of the bindings with the members, each reading only the
// tables of the members it republishes, one at a time:
//
//  1. the declarations the bindings target, by target, with the members: a
//     republished member's targeted declarations are found in its table by
//     node and their occurrences published;
//  2. the bindings, by site, with the members: a republished member's sites
//     publish their `calls` occurrences, or their ambiguous callee and
//     candidates, and every member's record is taken, from its fresh table
//     when it is republished and from the predecessor's state otherwise.
//
// Every target's node is handed over in the first merge, before any relation
// of the second, so no relation reaches the sink ahead of an identity it
// names. A site's relation needs only its target's node, which the binding
// carries, so a site is published without reading its target's table.
//
// The republished members are every member on a full build, and otherwise
// the ones a refresh found changed (refresh): content-changed, added, those
// whose bindings changed and those whose set of targeted declarations
// changed. Their predecessor's facts are replaced; every other member's are
// carried over by the delta build.

// occurrence is a structural node a published edge names, with the file it
// is declared in.
type occurrence struct {
	Endpoint
	File     model.FileID
	Hash     string
	Language string
}

// publisher publishes one unit's bindings.
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

// publish runs the two merges: occurrences of targets, then sites, and
// returns every member's record, by FileID.
func (p *publisher) publish(bindings *pagination.SortedRun[idBinding], targets *pagination.SortedRun[target]) (*pagination.SortedRun[record], error) {
	if err := p.publishTargets(targets); err != nil {
		return nil, err
	}
	return p.publishSites(bindings)
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
		var byNode map[model.NodeID]int32
		if publish {
			var has bool
			if t, has, err = p.u.tables.Table(p.ctx, p.u.gen, fv); err != nil {
				return err
			}
			if !has {
				return internalErr("a binding targets a declaration of " + fv.Path + ", which has no link table")
			}
			byNode = make(map[model.NodeID]int32, len(t.Decls))
			for i := len(t.Decls) - 1; i >= 0; i-- {
				byNode[t.Decls[i].Node] = int32(i)
			}
		}
		var nodes []model.NodeFact
		for ok && cur.File == fv.ID {
			if publish {
				i, found := byNode[cur.Node]
				if !found {
					return internalErr("a binding targets node " + string(cur.Node) + ", which the link table of " + fv.Path + " does not declare")
				}
				nodes = append(nodes, p.occurrenceFact(occurrenceOf(fv, t, t.Decls[i].Endpoint)))
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
			return notMember("site", cur.SiteFile)
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
		if has {
			if rec.Interface, err = t.InterfaceDigest(); err != nil {
				return err
			}
			if rec.Sites, err = t.SitesDigest(); err != nil {
				return err
			}
			rec.Digest, rec.Tabled, rec.Dropped = memberDigest(rec.Interface, rec.Sites, t.Dropped), true, int64(t.Dropped)
		}
		f := filePublication{p: p, fv: fv, t: t, seen: map[model.NodeID]bool{}}
		for ok && cur.SiteFile == fv.ID {
			if !has {
				return internalErr("a binding names a site of " + fv.Path + ", which has no link table")
			}
			site := cur.Site
			if site >= int32(len(t.Sites)) {
				return internalErr("a binding names site " + strconv.Itoa(int(site)) + " past the link table of " + fv.Path)
			}
			group = group[:0]
			for ok && cur.SiteFile == fv.ID && cur.Site == site {
				group = append(group, cur)
				if cur, ok, err = next(); err != nil {
					return err
				}
			}
			if err := f.site(&t.Sites[site], group); err != nil {
				return err
			}
		}
		rec.Unkeyed = f.unkeyed
		if err := p.put(f.nodes, f.rels); err != nil {
			return err
		}
		return p.keep(records, rec)
	})
	if err != nil {
		return nil, err
	}
	if ok {
		return nil, notMember("site", cur.SiteFile)
	}
	return records.Sorted()
}

// keep adds rec to records and to the unit's coverage.
func (p *publisher) keep(records *pagination.ExternalSort[record], rec record) error {
	if !rec.Tabled {
		p.cov.untabled++
	}
	p.cov.dropped += rec.Dropped
	p.cov.unkeyed += rec.Unkeyed
	return records.Add(rec)
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
	// unkeyed counts the file's ambiguous sites whose call-site key does not
	// fit a native key, which have no identity to mint and are not
	// published.
	unkeyed int64
}

// site publishes one site bound to the declarations of group: one `calls`
// occurrence to a single target, or, for two or more, the ambiguous family
// the structural provider publishes its own ambiguous calls into, with every
// candidate.
func (f *filePublication) site(s *Site, group []idBinding) error {
	caller := f.t.Module
	if s.Caller >= 0 {
		if s.Caller >= int32(len(f.t.Decls)) {
			return internalErr("a link table site of " + f.fv.Path + " names a caller that is not a declaration of its file")
		}
		caller = f.t.Decls[s.Caller].Endpoint
	}
	// Two declarations that are occurrences of one node are one target, so
	// a repeated identity is neither a second candidate nor an ambiguity.
	targets := make([]idBinding, 0, len(group))
	distinct := make(map[model.NodeID]bool, len(group))
	for _, b := range group {
		if !distinct[b.Target] {
			distinct[b.Target] = true
			targets = append(targets, b)
		}
	}
	rng := located(s.Range)
	key := callsiteKey(f.fv.Path, s.NameStart, s.NameEnd)
	if len(targets) == 1 {
		f.node(occurrenceOf(f.fv, f.t, caller))
		f.relation(caller.Node, model.RelCalls, targets[0].Target, rng, key, targets[0].Detail)
		return nil
	}
	native := "link:" + key
	if key == "" || len(native) > model.MaxNativeKeyBytes {
		f.unkeyed++
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
	for _, b := range targets {
		f.relation(caller.Node, model.RelMayReferTo, b.Target, rng, key, DetailAmbiguousTarget)
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
