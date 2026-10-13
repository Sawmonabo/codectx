package search

// The exact and prefix retrieval tiers (exact_path, exact_qualified_name,
// qualified_name_prefix, exact_name) that serve resolve, document-symbols,
// workspace-symbols and definition.

import (
	"container/list"
	"context"
	"errors"
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// exactReader is the pinned read surface the four non-lexical tiers need. It
// exists so the tiers depend on the reader's method signatures rather than on
// the concrete reader: every method below is *sqlite.PinnedReader's, byte
// for byte, and the assertion under it makes that a build-time fact.
type exactReader interface {
	Node(ctx context.Context, id model.NodeID) (sqlite.StoredNode, error)
	FileByPath(ctx context.Context, path string) (model.FileID, error)
	File(ctx context.Context, id model.FileID) (model.FileVersion, error)
	NodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]sqlite.StoredNode, error)
	DistinctNodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]sqlite.StoredNode, error)
	Nodes(ctx context.Context, f sqlite.NodeFilter, after model.NodeID, limit int) ([]sqlite.StoredNode, error)
	NodesAmong(ctx context.Context, f sqlite.NodeFilter, ids []model.NodeID) ([]sqlite.StoredNode, error)
}

var _ exactReader = (*sqlite.PinnedReader)(nil)

// exactHit is one candidate from tiers 0-3, carrying the whole stored node and
// its file's normalized path: everything model.SearchHit needs except Range --
// Kind, Name, QualifiedName, Signature, FileID and Bytes on the node, Path
// beside it.
type exactHit struct {
	Tier model.SearchTier
	Path string
	Node sqlite.StoredNode
}

// exactTiers is the ranking order of the non-lexical tiers, most to
// least specific. exact_path is not in it: it is keyed by a path, not by a
// symbol query, so exactCandidates runs it separately.
var exactTiers = [...]model.SearchTier{
	model.TierExactQualifiedName, model.TierQualifiedNamePrefix, model.TierExactName,
}

// nodeFilterFor is the NodeFilter that serves one symbol tier. The prefix tier
// goes through NodeFilter.QualifiedPrefix, which storage turns into the indexed
// range [prefix, prefixUpperBound(prefix)) — never a leading wildcard. Identity
// strings are compared as stored: nothing here lowercases a name.
func nodeFilterFor(tier model.SearchTier, query string, kinds []model.NodeKind) sqlite.NodeFilter {
	f := sqlite.NodeFilter{Kinds: kinds}
	switch tier {
	case model.TierExactQualifiedName:
		f.QualifiedName = query
	case model.TierQualifiedNamePrefix:
		f.QualifiedPrefix = query
	default:
		f.Name = query
	}
	return f
}

