package search

// L2 owns this file: the exact and prefix retrieval tiers (exact_path, exact_qualified_name, qualified_name_prefix, exact_name) that serve resolve, document-symbols, workspace-symbols and definition.

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
// exists so the tiers depend on the frozen signatures of digest §2 rather than
// on the concrete reader: every method below is *sqlite.PinnedReader's, byte
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
// its file's normalized path.
//
// It carries the node rather than a search_fts rowid on purpose: an exact-tier
// candidate has no lexical rowid, so SearchDocuments cannot hydrate it ("missing
// rowids are omitted" would drop it silently). Everything model.SearchHit needs
// except Q10's Range is already here — Kind, Name, QualifiedName, Signature,
// FileID and Bytes on the node, Path beside it.
type exactHit struct {
	Tier model.SearchTier
	Path string
	Node sqlite.StoredNode
}

// exactTiers is the digest §4 ranking order of the non-lexical tiers, most to
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

// pathCandidates is tier 0: the query read as a root-relative path, resolved to
// a file, then that file's visible nodes in document order. A query that is not
// a path, or names no visible file, yields no candidates rather than an error —
// the symbol tiers answer the same query.
//
// It returns the file the query resolved to, so the symbol tiers below can
// recognize a candidate this tier already emitted without keeping a set of the
// ids it emitted; the id is empty when the query is not a path or names no
// visible file, which is exactly when this tier emits nothing.
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
// scale posture removes; the ranked set and its disk-backed spool own the global
// bound instead.
func pathCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind,
	page int, emit func(exactHit) error) (model.FileID, error) {
	normalized, ok := normalizeQueryPath(query)
	if !ok {
		return "", nil
	}
	file, err := r.FileByPath(ctx, normalized)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
			return "", nil
		}
		return "", err
	}
	var afterStart int64
	var after model.NodeID
	for {
		// The page size is the caller's read bound, which storage clamps to at
		// most model.MaxPageItems (query.go:291); asking for more would be a
		// bound this code believes and the database does not, and the walk
		// would then end on the first page.
		nodes, err := r.DistinctNodesInFile(ctx, file, afterStart, after, page)
		if err != nil {
			return file, err
		}
		if len(nodes) == 0 {
			// An empty page is the end of the file's keyset. Only an empty one
			// ends the walk: the read can return a short page while the
			// keyset continues, and `after` advances strictly on every
			// non-empty page, so this terminates.
			return file, nil
		}
		for _, n := range nodes {
			afterStart, after = nodeStartByte(n), n.Node.ID
			if !matchesKinds(n.Node.Kind, kinds) {
				continue
			}
			if err := emit(exactHit{Tier: model.TierExactPath, Path: normalized, Node: n}); err != nil {
				return file, err
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
// filter itself (NodesInFile takes no kinds), and to a lexical document's kind,
// which storage cannot filter inside the MATCH either.
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

// exactCandidates runs all four non-lexical tiers for one search query and
// emits their candidates in tier order, each already carrying its path. A node
// that matches more than one tier is emitted once, at its most specific tier.
//
// page is the READ size for one keyset step, not a bound on the answer: every
// tier is walked to the end of its keyset and nothing is dropped, so there is no
// per-tier truncation left to report. The caller's ranked set and its
// disk-backed spool own the global bound.
//
// Peak heap here is ONE PAGE of stored nodes. The cross-tier "most specific
// tier wins" rule is decided by two predicates over the candidate in hand, not
// by a set of the ids already emitted:
//
//   - among the three symbol tiers, supersededByLowerTier decides it from the
//     candidate's own qualified name, because each tier's SQL filter is what
//     the next tier's predicate tests (a node emitted at exact_qualified_name
//     has QualifiedName == query; one emitted at qualified_name_prefix has
//     query as a prefix of it), and within a tier the keyset advances strictly
//     on node_id while Nodes collapses its duplicate rows;
//   - against tier 0, a symbol candidate repeats the path tier exactly when it
//     is declared in the file that tier walked, so comparing its FileID with
//     pathFile replaces the ids of that walk. Kind is ni.kind, a per-node_id
//     column both reads filter on identically (query.go:133), so a candidate
//     this skips is always one pathCandidates already emitted.
//
// One case the predicates do not cover: Nodes picks the
// precedence-winning row PER TIER's WHERE clause, so a node whose units publish
// different qualified names can present one at exact_qualified_name and another
// at exact_name, and the second tier's predicate then does not recognize the
// first. That emits the candidate twice, which is not a defect: the collector
// deduplicates on the node id, keeps the lower tier rank, and Occurrences is 0
// on both sides -- the only visible difference is that the survivor carries the
// reason of both tiers.
//
// Heap is one page: a one-character qualified_name_prefix that range-scans a
// corpus-sized slice of node_ids never holds more than one keyset page of
// candidates at a time.
//
// Every candidate scores 0 (digest §4, Q6): tier rank, not score, separates the
// exact tiers, and a candidate that also matched lexically keeps that score when
// the two are folded together.
//
// It takes the query and the kind filter, not the whole SearchRequest: kinds are
// a stored column the node filter applies, while SearchRequest.Languages and
// .Paths have to be applied the same way across every tier including the lexical
// one, and this package's digest does not say what a Paths entry means (the only
// precedent, model.FileSelection.Paths, is an exact file list). The caller owns
// those two filters.
func exactCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind,
	page int, emit func(exactHit) error) error {
	paths := newPathResolver(r, page)
	pathFile, err := pathCandidates(ctx, r, query, kinds, page, emit)
	if err != nil {
		return err
	}
	for _, tier := range exactTiers {
		// One row per node: where several units publish the same node fact,
		// Nodes applies the Section 9.4 read-time precedence order (verified
		// source binding, then provider id, then unit key) inside the query
		// itself (storage/sqlite/query.go:138-143). Ruling Q1/Q8 puts that
		// mechanism in storage and this note where Nodes is consumed: nothing
		// in this package re-ranks or re-folds provider ids, and a nil-aware
		// clause would be added here only if a leg showed a nil-valued row
		// winning.
		filter := nodeFilterFor(tier, query, kinds)
		after := model.NodeID("")
		// The tier is walked on its keyset, not read once. Nodes applies its
		// SQL LIMIT to node_facts rows and only then collapses the duplicate
		// node_id rows the Section 9.4 precedence order leaves behind
		// (storage/sqlite/query.go:246-263), so a full-bound read routinely
		// returns fewer nodes than it asked for while the keyset still has
		// rows: a deduped count can neither end the walk nor decide whether
		// the tier had more. Only an EMPTY page ends it, and `after` advances
		// strictly on every non-empty one, so it terminates.
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
				// A candidate declared in the file tier 0 walked was already
				// emitted there, at the tier that outranks this one.
				if (pathFile != "" && n.Node.FileID == pathFile) || supersededByLowerTier(tier, n, query) {
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
