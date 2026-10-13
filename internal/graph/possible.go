package graph

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// Possible rows (ADR-0015 decisions 3 and 6): the `may_call` and `may_import`
// relations a call or import answer lists after every precise row. They are
// never stored. Each is computed per query from the open sites the linker
// publishes -- an open call site named by its callee, an open import site
// carrying its path's literal fragments -- and carries the occurrences of the
// stored relation to that site, so a possible row is located exactly as the
// call or import it stands for.
//
//   - callers: for each start node, the callers of every open call site the
//     node is a candidate of, one `may_call` per caller.
//   - callees: for each start node, the candidates of every open call site it
//     calls, ranked as Candidates ranks them, one `may_call` per candidate.
//   - an outgoing `imports` walk: for each start node, the files every open
//     import site it holds may import, one `may_import` per file.
//
// The rows are one hop from the start nodes. A request that names `may_call`
// (or `may_import`) walks on through them: the rows' far ends are the next
// hop's starts, up to the walk's depth bound. A precise walk never continues
// from a possible row.

// tailMode is which possible rows a walk lists.
type tailMode uint8

const (
	tailNone tailMode = iota
	tailCallers
	tailCallees
	tailImports
)

// tailFor is the possible rows a walk in direction over kinds lists.
func tailFor(direction model.Direction, kinds []model.RelationKind) tailMode {
	calls := slices.Contains(kinds, model.RelCalls) || slices.Contains(kinds, model.RelMayCall)
	imports := slices.Contains(kinds, model.RelImports) || slices.Contains(kinds, model.RelMayImport)
	switch {
	case calls && direction == model.DirectionIncoming:
		return tailCallers
	case calls && direction == model.DirectionOutgoing:
		return tailCallees
	case imports && direction == model.DirectionOutgoing:
		return tailImports
	}
	return tailNone
}

// kind is the relation kind the mode's rows carry.
func (m tailMode) kind() model.RelationKind {
	if m == tailImports {
		return model.RelMayImport
	}
	return model.RelMayCall
}

// possibleRow is one computed relation and the stored relations to the open
// sites it stands for, whose occurrences it carries.
type possibleRow struct {
	Rel model.Relation
	Via []model.RelationID
}

// possibleList is a walk's whole ordered list of possible rows.
type possibleList struct {
	rows []possibleRow
	// sites are the distinct open sites the rows came from.
	sites int64
}

