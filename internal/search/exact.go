package search

// L2 owns this file: the exact and prefix retrieval tiers (exact_path, exact_qualified_name, qualified_name_prefix, exact_name) that serve resolve, document-symbols, workspace-symbols and definition.

import (
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
		return n.Node.QualifiedName == query || strings.HasPrefix(n.Node.QualifiedName, query)
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

// pathResolver memoizes file paths for one request: tiers 1-3 return nodes that
// carry a FileID but no path, and one page can hold many nodes from one file.
type pathResolver struct {
	r     exactReader
	paths map[model.FileID]string
}

func newPathResolver(r exactReader) *pathResolver {
	return &pathResolver{r: r, paths: map[model.FileID]string{}}
}

// path returns the normalized path of file. A node with no file (a manifest
// dependency, say) has no path and is not an error.
func (p *pathResolver) path(ctx context.Context, file model.FileID) (string, error) {
	if file == "" {
		return "", nil
	}
	if known, ok := p.paths[file]; ok {
		return known, nil
	}
	fv, err := p.r.File(ctx, file)
	if err != nil {
		return "", err
	}
	p.paths[file] = fv.Path
	return fv.Path, nil
}

// pathCandidates is tier 0: the query read as a root-relative path, resolved to
// a file, then that file's visible nodes in document order. A query that is not
// a path, or names no visible file, yields no candidates rather than an error —
// the symbol tiers answer the same query. The second return reports that the
// file had more KEPT candidates than limit.
//
// It pages NodesInFile on its (start_byte, node_id) keyset rather than issuing
// one bounded read, because storage takes no kind filter (search.go:109) and so
// applies the bound BEFORE the filter: a single read of limit nodes would
// answer "0 hits, not truncated" for a file whose matching declarations all sit
// past node limit. It reads one candidate past the bound to tell a full answer
// from a truncated one without claiming truncation it has not seen, and returns
// at most limit.
func pathCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind, limit int) ([]exactHit, bool, error) {
	normalized, ok := normalizeQueryPath(query)
	if !ok {
		return nil, false, nil
	}
	file, err := r.FileByPath(ctx, normalized)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
			return nil, false, nil
		}
		return nil, false, err
	}
	out := make([]exactHit, 0, limit)
	var afterStart int64
	var after model.NodeID
	for len(out) <= limit {
		// The page size is the caller's bound, which storage clamps to at most
		// model.MaxPageItems (query.go:283); asking for more would be a bound
		// this code believes and the database does not, and the loop would
		// then end on the first page.
		nodes, err := r.NodesInFile(ctx, file, afterStart, after, limit)
		if err != nil {
			return nil, false, err
		}
		if len(nodes) == 0 {
			break
		}
		for _, n := range nodes {
			afterStart, after = nodeStartByte(n), n.Node.ID
			if !matchesKinds(n.Node.Kind, kinds) {
				continue
			}
			out = append(out, exactHit{Tier: model.TierExactPath, Path: normalized, Node: n})
			if len(out) > limit {
				break
			}
		}
		if len(nodes) < limit {
			// A short page is the end of the file's keyset, so nothing was
			// dropped and the answer for this tier is complete.
			break
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
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
// returns their candidates in tier order, each already carrying its path. A
// node that matches more than one tier appears once, at its most specific tier.
// limit bounds each tier, so the result holds at most 4*limit candidates; the
// caller's heap and dedup own the global bound. The second return reports that
// some tier filled that bound, which the caller turns into QueryMeta.Truncated.
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
func exactCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind, limit int) ([]exactHit, bool, error) {
	paths := newPathResolver(r)
	out, full, err := pathCandidates(ctx, r, query, kinds, limit)
	if err != nil {
		return nil, false, err
	}
	seen := make(map[model.NodeID]struct{}, len(out))
	for _, hit := range out {
		seen[hit.Node.Node.ID] = struct{}{}
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
		nodes, err := r.Nodes(ctx, nodeFilterFor(tier, query, kinds), "", limit)
		if err != nil {
			return nil, false, err
		}
		// A tier that filled its bound had more to give. Section 14.3 forbids
		// dropping those silently, so the caller reports the answer as
		// truncated instead of serving a confidently short page.
		full = full || len(nodes) >= limit
		for _, n := range nodes {
			if _, duplicate := seen[n.Node.ID]; duplicate || supersededByLowerTier(tier, n, query) {
				continue
			}
			p, err := paths.path(ctx, n.Node.FileID)
			if err != nil {
				return nil, false, err
			}
			seen[n.Node.ID] = struct{}{}
			out = append(out, exactHit{Tier: tier, Path: p, Node: n})
		}
	}
	return out, full, nil
}
