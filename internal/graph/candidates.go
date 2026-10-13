package graph

import (
	"cmp"
	"context"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// DeclarationReader is the OPTIONAL seam a GraphReader implements so name-only
// candidates can be computed per query (ADR-0015 decision 3): the nodes of the
// pinned generation that carry one exact name, the same nodes hydrated with
// their file's path and the unit that published them, and the snapshot's
// files. A reader without it cannot list candidates, and the answer says they
// are unavailable rather than none.
type DeclarationReader interface {
	// Declarations pages the generation's nodes whose name is exactly name,
	// restricted to kinds (every kind when kinds is empty), by node id
	// ascending and starting after after. next is the id the following page
	// starts after, "" once the list is exhausted; a page may be short of
	// limit with more behind it, so only next ends the list.
	Declarations(ctx context.Context, name string, kinds []model.NodeKind, after model.NodeID,
		limit int) (page []Declaration, next model.NodeID, err error)
	// Declared hydrates nodes as Declarations; a node the generation does
	// not publish is omitted.
	Declared(ctx context.Context, ids []model.NodeID) ([]Declaration, error)
	// SnapshotFiles pages the pinned snapshot's files by file id ascending,
	// starting after after; next is the last id of a full page, "" at the end.
	SnapshotFiles(ctx context.Context, after model.FileID, limit int) (page []model.FileVersion, next model.FileID, err error)
	// FileNodes maps each file to its file node, by the file's path; a file
	// with no file node is omitted.
	FileNodes(ctx context.Context, files []model.FileVersion) (map[model.FileID]model.NodeID, error)
}

// Declaration is one node with where it is: its file's path in the pinned
// snapshot and the unit that published it.
type Declaration struct {
	Node model.Node
	Path string
	Unit model.UnitID
}

// declarationReader is the seam of r, nil when r lacks it.
func declarationReader(r GraphReader) DeclarationReader {
	d, _ := r.(DeclarationReader)
	return d
}

// candidatesUnavailable is the answer of a reader that cannot list
// candidates: unavailable, never an empty list.
func candidatesUnavailable() error {
	return &model.Error{Code: model.CodeProviderUnavailable,
		Message: "this index cannot list name-only call candidates: its reader does not look declarations up by name"}
}

// Candidates returns one ranked page of the name-only candidates of an open
// call site, or of a direct lookup by name, shape, argument count and
// language, read from the generation r is pinned to (ADR-0015): the
// declarations of the same name, as the language compares names, that fit the
// site's shape and language family (and, in the statically typed families,
// its argument count), ordered by closeness and then by node id. Total is
// exact and nothing is cut at a count. A site whose callee does not fold to a
// name (reason `computed`) has none.
//
// Every candidate of one lookup shares the lookup's name, so "names with fewer
// compatible definitions first" orders nothing inside one answer; closeness
// and the node id are the whole order.
//
// The cursor is a position in that order -- the generation, the query's hash
// (the site, or the name, shape, argument count and language of a direct
// lookup), the closeness step and the node id of the last candidate served.
// It is refused on any generation but the one it was minted on and on any
// other query, so one site's position never resumes another site's list. It
// is not signed: it names nothing a caller could not ask for directly, and it
// never leaves the process (the context compiler pages it to exhaustion).
func Candidates(ctx context.Context, r GraphReader, req model.CandidateRequest) (model.CandidatePage, error) {
	if err := req.Validate(); err != nil {
		return model.CandidatePage{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.CandidatePage{}, err
	}
	gen := r.Binding().GenerationID
	if req.GenerationID != 0 && req.GenerationID != gen {
		return model.CandidatePage{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "candidates names generation " + strconv.FormatInt(int64(req.GenerationID), 10) +
				", and this reader is pinned to " + strconv.FormatInt(int64(gen), 10)}
	}
	dr := declarationReader(r)
	if dr == nil {
		return model.CandidatePage{}, candidatesUnavailable()
	}
	var site callSite
	if req.Site != "" {
		decls, err := dr.Declared(ctx, []model.NodeID{req.Site})
		if err != nil {
			return model.CandidatePage{}, err
		}
		if len(decls) != 1 {
			return model.CandidatePage{}, &model.Error{Code: model.CodeArgumentInvalid,
				Message: "candidates site " + string(req.Site) + " is not in this generation"}
		}
		s, ok := openCallSite(decls[0])
		if !ok {
			return model.CandidatePage{}, &model.Error{Code: model.CodeArgumentInvalid,
				Message: "candidates site " + string(req.Site) + " is not an open call site"}
		}
		if site, err = withImports(ctx, r, dr, s); err != nil {
			return model.CandidatePage{}, err
		}
	} else {
		site = callSite{Name: req.Name, Shape: req.Shape, Arguments: req.Arguments, Language: req.Language}
	}
	ranked, err := rankCandidates(ctx, dr, site)
	if err != nil {
		return model.CandidatePage{}, err
	}
	query := candidateQueryHash(req)
	after, err := parseCandidateCursor(req.Page.Cursor, gen, query)
	if err != nil {
		return model.CandidatePage{}, err
	}
	start := 0
	if after != nil {
		start, _ = slices.BinarySearchFunc(ranked, *after, compareRanked)
		if start < len(ranked) && compareRanked(ranked[start], *after) == 0 {
			start++
		}
	}
	limit := pagination.PageLimit(ctx, req.Page.Limit)
	end := min(start+limit, len(ranked))
	page := model.CandidatePage{Total: int64(len(ranked)), Candidates: make([]model.Candidate, 0, end-start)}
	resolution := model.ResolutionNameOnly
	if len(ranked) == 1 {
		resolution = model.ResolutionUniqueName
	}
	for _, c := range ranked[start:end] {
		page.Candidates = append(page.Candidates, model.Candidate{NodeID: c.ID, Resolution: resolution,
			Closeness: closenessSteps[c.Step]})
	}
	if end < len(ranked) {
		page.Cursor = candidateCursor(gen, query, ranked[end-1])
	}
	if err := page.Validate(); err != nil {
		return model.CandidatePage{}, err
	}
	return page, nil
}

// closenessSteps are the closeness steps in ranking order.
var closenessSteps = []model.CandidateCloseness{
	model.ClosenessSameFile, model.ClosenessSameDirectory, model.ClosenessImported,
	model.ClosenessSameUnit, model.ClosenessElsewhere,
}

// rankedCandidate is one compatible declaration and its closeness step.
type rankedCandidate struct {
	Step int
	ID   model.NodeID
}

func compareRanked(a, b rankedCandidate) int {
	if c := cmp.Compare(a.Step, b.Step); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// rankCandidates reads every declaration of the site's name, keeps those that
// fit it, and orders them. Every one is read: the total is exact and the
// order is global, so no page can be ranked from a prefix.
func rankCandidates(ctx context.Context, dr DeclarationReader, site callSite) ([]rankedCandidate, error) {
	if site.Reason == model.ReasonComputed || site.Name == "" {
		return nil, nil
	}
	kinds := shapeKinds(site.Shape)
	var out []rankedCandidate
	var after model.NodeID
	for {
		page, next, err := dr.Declarations(ctx, site.Name, kinds, after, model.MaxPageItems)
		if err != nil {
			return nil, err
		}
		for _, d := range page {
			if site.fits(d.Node) {
				out = append(out, rankedCandidate{Step: site.closeness(d), ID: d.Node.ID})
			}
		}
		if next == "" {
			break
		}
		after = next
	}
	slices.SortFunc(out, compareRanked)
	return out, nil
}

// candidateQueryHashDomain is the hash domain of the query a candidate
// cursor is bound to.
const candidateQueryHashDomain = "graph.candidates.query"

// candidateQueryHash is the identity of one candidate query: the site, or
// the direct lookup's name, shape, argument count and language.
func candidateQueryHash(req model.CandidateRequest) string {
	h := model.NewHasher(candidateQueryHashDomain)
	h.AddString(string(req.Site))
	h.AddString(req.Name)
	h.AddString(string(req.Shape))
	h.AddString(strconv.FormatInt(req.Arguments, 10))
	h.AddString(req.Language)
	return h.Sum()
}

// candidateCursor renders a position in a candidate ranking.
func candidateCursor(gen model.GenerationID, query string, c rankedCandidate) string {
	return strconv.FormatInt(int64(gen), 10) + "." + query + "." + strconv.Itoa(c.Step) + "." + string(c.ID)
}

// parseCandidateCursor reads a position back, refusing one minted on another
// generation, for another query, or not minted here at all.
func parseCandidateCursor(token string, gen model.GenerationID, query string) (*rankedCandidate, error) {
	if token == "" {
		return nil, nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 4 {
		return nil, cursorInvalid("candidate cursor is not a position in a candidate ranking")
	}
	g, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || model.GenerationID(g) != gen {
		return nil, cursorInvalid("candidate cursor was minted on another generation")
	}
	if parts[1] != query {
		return nil, cursorInvalid("candidate cursor was minted for another site or lookup")
	}
	step, err := strconv.Atoi(parts[2])
	if err != nil || step < 0 || step >= len(closenessSteps) || !model.ValidHexID(parts[3]) {
		return nil, cursorInvalid("candidate cursor is not a position in a candidate ranking")
	}
	return &rankedCandidate{Step: step, ID: model.NodeID(parts[3])}, nil
}

// callSite is what a candidate is matched against: an open call site's
// callee name, shape, argument count and language, and, for a site with a
// location, where it is.
type callSite struct {
	// ID is the open-site node, empty for a direct lookup.
	ID        model.NodeID
	Name      string
	Shape     model.CallShape
	Arguments int64
	Language  string
	Reason    model.OpenSiteReason
	// FileID, Dir and Unit place the site; Linked are the files its file
	// imports or is imported by.
	FileID model.FileID
	Dir    string
	Unit   model.UnitID
	Linked map[model.FileID]bool
}

// openCallSite reads an open call site's description off its node, false for
// any other node -- the structural provider's `call:<name>` placeholder
// included, which carries a `callee` and no shape.
func openCallSite(d Declaration) (callSite, bool) {
	meta, ok := openSiteMeta(d.Node)
	if !ok {
		return callSite{}, false
	}
	s := callSite{ID: d.Node.ID, Name: d.Node.Name, Shape: meta.Shape, Arguments: meta.Arguments, Language: d.Node.Language,
		Reason: meta.Reason, FileID: d.Node.FileID, Unit: d.Unit}
	if d.Path != "" {
		s.Dir = path.Dir(d.Path)
	}
	return s, true
}

// withImports fills the files the site's file is linked to by `imports`, in
// either direction, read off every container above the site -- the file's
// module among them, which is where a file's imports are written.
func withImports(ctx context.Context, r GraphReader, dr DeclarationReader, s callSite) (callSite, error) {
	s.Linked = map[model.FileID]bool{}
	if s.FileID == "" || s.ID == "" {
		return s, nil
	}
	code, ok := r.Kinds().Code(model.RelImports)
	if !ok {
		return s, nil
	}
	refs, err := r.Resolve(ctx, []model.NodeID{s.ID})
	if err != nil {
		return s, err
	}
	if len(refs) != 1 || refs[0] == 0 {
		return s, nil
	}
	// The chain from the site up to the file's top node: one container read
	// per level of nesting.
	var chain []NodeRef
	seen := map[NodeRef]bool{refs[0]: true}
	for at := refs[0]; ; {
		up, err := r.Containers(ctx, []NodeRef{at})
		if err != nil {
			return s, err
		}
		if len(up) != 1 || up[0] == 0 || seen[up[0]] {
			break
		}
		seen[up[0]] = true
		chain = append(chain, up[0])
		at = up[0]
	}
	if len(chain) == 0 {
		return s, nil
	}
	var ends []NodeRef
	if _, err := r.Neighbours(ctx, ascending(chain), model.DirectionBoth, []KindCode{code}, EdgePos{},
		func(e Edge) error { ends = append(ends, e.Neighbour); return nil }); err != nil {
		return s, err
	}
	ends = ascending(ends)
	for start := 0; start < len(ends); start += model.MaxPageItems {
		ids, err := r.NodeIDs(ctx, ends[start:min(start+model.MaxPageItems, len(ends))])
		if err != nil {
			return s, err
		}
		decls, err := dr.Declared(ctx, ids)
		if err != nil {
			return s, err
		}
		for _, d := range decls {
			if d.Node.FileID != "" && d.Node.FileID != s.FileID {
				s.Linked[d.Node.FileID] = true
			}
		}
	}
	return s, nil
}

// shapeKinds are the declaration kinds a site's shape can call: methods for a
// member call, functions for a bare call or a call through a value, classes
// and structs for a construction.
func shapeKinds(shape model.CallShape) []model.NodeKind {
	switch shape {
	case model.CallShapeMember:
		return []model.NodeKind{model.NodeMethod}
	case model.CallShapeBare, model.CallShapeValue:
		return []model.NodeKind{model.NodeFunction}
	case model.CallShapeConstruct:
		return []model.NodeKind{model.NodeClass, model.NodeStruct}
	}
	return nil
}