// sortedNodes is ids ascending without duplicates, in a new slice.
func sortedNodes(ids []model.NodeID) []model.NodeID {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// storedEnd is one stored relation of a node's list and the node at its
// other end.
type storedEnd struct {
	Rel   model.RelationID
	Other model.NodeID
}

// storedList reads every entry of kind on one node's list in direction.
func storedList(ctx context.Context, r GraphReader, nodes []model.NodeID, direction model.Direction,
	kind model.RelationKind) (map[model.NodeID][]storedEnd, error) {
	out := map[model.NodeID][]storedEnd{}
	code, ok := r.Kinds().Code(kind)
	if !ok || len(nodes) == 0 {
		return out, nil
	}
	ids := sortedNodes(nodes)
	refs, err := r.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	owner := map[NodeRef]model.NodeID{}
	var live []NodeRef
	for i, ref := range refs {
		if ref != 0 {
			owner[ref] = ids[i]
			live = append(live, ref)
		}
	}
	if len(live) == 0 {
		return out, nil
	}
	var edges []Edge
	if _, err := r.Neighbours(ctx, ascending(live), direction, []KindCode{code}, EdgePos{}, func(e Edge) error {
		edges = append(edges, e)
		return nil
	}); err != nil {
		return nil, err
	}
	for start := 0; start < len(edges); start += model.MaxPageItems {
		chunk := edges[start:min(start+model.MaxPageItems, len(edges))]
		rels := make([]RelRef, len(chunk))
		others := make([]NodeRef, len(chunk))
		for i, e := range chunk {
			rels[i], others[i] = e.Rel, e.Neighbour
		}
		relIDs, err := r.RelationIDs(ctx, rels)
		if err != nil {
			return nil, err
		}
		otherIDs, err := r.NodeIDs(ctx, others)
		if err != nil {
			return nil, err
		}
		for i, e := range chunk {
			if relIDs[i] == "" || otherIDs[i] == "" {
				continue
			}
			o := owner[e.Owner]
			out[o] = append(out[o], storedEnd{Rel: relIDs[i], Other: otherIDs[i]})
		}
	}
	return out, nil
}

// possibleRows computes a walk's possible rows from its start nodes. With
// through set the rows' far ends are walked on, up to depth hops.
func (e *Engine) possibleRows(ctx context.Context, r GraphReader, mode tailMode, starts []model.NodeID,
	through bool, depth int) (possibleList, error) {
	var list possibleList
	dr := declarationReader(r)
	if dr == nil || mode == tailNone {
		return list, nil
	}
	repo := r.Binding().RepositoryID
	at := map[model.RelationID]int{}
	sites := map[model.NodeID]bool{}
	seen := map[model.NodeID]bool{}
	for _, s := range starts {
		seen[s] = true
	}
	add := func(from, to model.NodeID, via model.RelationID) model.NodeID {
		id := model.NewRelationID(repo, from, mode.kind(), to)
		if i, ok := at[id]; ok {
			if !slices.Contains(list.rows[i].Via, via) {
				list.rows[i].Via = append(list.rows[i].Via, via)
			}
			return ""
		}
		at[id] = len(list.rows)
		list.rows = append(list.rows, possibleRow{
			Rel: model.Relation{ID: id, From: from, Kind: mode.kind(), To: to}, Via: []model.RelationID{via}})
		if mode == tailCallers {
			return from
		}
		return to
	}
	level := sortedNodes(starts)
	for hop := 1; len(level) > 0; hop++ {
		var next []model.NodeID
		reach := func(n model.NodeID) {
			if n != "" && !seen[n] {
				seen[n] = true
				next = append(next, n)
			}
		}
		switch mode {
		case tailCallers:
			if err := callersOfSites(ctx, r, dr, level, sites, func(caller, start model.NodeID, via model.RelationID) {
				reach(add(caller, start, via))
			}); err != nil {
				return possibleList{}, err
			}
		case tailCallees:
			if err := candidatesOfSites(ctx, r, dr, level, sites, func(start, cand model.NodeID, via model.RelationID) {
				reach(add(start, cand, via))
			}); err != nil {
				return possibleList{}, err
			}
		case tailImports:
			if err := e.filesOfImportSites(ctx, r, dr, level, sites, func(start, file model.NodeID, via model.RelationID) {
				reach(add(start, file, via))
			}); err != nil {
				return possibleList{}, err
			}
		}
		if !through || (depth > 0 && hop >= depth) {
			break
		}
		level = sortedNodes(next)
	}
	list.sites = int64(len(sites))
	return list, nil
}

// callersOfSites finds, for each node of level, the open call sites it is a
// candidate of and each site's callers, in (start, site, caller) order.
func callersOfSites(ctx context.Context, r GraphReader, dr DeclarationReader, level []model.NodeID,
	sites map[model.NodeID]bool, emit func(caller, start model.NodeID, via model.RelationID)) error {
	decls, err := dr.Declared(ctx, level)
	if err != nil {
		return err
	}
	slices.SortFunc(decls, func(a, b Declaration) int { return cmp.Compare(a.Node.ID, b.Node.ID) })
	for _, d := range decls {
		named, err := sitesNaming(ctx, dr, d.Node)
		if err != nil {
			return err
		}
		if len(named) == 0 {
			continue
		}
		callers, err := storedList(ctx, r, named, model.DirectionIncoming, model.RelCalls)
		if err != nil {
			return err
		}
		for _, site := range named {
			sites[site] = true
			for _, c := range callers[site] {
				emit(c.Other, d.Node.ID, c.Rel)
			}
		}
	}
	return nil
}

// sitesNaming lists, by node id, the open call sites whose candidates include
// declaration n.
func sitesNaming(ctx context.Context, dr DeclarationReader, n model.Node) ([]model.NodeID, error) {
	if !isDeclared(n) || n.Name == "" {
		return nil, nil
	}
	var out []model.NodeID
	var after model.NodeID
	for {
		page, next, err := dr.Declarations(ctx, n.Name, nil, after, model.MaxPageItems)
		if err != nil {
			return nil, err
		}
		for _, d := range page {
			if site, ok := openCallSite(d); ok && site.Reason != model.ReasonComputed && site.fits(n) {
				out = append(out, d.Node.ID)
			}
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// candidatesOfSites finds, for each node of level, the open call sites it
// calls and each site's ranked candidates, in (start, site, rank) order.
func candidatesOfSites(ctx context.Context, r GraphReader, dr DeclarationReader, level []model.NodeID,
	sites map[model.NodeID]bool, emit func(start, cand model.NodeID, via model.RelationID)) error {
	calls, err := storedList(ctx, r, level, model.DirectionOutgoing, model.RelCalls)
	if err != nil {
		return err
	}
	for _, start := range sortedNodes(level) {
		ends := calls[start]
		if len(ends) == 0 {
			continue
		}
		targets := make([]model.NodeID, len(ends))
		for i, c := range ends {
			targets[i] = c.Other
		}
		decls, err := dr.Declared(ctx, sortedNodes(targets))
		if err != nil {
			return err
		}
		open := map[model.NodeID]Declaration{}
		for _, d := range decls {
			if isOpenCallSite(d.Node) {
				open[d.Node.ID] = d
			}
		}
		slices.SortFunc(ends, func(a, b storedEnd) int { return cmp.Compare(a.Other, b.Other) })
		for _, c := range ends {
			d, ok := open[c.Other]
			if !ok {
				continue
			}
			sites[c.Other] = true
			site, _ := openCallSite(d)
			if site, err = withImports(ctx, r, dr, site); err != nil {
				return err
			}
			ranked, err := rankCandidates(ctx, dr, site)
			if err != nil {
				return err
			}
			for _, ids := range ranked {
				for _, cand := range ids {
					emit(start, cand, c.Rel)
				}
			}
		}
	}
	return nil
}

// filesOfImportSites finds, for each node of level, the open import sites it
// imports and the files each may import, in (start, site, file) order.
func (e *Engine) filesOfImportSites(ctx context.Context, r GraphReader, dr DeclarationReader, level []model.NodeID,
	sites map[model.NodeID]bool, emit func(start, file model.NodeID, via model.RelationID)) error {
	imports, err := storedList(ctx, r, level, model.DirectionOutgoing, model.RelImports)
	if err != nil {
		return err
	}
	var files []model.FileVersion
	loaded := false
	for _, start := range sortedNodes(level) {
		ends := imports[start]
		if len(ends) == 0 {
			continue
		}
		targets := make([]model.NodeID, 0, len(ends)+1)
		for _, c := range ends {
			targets = append(targets, c.Other)
		}
		decls, err := dr.Declared(ctx, sortedNodes(append(targets, start)))
		if err != nil {
			return err
		}
		byID := map[model.NodeID]Declaration{}
		for _, d := range decls {
			byID[d.Node.ID] = d
		}
		slices.SortFunc(ends, func(a, b storedEnd) int { return cmp.Compare(a.Other, b.Other) })
		for _, c := range ends {
			d, ok := byID[c.Other]
			if !ok {
				continue
			}
			fragments, rows, ok := openImportFragments(d.Node)
			if !ok {
				continue
			}
			if rows > 0 {
				if fragments, err = rowFragments(ctx, dr, d.Node.ID, rows); err != nil {
					return err
				}
			}
			sites[c.Other] = true
			if !loaded {
				if files, err = snapshotFiles(ctx, dr); err != nil {
					return err
				}
				loaded = true
			}
			importer := byID[start]
			if importer.Path == "" {
				importer = d
			}
			matched := matchImportFragments(files, fragments, importer.Path)
			nodes, err := dr.FileNodes(ctx, matched)
			if err != nil {
				return err
			}
			for _, f := range matched {
				if id, ok := nodes[f.ID]; ok && id != start {
					emit(start, id, c.Rel)
				}
			}
		}
	}
	return nil
}

// snapshotFiles reads the pinned snapshot's whole manifest, deleted entries
// left out.
func snapshotFiles(ctx context.Context, dr DeclarationReader) ([]model.FileVersion, error) {
	var out []model.FileVersion
	var after model.FileID
	for {
		page, next, err := dr.SnapshotFiles(ctx, after, model.MaxPageItems)
		if err != nil {
			return nil, err
		}
		for _, f := range page {
			if f.Status != model.FileDeleted {
				out = append(out, f)
			}
		}
		if next == "" {
			return out, nil
		}
		after = next
	}
}

// matchImportFragments lists the files an open import site with fragments
// may import, nearest first: the files of the importer's own directory, then
// every other, each group by path. A path matches when the fragments occur in
// it in order; a first fragment that is relative (`./`, `../`) is resolved
// against the importer's directory and must begin the path. A site with no
// literal fragment matches nothing: every file would match, which says
// nothing. The importer's own file is never its own import.
func matchImportFragments(files []model.FileVersion, fragments []string, importer string) []model.FileVersion {
	var parts []string
	for _, f := range fragments {
		if f != "" {
			parts = append(parts, f)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	dir := path.Dir(importer)
	prefix := ""
	if first := parts[0]; strings.HasPrefix(first, "./") || strings.HasPrefix(first, "../") {
		prefix = path.Join(dir, first)
		if strings.HasSuffix(first, "/") {
			prefix += "/"
		}
		if prefix == "./" || prefix == "." {
			prefix = ""
		}
		parts = parts[1:]
	}
	var out []model.FileVersion
	for _, f := range files {
		if f.Path == importer || !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest, ok := f.Path[len(prefix):], true
		for _, p := range parts {
			i := strings.Index(rest, p)
			if i < 0 {
				ok = false
				break
			}
			rest = rest[i+len(p):]
		}
		if ok {
			out = append(out, f)
		}
	}
	near := func(f model.FileVersion) int {
		if path.Dir(f.Path) == dir {
			return 0
		}
		return 1
	}
	slices.SortStableFunc(out, func(a, b model.FileVersion) int {
		if c := cmp.Compare(near(a), near(b)); c != 0 {
			return c
		}
		return cmp.Compare(a.Path, b.Path)
	})
	return out
}

// possiblePage is the part of a possible list one page serves.
type possiblePage struct {
	relations   []model.Relation
	occurrences []model.RelationOccurrence
	nodes       []model.NodeID
	marks       []model.GraphRowMark
	notices     []string
	// next is the position after the page's last row, -1 when the list is
	// served to its end.
	next int64
}

// servePossible serves rows of list from at, as many as room relations and
// the answer's remaining occurrence budget hold. A row goes whole, except the
// one row of an otherwise empty page whose own sites outnumber a whole
// answer, which is served with as many as one answer carries and a notice
// giving the exact count. sites is how many occurrences the page already
// carries, and filled whether it already carries rows.
func (e *Engine) servePossible(ctx context.Context, r GraphReader, list possibleList, at int64, room int,
	sites int, filled bool) (possiblePage, error) {
	page := possiblePage{next: -1}
	loc, locating := r.(SourceLocator)
	// Every candidate of one site carries that site's occurrences: each
	// stored relation's are read once per page.
	read := map[model.RelationID][]model.RelationOccurrence{}
	i := int(at)
	for ; i < len(list.rows); i++ {
		if len(page.relations) >= room {
			break
		}
		row := list.rows[i]
		var occ []model.RelationOccurrence
		if locating {
			for _, via := range row.Via {
				listed, ok := read[via]
				if !ok {
					var err error
					if listed, _, err = e.relationSites(ctx, loc, via, occurrenceBudget); err != nil {
						return possiblePage{}, err
					}
					read[via] = listed
				}
				for _, o := range listed {
					o.RelationID = row.Rel.ID
					occ = append(occ, o)
				}
			}
		}
		if sites+len(page.occurrences)+len(occ) > occurrenceBudget {
			if filled || len(page.relations) > 0 {
				break
			}
			page.notices = append(page.notices, fmt.Sprintf(
				"possible relation %s has %d located occurrences; %d are listed, which is all one answer carries",
				row.Rel.ID, len(occ), occurrenceBudget-sites))
			occ = occ[:occurrenceBudget-sites]
		}
		page.relations = append(page.relations, row.Rel)
		page.occurrences = append(page.occurrences, occ...)
		page.nodes = append(page.nodes, row.Rel.From, row.Rel.To)
		page.marks = append(page.marks, model.GraphRowMark{RelationID: row.Rel.ID, Kind: model.MarkNameOnly,
			Possible: true})
	}
	if i < len(list.rows) {
		page.next = int64(i)
	}
	return page, nil
}

// possibleNotice says, in plain words, what the possible rows are; and, for a
// callers answer with no caller at all, how many of the project's call sites
// are bound and open, from the call linker's capability counts.
func possibleNotice(mode tailMode, list possibleList, precise int64, caps []model.CapabilityState) string {
	rows := int64(len(list.rows))
	switch {
	case mode == tailCallers && rows > 0:
		callers := map[model.NodeID]bool{}
		for _, r := range list.rows {
			callers[r.Rel.From] = true
		}
		return fmt.Sprintf("%d open call sites in %d callers name this symbol and no analysis bound them: "+
			"they are listed by name only, as `may_call`, and may not call it", list.sites, len(callers))
	case mode == tailCallers && precise == 0:
		return "no caller was found, by analysis or by name: " + siteCounts(caps)
	case mode == tailCallees && rows > 0:
		return fmt.Sprintf("%d open call sites name a callee no analysis bound: its %d candidates of the same "+
			"name and shape are listed by name only, as `may_call`, and may not be what runs", list.sites, rows)
	case mode == tailImports && rows > 0:
		return fmt.Sprintf("%d imports name a path that does not fold to a constant: the %d files whose paths "+
			"match their literal parts are listed as `may_import`, and may not be what is imported", list.sites, rows)
	}
	return ""
}

// siteCountKeys are the call linker's exact site counts, in the order a
// notice quotes them.
var siteCountKeys = []string{"sites", "bound_sites", "open_sites", "computed_sites", "external_sites"}

// siteCounts quotes the project's call-site counts: each key summed over
// every `calls` capability row that carries it, or says the index reports
// none.
//
// The stored rows stand for disjoint sets of the linker's units -- its fresh
// units folded into one workspace row, each degraded unit in a row of its
// own -- so their sum is the project's figure and no unit is counted twice.
// Any one row alone is a part of it. A value that is not a count is left out of the sum rather than read
// as zero.
func siteCounts(caps []model.CapabilityState) string {
	sums := make([]int64, len(siteCountKeys))
	seen := make([]bool, len(siteCountKeys))
	for _, c := range caps {
		if c.Capability != string(model.RelCalls) {
			continue
		}
		for i, k := range siteCountKeys {
			if n, err := strconv.ParseInt(c.Details[k], 10, 64); err == nil {
				sums[i] += n
				seen[i] = true
			}
		}
	}
	var parts []string
	for i, k := range siteCountKeys {
		if seen[i] {
			parts = append(parts, strings.ReplaceAll(k, "_", " ")+" "+strconv.FormatInt(sums[i], 10))
		}
	}
	if len(parts) == 0 {
		return "this index reports no call-site counts, so how many calls no analysis bound is not known"
	}
	return "the project's call sites are " + strings.Join(parts, ", ")
}

// dropPlaceholders removes, from one page of a call answer, the structural
// provider's `call:<name>` placeholder for every site an open call site also
// stands for (same file, overlapping call range): one row per open site, never
// two. Whether an open site covers a placeholder's site is read from the
// pinned generation -- the caller's stored calls to open sites and their
// occurrences -- and not from the page, so a placeholder whose open site is
// served on another page is dropped all the same. A placeholder relation keeps
// the occurrences no open site covers, and is dropped only when none is left;
// a placeholder node no relation of the page still names is dropped with it.
// It returns the relation ids it dropped.
func (e *Engine) dropPlaceholders(ctx context.Context, r GraphReader, nodes []model.Node, relations []model.Relation,
	where *located) ([]model.Node, []model.Relation, map[model.RelationID]bool, error) {
	placeholder := map[model.NodeID]bool{}
	for _, n := range nodes {
		if isCallPlaceholder(n) {
			placeholder[n.ID] = true
		}
	}
	toPlaceholder := map[model.RelationID]bool{}
	var callers []model.NodeID
	for _, rel := range relations {
		if rel.Kind == model.RelCalls && placeholder[rel.To] {
			toPlaceholder[rel.ID] = true
			callers = append(callers, rel.From)
		}
	}
	loc, locating := r.(SourceLocator)
	dr := declarationReader(r)
	if len(toPlaceholder) == 0 || !locating || dr == nil {
		return nodes, relations, nil, nil
	}
	calls, err := storedList(ctx, r, callers, model.DirectionOutgoing, model.RelCalls)
	if err != nil {
		return nil, nil, nil, err
	}
	var targets []model.NodeID
	for _, ends := range calls {
		for _, c := range ends {
			targets = append(targets, c.Other)
		}
	}
	open := map[model.NodeID]bool{}
	if len(targets) > 0 {
		decls, err := dr.Declared(ctx, sortedNodes(targets))
		if err != nil {
			return nil, nil, nil, err
		}
		for _, d := range decls {
			if isOpenCallSite(d.Node) {
				open[d.Node.ID] = true
			}
		}
	}
	covered := map[model.FileID][]model.ByteRange{}
	for _, caller := range sortedNodes(callers) {
		for _, c := range calls[caller] {
			if !open[c.Other] {
				continue
			}
			sites, _, err := e.relationSites(ctx, loc, c.Rel, occurrenceBudget)
			if err != nil {
				return nil, nil, nil, err
			}
			for _, o := range sites {
				covered[o.FileID] = append(covered[o.FileID], o.Bytes)
			}
		}
	}
	if len(covered) == 0 {
		return nodes, relations, nil, nil
	}
	overlaps := func(o model.RelationOccurrence) bool {
		for _, b := range covered[o.FileID] {
			if o.Bytes.Start < b.End && b.Start < o.Bytes.End {
				return true
			}
		}
		return false
	}
	kept := map[model.RelationID]int{}
	had := map[model.RelationID]bool{}
	occ := where.occurrences[:0:0]
	for _, o := range where.occurrences {
		if toPlaceholder[o.RelationID] {
			had[o.RelationID] = true
			if overlaps(o) {
				continue
			}
			kept[o.RelationID]++
		}
		occ = append(occ, o)
	}
	where.occurrences = occ
	dropped := map[model.RelationID]bool{}
	named := map[model.NodeID]bool{}
	rels := relations[:0:0]
	for _, rel := range relations {
		if toPlaceholder[rel.ID] && had[rel.ID] && kept[rel.ID] == 0 {
			dropped[rel.ID] = true
			continue
		}
		rels = append(rels, rel)
		named[rel.From], named[rel.To] = true, true
	}
	if len(dropped) == 0 {
		return nodes, relations, nil, nil
	}
	gone := map[model.NodeID]bool{}
	out := nodes[:0:0]
	for _, n := range nodes {
		if placeholder[n.ID] && !named[n.ID] {
			gone[n.ID] = true
			continue
		}
		out = append(out, n)
	}
	locs := where.locations[:0:0]
	for _, l := range where.locations {
		if !gone[l.NodeID] {
			locs = append(locs, l)
		}
	}
	where.locations = locs
	return out, rels, dropped, nil
}

// openSiteCounter counts, per depth of an impact walk, the open call sites
// whose candidates include a node the walk reached: one batched lookup per
// distinct name per level. Records arrive in frontier order, depth
// ascending, so a level is counted when the first record of the next one
// arrives, and the last one by flush. A site is counted once, at the first
// depth it names a reached node.
//
// Its sets -- the level's reached nodes, their names, and every site counted
// so far -- start in heap and move to a scratch database (openScratch, under
// the path searches' fixed page cache) once they hold openSiteHeapEntries
// between them, so the common impact pays nothing for the scratch and a walk
// over a monorepo's affected set holds a bounded working set. The count is the
// same either way: the sets are exact in both, and each level is read in node
// order and counted in name order.
type openSiteCounter struct {
	dr         DeclarationReader
	b          *budget
	depth      int
	scratchDir string
	pending    map[model.NodeID]bool
	seen       map[model.NodeID]bool
	// sc holds the sets once they spilled, and is nil until then.
	sc *pathScratch
}

// openSiteHeapEntries is how many nodes and sites the counter holds in heap
// before it moves its sets to scratch. It sizes a working set, never a count:
// every site is counted either way. It is a variable only so a test can make
// the counter spill.
var openSiteHeapEntries = 1 << 16

// openSiteCountSchema is the counter's scratch once it spills.
const openSiteCountSchema = `
CREATE TABLE pending(node TEXT PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE named(name TEXT NOT NULL, node TEXT NOT NULL, kind TEXT NOT NULL, lang TEXT NOT NULL, sig TEXT NOT NULL,
	file TEXT NOT NULL, PRIMARY KEY(name, node)) WITHOUT ROWID;
CREATE TABLE seen(site TEXT PRIMARY KEY) WITHOUT ROWID;
`

func newOpenSiteCounter(dr DeclarationReader, b *budget, scratchDir string) *openSiteCounter {
	return &openSiteCounter{dr: dr, b: b, scratchDir: scratchDir, pending: map[model.NodeID]bool{}, seen: map[model.NodeID]bool{}}
}

// close releases the scratch, if the sets spilled into one.
func (c *openSiteCounter) close() {
	if c.sc != nil {
		c.sc.close()
		c.sc = nil
	}
}

// note records one affected node.
func (c *openSiteCounter) note(ctx context.Context, rec impactRecord) error {
	if c.dr == nil {
		return nil
	}
	if rec.Depth != c.depth {
		if err := c.flush(ctx); err != nil {
			return err
		}
		c.depth = rec.Depth
	}
	if c.sc != nil {
		return c.exec(`INSERT INTO pending(node) VALUES(?1) ON CONFLICT DO NOTHING`, string(rec.NodeID))
	}
	c.pending[rec.NodeID] = true
	return c.spillIfFull()
}

// spillIfFull moves the heap sets to scratch once they reach the bound.
func (c *openSiteCounter) spillIfFull() error {
	if c.sc != nil || len(c.pending)+len(c.seen) < openSiteHeapEntries {
		return nil
	}
	sc, err := openScratch(context.Background(), c.scratchDir, openSiteCountSchema)
	if err != nil {
		return err
	}
	c.sc = sc
	for id := range c.pending {
		if err := c.exec(`INSERT INTO pending(node) VALUES(?1)`, string(id)); err != nil {
			return err
		}
	}
	for id := range c.seen {
		if err := c.exec(`INSERT INTO seen(site) VALUES(?1)`, string(id)); err != nil {
			return err
		}
	}
	c.pending, c.seen = nil, nil
	return nil
}

// exec runs one scratch statement. The scratch is a local file the count must
// leave consistent, so it runs outside the request's cancellation.
func (c *openSiteCounter) exec(query string, args ...any) error {
	if _, err := c.sc.tx.ExecContext(context.Background(), query, args...); err != nil {
		return internalErr("open-site count scratch: " + err.Error())
	}
	return nil
}

// countSite marks site counted and reports whether it was not counted before.
func (c *openSiteCounter) countSite(site model.NodeID) (bool, error) {
	if c.sc == nil {
		if c.seen[site] {
			return false, nil
		}
		c.seen[site] = true
		return true, c.spillIfFull()
	}
	res, err := c.sc.tx.ExecContext(context.Background(),
		`INSERT INTO seen(site) VALUES(?1) ON CONFLICT DO NOTHING`, string(site))
	if err != nil {
		return false, internalErr("open-site count scratch: " + err.Error())
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, internalErr("open-site count scratch: " + err.Error())
	}
	return n == 1, nil
}

// counted reports whether site was already counted.
func (c *openSiteCounter) counted(site model.NodeID) (bool, error) {
	if c.sc == nil {
		return c.seen[site], nil
	}
	var held bool
	if err := c.sc.tx.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM seen WHERE site = ?1)`, string(site)).Scan(&held); err != nil {
		return false, internalErr("open-site count scratch: " + err.Error())
	}
	return held, nil
}

// pendingPage reads up to limit of the level's nodes after after, in node
// order.
func (c *openSiteCounter) pendingPage(after model.NodeID, limit int) ([]model.NodeID, error) {
	rows, err := c.sc.tx.QueryContext(context.Background(),
		`SELECT node FROM pending WHERE node > ?1 ORDER BY node LIMIT ?2`, string(after), limit)
	if err != nil {
		return nil, internalErr("open-site count scratch: " + err.Error())
	}
	defer rows.Close()
	var out []model.NodeID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, internalErr("open-site count scratch: " + err.Error())
		}
		out = append(out, model.NodeID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("open-site count scratch: " + err.Error())
	}
	return out, nil
}

// namedNodes reads the level's nodes named name, from scratch, with every
// field a site's fit reads (callSite.fits): a node read back without its file
// is not a declaration, and no site would fit it.
func (c *openSiteCounter) namedNodes(name string) ([]model.Node, error) {
	rows, err := c.sc.tx.QueryContext(context.Background(),
		`SELECT node, kind, lang, sig, file FROM named WHERE name = ?1 ORDER BY node`, name)
	if err != nil {
		return nil, internalErr("open-site count scratch: " + err.Error())
	}
	defer rows.Close()
	var out []model.Node
	for rows.Next() {
		n := model.Node{Name: name}
		var id, kind, file string
		if err := rows.Scan(&id, &kind, &n.Language, &n.Signature, &file); err != nil {
			return nil, internalErr("open-site count scratch: " + err.Error())
		}
		n.ID, n.Kind, n.FileID = model.NodeID(id), model.NodeKind(kind), model.FileID(file)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("open-site count scratch: " + err.Error())
	}
	return out, nil
}

// nextName reads the first name of the level after after, "" at the end.
func (c *openSiteCounter) nextName(after string) (string, error) {
	var name string
	err := c.sc.tx.QueryRowContext(context.Background(),
		`SELECT name FROM named WHERE name > ?1 ORDER BY name LIMIT 1`, after).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", internalErr("open-site count scratch: " + err.Error())
	}
	return name, nil
}

// flush counts the pending level.
func (c *openSiteCounter) flush(ctx context.Context) error {
	if c.dr == nil || c.depth < 1 || (c.sc == nil && len(c.pending) == 0) {
		return nil
	}
	// The level's declared nodes, grouped by name: in heap, or in the
	// scratch's named table once the sets spilled.
	var byName map[string][]model.Node
	declare := func(ids []model.NodeID) error {
		decls, err := c.dr.Declared(ctx, ids)
		if err != nil {
			return err
		}
		for _, d := range decls {
			if !isDeclared(d.Node) || d.Node.Name == "" {
				continue
			}
			if c.sc == nil {
				byName[d.Node.Name] = append(byName[d.Node.Name], d.Node)
				continue
			}
			if err := c.exec(`INSERT INTO named(name, node, kind, lang, sig, file) VALUES(?1, ?2, ?3, ?4, ?5, ?6) ON CONFLICT DO NOTHING`,
				d.Node.Name, string(d.Node.ID), string(d.Node.Kind), d.Node.Language, d.Node.Signature, string(d.Node.FileID)); err != nil {
				return err
			}
		}
		return nil
	}
	var names []string
	if c.sc == nil {
		byName = map[string][]model.Node{}
		ids := make([]model.NodeID, 0, len(c.pending))
		for id := range c.pending {
			ids = append(ids, id)
		}
		clear(c.pending)
		slices.Sort(ids)
		for start := 0; start < len(ids); start += model.MaxPageItems {
			if err := declare(ids[start:min(start+model.MaxPageItems, len(ids))]); err != nil {
				return err
			}
		}
		names = make([]string, 0, len(byName))
		for n := range byName {
			names = append(names, n)
		}
		slices.Sort(names)
	} else {
		var after model.NodeID
		for {
			ids, err := c.pendingPage(after, model.MaxPageItems)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				break
			}
			if err := declare(ids); err != nil {
				return err
			}
			after = ids[len(ids)-1]
		}
		if err := c.exec(`DELETE FROM pending`); err != nil {
			return err
		}
	}
	// Names in order: from the heap list, or one keyset step at a time from
	// the scratch. A spill during the count leaves the level where it was
	// grouped; only the counted sites move.
	spilled := c.sc != nil
	var count int64
	var lastName string
	for i := 0; ; i++ {
		var name string
		var nodes []model.Node
		if !spilled {
			if i == len(names) {
				break
			}
			name, nodes = names[i], byName[names[i]]
		} else {
			var err error
			if name, err = c.nextName(lastName); err != nil {
				return err
			}
			if name == "" {
				break
			}
			if nodes, err = c.namedNodes(name); err != nil {
				return err
			}
			lastName = name
		}
		var after model.NodeID
		for {
			page, next, err := c.dr.Declarations(ctx, name, nil, after, model.MaxPageItems)
			if err != nil {
				return err
			}
			for _, d := range page {
				site, ok := openCallSite(d)
				if !ok || site.Reason == model.ReasonComputed {
					continue
				}
				done, err := c.counted(d.Node.ID)
				if err != nil {
					return err
				}
				if done {
					continue
				}
				for _, n := range nodes {
					if site.fits(n) {
						fresh, err := c.countSite(d.Node.ID)
						if err != nil {
							return err
						}
						if fresh {
							count++
						}
						break
					}
				}
			}
			if next == "" {
				break
			}
			after = next
		}
	}
	if spilled {
		if err := c.exec(`DELETE FROM named`); err != nil {
			return err
		}
	}
	for len(c.b.openSites) < c.depth {
		c.b.openSites = append(c.b.openSites, 0)
	}
	c.b.openSites[c.depth-1] += count
	return nil
}

// expandOpenSites adds to an exhausted impact walk's retained input the nodes
// reached through open call sites: for an incoming impact the callers of the
// open sites an affected node is a candidate of, for an outgoing one the
// candidates of the open sites it calls, at one more depth and the cost of a
// `may_call` hop, and on from those, level by level, up to the depth bound.
// Each record's route ends in the computed relation, so the ranking folds and
// orders it with the precise records.
//
// The walk's records are read back once to find each affected node's best
// depth, cost and route. That table is the size of the affected set, so it
// lives in a scratch database under a fixed page cache (openScratch), never in
// heap: one keyed upsert per record, then each level read back in node order
// a chunk at a time, one point lookup per row for its parent's record and one
// conditional insert per reached node. A level is expanded a chunk of starts
// at a time; possibleRows emits a start's rows together and in start order,
// so the chunks concatenated are the level's rows in the level's order.
//
// The expansion runs inside the one request that finished the walk and its
// scratch ends with that request. A deadline that lands inside it ends the
// expansion and the answer is marked truncated (impactPhaseError), exactly as
// before; there is no partial expansion for a continuation to resume.
//
// A reader without the declaration seam cannot list any open site's
// candidates, so the expansion answers what Candidates answers on that reader:
// unavailable, never an empty expansion that reads as "no possible callers".
func (e *Engine) expandOpenSites(ctx context.Context, r GraphReader, req model.ImpactRequest, maxDepth config.Limit,
	retain *retainedWalk) (err error) {
	if declarationReader(r) == nil {
		return candidatesUnavailable()
	}
	var modes []tailMode
	switch req.Direction {
	case model.DirectionIncoming:
		modes = []tailMode{tailCallers}
	case model.DirectionOutgoing:
		modes = []tailMode{tailCallees}
	default:
		modes = []tailMode{tailCallers, tailCallees}
	}
	// The scratch is a local file the expansion must leave consistent, so its
	// statements run outside the request's cancellation, as the path
	// searches' do; the deadline is honoured by the graph reads between them.
	scCtx := context.WithoutCancel(ctx)
	sc, err := openScratch(scCtx, e.scratchDir(), openSiteScratchSchema)
	if err != nil {
		return err
	}
	defer sc.close()
	best := &bestTable{tx: sc.tx}
	if err := best.prepare(scCtx); err != nil {
		return err
	}
	defer best.closeStatements()
	seed := map[model.NodeID]bool{}
	for _, s := range req.Start {
		seed[s] = true
		if err := best.offer(scCtx, impactRecord{NodeID: s}); err != nil {
			return err
		}
	}
	if err := retain.eachEntry(func(rec impactRecord) error {
		if seed[rec.NodeID] {
			return nil
		}
		return best.offer(scCtx, rec)
	}); err != nil {
		return err
	}
	for depth := 0; ; depth++ {
		more, err := best.hasLevel(scCtx, depth)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
		if atBound(maxDepth, depth) {
			continue
		}
		for _, mode := range modes {
			direction := model.DirectionIncoming
			if mode == tailCallees {
				direction = model.DirectionOutgoing
			}
			var after model.NodeID
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				level, err := best.level(scCtx, depth, after, openSiteChunk)
				if err != nil {
					return err
				}
				if len(level) == 0 {
					break
				}
				after = level[len(level)-1].NodeID
				parents := make(map[model.NodeID]impactRecord, len(level))
				starts := make([]model.NodeID, len(level))
				for i, rec := range level {
					parents[rec.NodeID], starts[i] = rec, rec.NodeID
				}
				list, err := e.possibleRows(ctx, r, mode, starts, false, 0)
				if err != nil {
					return err
				}
				// A parent that reaches a site is admitted into the route
				// table once, so its children name its route by reference
				// (routetable.go). A seed is no admission: its children's
				// routes start at it.
				admitted := make(map[model.NodeID]uint64, len(level))
				admit := func(p impactRecord) (uint64, error) {
					if p.Via == "" {
						return 0, nil
					}
					if id, ok := admitted[p.NodeID]; ok {
						return id, nil
					}
					id, err := retain.admitRoute(p.Adm, p.Via)
					if err != nil {
						return 0, err
					}
					admitted[p.NodeID] = id
					return id, nil
				}
				for _, row := range list.rows {
					from, to := row.Rel.To, row.Rel.From
					if mode == tailCallees {
						from, to = row.Rel.From, row.Rel.To
					}
					if seed[to] {
						continue
					}
					parent := parents[from]
					adm, err := admit(parent)
					if err != nil {
						return err
					}
					rec := impactRecord{
						NodeID:    to,
						Depth:     depth + 1,
						Cost:      parent.Cost + Cost(model.RelMayCall),
						Direction: direction,
						Via:       row.Rel.ID,
						Parent:    from,
						Adm:       adm,
						Hash:      parent.Hash.extend(row.Rel.ID),
						Reasons:   []string{impactReason(model.RelMayCall, direction, depth+1)},
						Possible:  true,
					}
					if err := retain.addEntry(rec); err != nil {
						return err
					}
					if err := best.reach(scCtx, rec); err != nil {
						return err
					}
				}
				if len(level) < openSiteChunk {
					break
				}
			}
		}
	}
}

// openSiteChunk is how many nodes of one level expandOpenSites holds at once.
// It sizes a working set, never an answer: every node of the level is
// expanded, a chunk at a time. It is a variable only so a test can make a
// level span several chunks.
var openSiteChunk = 4096

// openSiteScratchSchema is expandOpenSites' scratch: each affected node's
// best record, keyed by node, with the level index the expansion reads.
const openSiteScratchSchema = `
CREATE TABLE best(node TEXT PRIMARY KEY, depth INTEGER NOT NULL, cost INTEGER NOT NULL, rec BLOB NOT NULL) WITHOUT ROWID;
CREATE INDEX best_level ON best(depth, node);
`

// bestTable is the best record of each affected node, in the scratch.
type bestTable struct {
	tx                   *sql.Tx
	offerStmt, reachStmt *sql.Stmt
	levelStmt, hasStmt   *sql.Stmt
}

func (b *bestTable) prepare(ctx context.Context) error {
	var err error
	// A record replaces the one held when it is shallower, or as deep and
	// cheaper; the first of two equal ones stays, as a fold in arrival order
	// keeps it.
	if b.offerStmt, err = b.tx.PrepareContext(ctx, `INSERT INTO best(node, depth, cost, rec) VALUES(?1, ?2, ?3, ?4)
		ON CONFLICT(node) DO UPDATE SET depth = excluded.depth, cost = excluded.cost, rec = excluded.rec
		WHERE excluded.depth < best.depth OR (excluded.depth = best.depth AND excluded.cost < best.cost)`); err != nil {
		return internalErr("open-site scratch: " + err.Error())
	}
	// A node the expansion reaches is recorded only the first time: the one
	// already held is at most as deep.
	if b.reachStmt, err = b.tx.PrepareContext(ctx, `INSERT INTO best(node, depth, cost, rec) VALUES(?1, ?2, ?3, ?4)
		ON CONFLICT(node) DO NOTHING`); err != nil {
		return internalErr("open-site scratch: " + err.Error())
	}
	if b.levelStmt, err = b.tx.PrepareContext(ctx, `SELECT rec FROM best WHERE depth = ?1 AND node > ?2 ORDER BY node LIMIT ?3`); err != nil {
		return internalErr("open-site scratch: " + err.Error())
	}
	if b.hasStmt, err = b.tx.PrepareContext(ctx, `SELECT EXISTS(SELECT 1 FROM best WHERE depth >= ?1)`); err != nil {
		return internalErr("open-site scratch: " + err.Error())
	}
	return nil
}

func (b *bestTable) closeStatements() {
	for _, s := range []*sql.Stmt{b.offerStmt, b.reachStmt, b.levelStmt, b.hasStmt} {
		if s != nil {
			s.Close()
		}
	}
}

func (b *bestTable) put(ctx context.Context, stmt *sql.Stmt, rec impactRecord) error {
	raw, err := encodeImpactRecord(rec)
	if err != nil {
		return err
	}
	if _, err := stmt.ExecContext(ctx, string(rec.NodeID), rec.Depth, int64(rec.Cost), raw); err != nil {
		return internalErr("open-site scratch: " + err.Error())
	}
	return nil
}

// offer folds one walk record into the table.
func (b *bestTable) offer(ctx context.Context, rec impactRecord) error {
	return b.put(ctx, b.offerStmt, rec)
}

// reach records a node the expansion reached, unless it is already held.
func (b *bestTable) reach(ctx context.Context, rec impactRecord) error {
	return b.put(ctx, b.reachStmt, rec)
}

// hasLevel reports whether any node is held at depth or deeper.
func (b *bestTable) hasLevel(ctx context.Context, depth int) (bool, error) {
	var held bool
	if err := b.hasStmt.QueryRowContext(ctx, depth).Scan(&held); err != nil {
		return false, internalErr("open-site scratch: " + err.Error())
	}
	return held, nil
}

// level reads up to limit records of depth after node after, in node order.
func (b *bestTable) level(ctx context.Context, depth int, after model.NodeID, limit int) ([]impactRecord, error) {
	rows, err := b.levelStmt.QueryContext(ctx, depth, string(after), limit)
	if err != nil {
		return nil, internalErr("open-site scratch: " + err.Error())
	}
	defer rows.Close()
	var out []impactRecord
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, internalErr("open-site scratch: " + err.Error())
		}
		rec, err := decodeImpactRecord(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("open-site scratch: " + err.Error())
	}
	return out, nil
}
