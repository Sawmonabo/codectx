package search

// The exact and prefix retrieval tiers (exact_path, exact_qualified_name,
// qualified_name_prefix, exact_name) that serve resolve, document-symbols,
// workspace-symbols and definition.

import (
	"cmp"
	"container/list"
	"context"
	"errors"
	"path"
	"slices"
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

// supersededByLowerTier reports whether a candidate found at tier also matches
// a more specific tier for the same query, in which case that tier's page owns
// it and this one must not emit it again. The test is on the candidate alone,
// so it holds page by page without carrying state across cursors, and it uses
// byte comparison — the same basis as the SQL predicates it mirrors.
func supersededByLowerTier(tier model.SearchTier, n sqlite.StoredNode, query string) bool {
	switch tier {
	case model.TierQualifiedNamePrefix:
		return n.Node.QualifiedName == query
	case model.TierExactName:
		// HasPrefix alone: an exact equality is a prefix of itself, so a
		// separate equality clause stated the same rule twice.
		return strings.HasPrefix(n.Node.QualifiedName, query)
	}
	return false
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

// exactCandidates runs the non-lexical tiers in span for one search query and
// emits their candidates in tier order, each already carrying its path. A node
// is emitted once, at its tier (tierRule): the earliest exact tier any of its
// visible rows matches. A tier outside span is not walked, but the rule is
// decided whatever the span, so a continuation that resumes at a later tier
// drops exactly the candidates the first page's walk dropped.
//
// page is the READ size for one keyset step, not a bound on the answer: every
// tier is walked to the end of its keyset and nothing is dropped.
//
// Nodes picks the precedence-winning row PER TIER's WHERE clause, so a node
// whose units publish different qualified names can present one row at
// exact_qualified_name and another at exact_name; the candidate in hand is one
// row and cannot speak for the node's others. tierRule asks the earlier tiers
// for the node itself, which is why no set of emitted ids is held: the walk's
// heap is one read page per tier, plus, when the query names a file, the ids of
// that one file's declarations (tierRule.inPathFile).
//
// It takes the query and the kind filter, not the whole SearchRequest: kinds are
// a stored column the node filter applies, while SearchRequest.Languages and
// .Paths are applied by the caller to every tier, the lexical one included.
func exactCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind,
	page int, span tierSpan, emit func(exactHit) error) error {
	normalized, pathFile, err := queryFile(ctx, r, query)
	if err != nil {
		return err
	}
	if span.covers(model.TierExactPath) {
		if err := pathCandidates(ctx, r, normalized, pathFile, kinds, page, emit); err != nil {
			return err
		}
	}
	paths := newPathResolver(r, page)
	rule := newTierRule(r, query, kinds, pathFile, page)
	for _, tier := range exactTiers {
		if !span.covers(tier) {
			continue
		}
		// One row per node: where several units publish the same node fact,
		// Nodes applies the Section 9.4 read-time precedence order (verified
		// source binding, then provider id, then unit key) inside the query
		// itself. Nothing in this package re-ranks or re-folds provider ids.
		filter := nodeFilterFor(tier, query, kinds)
		after := model.NodeID("")
		// The tier is walked on its keyset, not read once. Nodes applies its
		// SQL LIMIT to node_facts rows and only then collapses the duplicate
		// node_id rows the Section 9.4 precedence order leaves behind, so a
		// full-bound read routinely returns fewer nodes than it asked for while
		// the keyset still has rows: a deduped count can neither end the walk
		// nor decide whether the tier had more. Only an EMPTY page ends it, and
		// `after` advances strictly on every non-empty one, so it terminates.
		for {
			nodes, err := r.Nodes(ctx, filter, after, page)
			if err != nil {
				return err
			}
			if len(nodes) == 0 {
				break
			}
			for _, n := range nodes {
				after = n.Node.ID
				earlier, err := rule.earlier(ctx, tier, n)
				if err != nil {
					return err
				}
				if earlier {
					continue
				}
				p, err := paths.path(ctx, n.Node.FileID)
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

// tierRule decides the one exact tier that owns a node for one query: the
// earliest tier ANY of the node's visible rows matches. exact_path owns every
// node with a row declared in the file the query names; exact_qualified_name
// every node with a row whose qualified name is the query; qualified_name_prefix
// every node with a row whose qualified name the query prefixes; exact_name
// every node with a row whose name is the query. The kind filter is a per-node
// column, so it admits or refuses all of a node's rows together.
//
// The rule is a function of the pinned generation and the query alone, never of
// the page: every page of one answer, and the lexical fold beside them, assigns
// a node the same tier, so a node is served once across all of an answer's
// pages without any memory between them.
//
// Each symbol tier is asked through its own keyset (tierRows). The tiers nest:
// a qualified name equal to the query is also prefixed by it, so a node with no
// prefix-tier row has no exact-qualified-name row either, and one read of the
// prefix tier answers for both earlier tiers of an exact_name candidate.
type tierRule struct {
	r        exactReader
	query    string
	kinds    []model.NodeKind
	pathFile model.FileID
	// inFile holds the nodes with a visible row in pathFile, read on first
	// use; nil until then.
	inFile    map[model.NodeID]struct{}
	qualified *tierRows
	prefixed  *tierRows
	named     *tierRows
}

// newTierRule builds the rule for one query. size is the read size of each
// tier's keyset seek: the exact walk passes its read page, so consecutive
// candidates share one read; the lexical fold passes 1, since its nodes are
// sparse in a tier and each needs only whether the tier holds it.
func newTierRule(r exactReader, query string, kinds []model.NodeKind, pathFile model.FileID, size int) *tierRule {
	rows := func(tier model.SearchTier) *tierRows {
		return &tierRows{r: r, filter: nodeFilterFor(tier, query, kinds), size: max(size, 1)}
	}
	return &tierRule{r: r, query: query, kinds: kinds, pathFile: pathFile,
		qualified: rows(model.TierExactQualifiedName), prefixed: rows(model.TierQualifiedNamePrefix),
		named: rows(model.TierExactName)}
}

// earlier reports whether a tier ranked before tier owns the candidate n, in
// which case that tier serves it and tier must not. The row in hand is tried
// first: when it already matches an earlier tier, no read is needed.
func (t *tierRule) earlier(ctx context.Context, tier model.SearchTier, n sqlite.StoredNode) (bool, error) {
	if tier == model.TierExactPath {
		return false, nil
	}
	if t.pathFile != "" {
		if n.Node.FileID == t.pathFile {
			return true, nil
		}
		in, err := t.inPathFile(ctx, n.Node.ID)
		if err != nil || in {
			return in, err
		}
	}
	id := n.Node.ID
	switch tier {
	case model.TierQualifiedNamePrefix:
		if n.Node.QualifiedName == t.query {
			return true, nil
		}
		_, ok, err := t.qualified.row(ctx, id)
		return ok, err
	case model.TierExactName:
		if strings.HasPrefix(n.Node.QualifiedName, t.query) {
			return true, nil
		}
		_, ok, err := t.prefixed.row(ctx, id)
		return ok, err
	}
	return false, nil
}

// owner answers the tier that owns node id and the row that tier serves for it,
// or false when no exact tier matches the node. The exact_path tier's row is not
// read: every row it serves lies in pathFile, whose path the caller holds.
func (t *tierRule) owner(ctx context.Context, id model.NodeID) (model.SearchTier, sqlite.StoredNode, bool, error) {
	var none sqlite.StoredNode
	if t.pathFile != "" {
		in, err := t.inPathFile(ctx, id)
		if err != nil {
			return "", none, false, err
		}
		if in {
			return model.TierExactPath, sqlite.StoredNode{Node: model.Node{ID: id, FileID: t.pathFile}}, true, nil
		}
	}
	prefixRow, ok, err := t.prefixed.row(ctx, id)
	if err != nil {
		return "", none, false, err
	}
	if ok {
		row, exact, err := t.qualified.row(ctx, id)
		if err != nil {
			return "", none, false, err
		}
		if exact {
			return model.TierExactQualifiedName, row, true, nil
		}
		return model.TierQualifiedNamePrefix, prefixRow, true, nil
	}
	row, ok, err := t.named.row(ctx, id)
	if err != nil || !ok {
		return "", none, false, err
	}
	return model.TierExactName, row, true, nil
}

// inPathFile reports whether any visible row of node id is declared in the file
// the query names. DistinctNodesInFile lists every node with a row in that
// file, once, so the file's node ids are read on first use and held for the
// rest of the request: one file's declarations, never the answer's.
func (t *tierRule) inPathFile(ctx context.Context, id model.NodeID) (bool, error) {
	if t.inFile == nil {
		t.inFile = map[model.NodeID]struct{}{}
		var afterStart int64
		var after model.NodeID
		for {
			// The file is read whole, so it is read at the storage page bound
			// whatever size the tier probes use.
			nodes, err := t.r.DistinctNodesInFile(ctx, t.pathFile, afterStart, after, model.MaxPageItems)
			if err != nil {
				t.inFile = nil
				return false, err
			}
			if len(nodes) == 0 {
				break
			}
			for _, n := range nodes {
				afterStart, after = nodeStartByte(n), n.Node.ID
				if matchesKinds(n.Node.Kind, t.kinds) {
					t.inFile[n.Node.ID] = struct{}{}
				}
			}
		}
	}
	_, ok := t.inFile[id]
	return ok, nil
}

// tierRows answers, for one symbol tier, the row the tier serves for a node,
// by seeking the tier's node_id keyset at that node. Nodes filters rows before
// it collapses them, so the tier lists a node exactly when some visible row of
// it matches the tier, and the row it lists is the precedence winner among
// those rows: the row the tier's own walk serves.
//
// The last read is kept: it holds every node of the tier in (from, last row],
// or every node past from when it came back empty, so the next node asked for
// in that range costs no read. Nodes asked for in ascending order therefore
// cost at most one read per `size` nodes of the tier, or one read each.
type tierRows struct {
	r      exactReader
	filter sqlite.NodeFilter
	size   int
	read   bool
	// from is the last read's exclusive start; fromStart says it began at the
	// start of the keyset.
	from      model.NodeID
	fromStart bool
	rows      []sqlite.StoredNode
}

// row answers the tier's row for node id, and whether the tier lists it.
func (t *tierRows) row(ctx context.Context, id model.NodeID) (sqlite.StoredNode, bool, error) {
	if !t.covers(id) {
		after, ok := predecessor(id)
		for {
			rows, err := t.r.Nodes(ctx, t.filter, after, t.size)
			if err != nil {
				t.read = false
				return sqlite.StoredNode{}, false, err
			}
			t.read, t.from, t.fromStart, t.rows = true, after, !ok, rows
			if t.covers(id) {
				break
			}
			// Only a read that began below id and ended below it lands here
			// (a predecessor-less id reads from the start of the keyset), so
			// the walk continues after its last node and terminates.
			after, ok = rows[len(rows)-1].Node.ID, true
		}
	}
	i, found := slices.BinarySearchFunc(t.rows, id, func(n sqlite.StoredNode, id model.NodeID) int {
		return cmp.Compare(n.Node.ID, id)
	})
	if !found {
		return sqlite.StoredNode{}, false, nil
	}
	return t.rows[i], true, nil
}

// covers reports whether the last read decides whether the tier lists id.
func (t *tierRows) covers(id model.NodeID) bool {
	if !t.read || (!t.fromStart && id <= t.from) {
		return false
	}
	return len(t.rows) == 0 || id <= t.rows[len(t.rows)-1].Node.ID
}

// predecessor is the node id immediately below id in the keyset order, so a
// read whose `after` is exclusive starts AT id. Node ids are fixed-width
// lowercase hex, compared by storage as the bytes they encode, so the
// predecessor is the hex decrement. The least id has none, and an id that is
// not hex has no decrement; both answer false and are read from the start of
// the keyset.
func predecessor(id model.NodeID) (model.NodeID, bool) {
	b := []byte(id)
	for i := len(b) - 1; i >= 0; i-- {
		switch c := b[i]; {
		case c == '0':
			b[i] = 'f'
		case c == 'a':
			b[i] = '9'
			return model.NodeID(b), true
		case (c > '0' && c <= '9') || (c > 'a' && c <= 'f'):
			b[i] = c - 1
			return model.NodeID(b), true
		default:
			return "", false
		}
	}
	return "", false
}