// normalizeQueryPath reads the query as a root-relative repository path: "/"
// separators, no volume, no leading slash, no "." or ".." segment. It reports
// false when the query is not a path at all, which is not an error — the symbol
// tiers still run on it.
func normalizeQueryPath(query string) (string, bool) {
	q := strings.TrimSpace(query)
	if q == "" || strings.ContainsRune(q, 0) {
		return "", false
	}
	q = strings.ReplaceAll(q, `\`, "/")
	q = strings.TrimPrefix(q, "./")
	if strings.HasPrefix(q, "/") || strings.Contains(q, "//") {
		return "", false
	}
	for _, seg := range strings.Split(q, "/") {
		if seg == "." || seg == ".." || seg == "" {
			return "", false
		}
	}
	cleaned := path.Clean(q)
	if cleaned != q || len(q) > model.MaxPathBytes {
		return "", false
	}
	return q, true
}

// pathResolver caches file paths for one request: tiers 1-3 return nodes that
// carry a FileID but no path, and one page can hold many nodes from one file.
//
// The cache holds at most one entry per file of ONE READ PAGE, least recently
// used evicted first, so the heap it costs is a function of the page and not of
// the answer. A request whose answer spans more files than that still answers
// every one of them: an evicted entry costs one more r.File read and nothing
// else. That is why the bound is not a limit in the sense the scale posture
// makes user-configurable -- it rejects no work, drops no candidate and
// truncates no answer; it is a read-amplification knob whose only observable
// effect is how many times a path is fetched.
type pathResolver struct {
	r exactReader
	// cap is the entry bound, order the LRU chain with the most recently used
	// at the front, and paths the index into it.
	cap   int
	order *list.List
	paths map[model.FileID]*list.Element
}

// cachedPath is one chain entry. It carries its own key so eviction can drop
// the index entry without searching the map.
type cachedPath struct {
	file model.FileID
	path string
}

// newPathResolver sizes the cache from the read page already in scope: a page
// cannot hold candidates from more files than it holds candidates, so a page
// whose nodes all come from distinct files still resolves each path once.
func newPathResolver(r exactReader, page int) *pathResolver {
	return &pathResolver{r: r, cap: max(page, 1), order: list.New(), paths: map[model.FileID]*list.Element{}}
}

// path returns the normalized path of file. A node with no file (a manifest
// dependency, say) has no path and is not an error.
func (p *pathResolver) path(ctx context.Context, file model.FileID) (string, error) {
	if file == "" {
		return "", nil
	}
	if e, ok := p.paths[file]; ok {
		p.order.MoveToFront(e)
		return e.Value.(*cachedPath).path, nil
	}
	fv, err := p.r.File(ctx, file)
	if err != nil {
		return "", err
	}
	if p.order.Len() >= p.cap {
		oldest := p.order.Back()
		delete(p.paths, oldest.Value.(*cachedPath).file)
		p.order.Remove(oldest)
	}
	p.paths[file] = p.order.PushFront(&cachedPath{file: file, path: fv.Path})
	return fv.Path, nil
}

// queryFile reads the query as a root-relative path and resolves it to a
// visible file. ok is false when the query is not a path or names no visible
// file, which is not an error -- the symbol tiers answer the same query.
func queryFile(ctx context.Context, r exactReader, query string) (normalized string, file model.FileID, err error) {
	normalized, ok := normalizeQueryPath(query)
	if !ok {
		return "", "", nil
	}
	file, err = r.FileByPath(ctx, normalized)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
			return "", "", nil
		}
		return "", "", err
	}
	return normalized, file, nil
}

// pathCandidates is tier 0: the query read as a root-relative path, resolved to
// a file (queryFile), then that file's visible nodes in document order.
//
// It pages DistinctNodesInFile on its (start_byte, node_id) keyset rather than
// issuing one bounded read, because storage takes no kind filter and so applies
// the bound BEFORE the filter: a single read of page nodes would answer "0
// hits" for a file whose matching declarations all sit past node page.
//
// The DISTINCT reading is what a retrieval tier needs: a candidate is a node
// identity, and a node two units declare at two offsets would otherwise be
// emitted twice by this tier. document-symbols keeps NodesInFile, which returns
// every declared offset. Storage decides it, so this walk holds no set of the
// ids it has emitted and its heap stays one read page.
//
// page is the READ size, not a bound on the answer: the walk runs the keyset to
// its end and emits every kept candidate. A file's declarations are not dropped
// because they sit past an arbitrary 200, which is the class-E truncation the
// scale posture removes.
func pathCandidates(ctx context.Context, r exactReader, normalized string, file model.FileID, kinds []model.NodeKind,
	page int, emit func(exactHit) error) error {
	if file == "" {
		return nil
	}
	var afterStart int64
	var after model.NodeID
	for {
		// The page size is the caller's read bound, which storage clamps to at
		// most model.MaxPageItems; asking for more would be a bound this code
		// believes and the database does not, and the walk would then end on
		// the first page.
		nodes, err := r.DistinctNodesInFile(ctx, file, afterStart, after, page)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			// An empty page is the end of the file's keyset. Only an empty one
			// ends the walk: the read can return a short page while the
			// keyset continues, and `after` advances strictly on every
			// non-empty page, so this terminates.
			return nil
		}
		for _, n := range nodes {
			afterStart, after = nodeStartByte(n), n.Node.ID
			if !matchesKinds(n.Node.Kind, kinds) {
				continue
			}
			if err := emit(exactHit{Tier: model.TierExactPath, Path: normalized, Node: n}); err != nil {
				return err
			}
		}
	}
}

// nodeStartByte is the offset NodesInFile pages on. A node with no byte range
// is legal (schema.sql:178) and storage sorts it at offset zero, so the keyset
// this walk carries has to agree with that or the walk would loop on it.
func nodeStartByte(n sqlite.StoredNode) int64 {
	if n.Bytes == nil {
		return 0
	}
	return int64(n.Bytes.Start)
}

// matchesKinds applies a kind filter to a node the storage layer could not
// filter itself (NodesInFile takes no kinds).
func matchesKinds(kind model.NodeKind, kinds []model.NodeKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if kind == k {
			return true
		}
	}
	return false
}

// tierSpan is the inclusive range of exact-tier ranks one walk covers.
type tierSpan struct{ from, to int }

// allExactTiers covers every exact tier.
var allExactTiers = tierSpan{from: model.TierExactPath.Rank(), to: model.TierExactName.Rank()}

// covers reports whether the span walks tier.
func (s tierSpan) covers(tier model.SearchTier) bool {
	return tier.Rank() >= s.from && tier.Rank() <= s.to
}

// exactCandidates runs the non-lexical tiers in span for the rule's query and
// emits their candidates in tier order, each already carrying its path. A node
// is emitted once, at its tier (tierRule): the earliest exact tier that serves
// it. A tier outside span is not walked, but the rule is decided whatever the
// span, so a continuation that resumes at a later tier drops exactly the
// candidates the first page's walk dropped.
//
// page is the READ size for one keyset step, not a bound on the answer: every
// tier is walked to the end of its keyset and nothing is dropped.
//
// Nodes picks the precedence-winning row PER TIER's WHERE clause, so a node
// whose units publish different qualified names can present one row at
// exact_qualified_name and another at exact_name; the candidate in hand is one
// row and cannot speak for the node's others. tierRule asks the earlier tiers
// for each read page of nodes together, which is why no set of emitted ids is
// held: the walk's heap is one read page, plus, when the query names a file,
// the ids of that one file's declarations (tierRule.inPathFile).
//
// Kinds are a stored column the node filter applies. The request's path and
// language filters decide the tier rule here; the caller applies them to the
// candidates it serves (servableExact), as it does to the lexical tier.
func exactCandidates(ctx context.Context, r exactReader, rule *tierRule, page int, span tierSpan,
	emit func(exactHit) error) error {
	if span.covers(model.TierExactPath) {
		if err := pathCandidates(ctx, r, rule.normalized, rule.pathFile, rule.kinds, page, emit); err != nil {
			return err
		}
	}
	ids := make([]model.NodeID, 0, min(page, model.MaxPageItems))
	for _, tier := range exactTiers {
		if !span.covers(tier) {
			continue
		}
		// One row per node: where several units publish the same node fact,
		// Nodes applies the Section 9.4 read-time precedence order (verified
		// source binding, then provider id, then unit key) inside the query
		// itself. Nothing in this package re-ranks or re-folds provider ids.
		nodeFilter := nodeFilterFor(tier, rule.query, rule.kinds)
		after := model.NodeID("")
		// The tier is walked on its keyset, not read once. Nodes applies its
		// SQL LIMIT to node_facts rows and only then collapses the duplicate
		// node_id rows the Section 9.4 precedence order leaves behind, so a
		// full-bound read routinely returns fewer nodes than it asked for while
		// the keyset still has rows: a deduped count can neither end the walk
		// nor decide whether the tier had more. Only an EMPTY page ends it, and
		// `after` advances strictly on every non-empty one, so it terminates.
		for {
			nodes, err := r.Nodes(ctx, nodeFilter, after, page)
			if err != nil {
				return err
			}
			if len(nodes) == 0 {
				break
			}
			after = nodes[len(nodes)-1].Node.ID
			ids = ids[:0]
			for _, n := range nodes {
				ids = append(ids, n.Node.ID)
			}
			owned, err := rule.ownedBefore(ctx, tier, ids)
			if err != nil {
				return err
			}
			for _, n := range nodes {
				if owned[n.Node.ID] {
					continue
				}
				p, err := rule.paths.path(ctx, n.Node.FileID)
				if err != nil {
					return err
				}
				if err := emit(exactHit{Tier: tier, Path: p, Node: n}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// tierRule decides the one exact tier that owns a node for one request: the
// earliest exact tier that SERVES the node, that is, whose row for it passes the
// request's own test (servesRow). exact_path serves a node with a visible row
// declared in the file the query names, when that file's path passes the
// filters; exact_qualified_name, qualified_name_prefix and exact_name serve a
// node when the row the tier lists for it (the precedence winner among the
// node's rows matching the tier, which is the row that tier's walk serves)
// passes the test. The kind filter is a per-node column, so it admits or
// refuses all of a node's rows together.
//
// So a node with a qualified-name row under a filtered-out path and a name row
// inside the filter is served at exact_name: a tier that cannot serve the node
// does not own it.
//
// The rule is a function of the pinned generation and the request alone, never
// of the page: every page of one answer, and the lexical fold beside them,
// assigns a node the same tier, so a node is served once across all of an
// answer's pages without any memory between them. One rule serves a whole
// request, so the file the query names is read once per request.
//
// The tiers nest: a qualified name equal to the query is also prefixed by it,
// so a node with no prefix-tier row has no exact-qualified-name row either, and
// the exact qualified-name tier is asked only about nodes the prefix tier
// lists.
type tierRule struct {
	r          exactReader
	query      string
	kinds      []model.NodeKind
	normalized string
	pathFile   model.FileID
	// pathKept says the exact_path tier can serve anything: the query names a
	// file and that file's path passes the request's filters.
	pathKept bool
	// servesRow is the request's test of one tier row: whether the tier,
	// listing the node with that row, serves it.
	servesRow func(ctx context.Context, row sqlite.StoredNode) (bool, error)
	paths     *pathResolver
	// inFile holds the nodes with a visible row in pathFile, read on first
	// use; nil until then.
	inFile map[model.NodeID]struct{}
}

// newSearchRule builds the rule of one search request: the query read as a
// path (queryFile) and as a symbol, under the request's kinds and filters. A
// tier serves a row that has a file whose path the filters keep, the test the
// exact phase applies to the same row (servableExact).
func newSearchRule(ctx context.Context, r exactReader, query string, kinds []model.NodeKind,
	filter hitFilter) (*tierRule, error) {
	normalized, pathFile, err := queryFile(ctx, r, query)
	if err != nil {
		return nil, err
	}
	t := &tierRule{r: r, query: query, kinds: kinds, normalized: normalized, pathFile: pathFile,
		paths: newPathResolver(r, model.MaxPageItems)}
	t.pathKept = pathFile != "" && normalized != "" && filter.keep(normalized)
	t.servesRow = func(ctx context.Context, row sqlite.StoredNode) (bool, error) {
		if row.Node.FileID == "" {
			return false, nil
		}
		p, err := t.paths.path(ctx, row.Node.FileID)
		if err != nil {
			return false, err
		}
		return p != "" && filter.keep(p), nil
	}
	return t, nil
}

// newSymbolRule builds the rule of one symbol request: the symbol tiers alone,
// with no kind or path filter. Every row serves, except that a definition
// serves only declaration sites (isDeclaration).
func newSymbolRule(r exactReader, query string, declarationsOnly bool) *tierRule {
	return &tierRule{r: r, query: query, paths: newPathResolver(r, model.MaxPageItems),
		servesRow: func(_ context.Context, row sqlite.StoredNode) (bool, error) {
			return !declarationsOnly || isDeclaration(row), nil
		}}
}

// ownedBefore answers which of ids an exact tier ranked before tier serves:
// those the walk of tier must not emit. TierLexical asks every exact tier, which
// is the lexical fold's question. ids are distinct; each symbol tier is asked
// about them in one statement per model.MaxPageItems ids (NodesAmong), so a
// page of candidates costs at most three statements and never one per node.
func (t *tierRule) ownedBefore(ctx context.Context, tier model.SearchTier,
	ids []model.NodeID) (map[model.NodeID]bool, error) {
	owned := map[model.NodeID]bool{}
	if tier == model.TierExactPath || len(ids) == 0 {
		return owned, nil
	}
	rest := ids
	if t.pathKept {
		if err := t.readPathFile(ctx); err != nil {
			return nil, err
		}
		rest = make([]model.NodeID, 0, len(ids))
		for _, id := range ids {
			if _, in := t.inFile[id]; in {
				owned[id] = true
			} else {
				rest = append(rest, id)
			}
		}
	}
	for from := 0; from < len(rest); from += model.MaxPageItems {
		if err := t.ownChunk(ctx, tier, rest[from:min(from+model.MaxPageItems, len(rest))], owned); err != nil {
			return nil, err
		}
	}
	return owned, nil
}

// ownChunk marks in owned the ids of one chunk that a symbol tier ranked before
// tier serves.
func (t *tierRule) ownChunk(ctx context.Context, tier model.SearchTier, ids []model.NodeID,
	owned map[model.NodeID]bool) error {
	switch rank := tier.Rank(); {
	case rank <= model.TierExactQualifiedName.Rank():
		return nil
	case rank == model.TierQualifiedNamePrefix.Rank():
		// Every candidate of the prefix tier is prefix-listed, so the exact
		// qualified-name tier is asked about all of them.
		return t.markServed(ctx, model.TierExactQualifiedName, ids, owned)
	}
	prefixed, err := t.r.NodesAmong(ctx, nodeFilterFor(model.TierQualifiedNamePrefix, t.query, t.kinds), ids)
	if err != nil {
		return err
	}
	if len(prefixed) > 0 {
		listed := make([]model.NodeID, len(prefixed))
		for i, row := range prefixed {
			listed[i] = row.Node.ID
		}
		if err := t.markServed(ctx, model.TierExactQualifiedName, listed, owned); err != nil {
			return err
		}
		if err := t.markRows(ctx, prefixed, owned); err != nil {
			return err
		}
	}
	if tier.Rank() <= model.TierExactName.Rank() {
		return nil
	}
	rest := make([]model.NodeID, 0, len(ids))
	for _, id := range ids {
		if !owned[id] {
			rest = append(rest, id)
		}
	}
	return t.markServed(ctx, model.TierExactName, rest, owned)
}

// markServed marks in owned the ids that tier lists with a row that serves.
func (t *tierRule) markServed(ctx context.Context, tier model.SearchTier, ids []model.NodeID,
	owned map[model.NodeID]bool) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := t.r.NodesAmong(ctx, nodeFilterFor(tier, t.query, t.kinds), ids)
	if err != nil {
		return err
	}
	return t.markRows(ctx, rows, owned)
}

// markRows marks in owned the node of every row that serves.
func (t *tierRule) markRows(ctx context.Context, rows []sqlite.StoredNode, owned map[model.NodeID]bool) error {
	for _, row := range rows {
		if owned[row.Node.ID] {
			continue
		}
		ok, err := t.servesRow(ctx, row)
		if err != nil {
			return err
		}
		if ok {
			owned[row.Node.ID] = true
		}
	}
	return nil
}

// readPathFile reads, on first use, the nodes with a visible row declared in
// the file the query names. DistinctNodesInFile lists every node with a row in
// that file, once, so the file's node ids are held for the rest of the
// request: one file's declarations, never the answer's.
func (t *tierRule) readPathFile(ctx context.Context) error {
	if t.inFile != nil {
		return nil
	}
	inFile := map[model.NodeID]struct{}{}
	var afterStart int64
	var after model.NodeID
	for {
		// The file is read whole, so it is read at the storage page bound.
		nodes, err := t.r.DistinctNodesInFile(ctx, t.pathFile, afterStart, after, model.MaxPageItems)
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			break
		}
		for _, n := range nodes {
			afterStart, after = nodeStartByte(n), n.Node.ID
			if matchesKinds(n.Node.Kind, t.kinds) {
				inFile[n.Node.ID] = struct{}{}
			}
		}
	}
	t.inFile = inFile
	return nil
}

// tierRow answers the row a symbol tier serves for node id, after the rule:
// ok is false when the tier does not list the node or an earlier tier owns it.
// It is one indexed lookup, the row the tier's walk would emit for the node.
func (t *tierRule) tierRow(ctx context.Context, tier model.SearchTier, id model.NodeID) (exactHit, bool, error) {
	rows, err := t.r.NodesAmong(ctx, nodeFilterFor(tier, t.query, t.kinds), []model.NodeID{id})
	if err != nil || len(rows) == 0 {
		return exactHit{}, false, err
	}
	owned, err := t.ownedBefore(ctx, tier, []model.NodeID{id})
	if err != nil || owned[id] {
		return exactHit{}, false, err
	}
	p, err := t.paths.path(ctx, rows[0].Node.FileID)
	if err != nil {
		return exactHit{}, false, err
	}
	return exactHit{Tier: tier, Path: p, Node: rows[0]}, true, nil
}
