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
// the symbol tiers answer the same query.
func pathCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind, limit int) ([]exactHit, error) {
	normalized, ok := normalizeQueryPath(query)
	if !ok {
		return nil, nil
	}
	file, err := r.FileByPath(ctx, normalized)
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
			return nil, nil
		}
		return nil, err
	}
	nodes, err := r.NodesInFile(ctx, file, 0, "", limit)
	if err != nil {
		return nil, err
	}
	out := make([]exactHit, 0, len(nodes))
	for _, n := range nodes {
		if !matchesKinds(n, kinds) {
			continue
		}
		out = append(out, exactHit{Tier: model.TierExactPath, Path: normalized, Node: n})
	}
	return out, nil
}

// matchesKinds applies the request's kind filter to a node the storage layer
// could not filter itself (NodesInFile takes no kinds).
func matchesKinds(n sqlite.StoredNode, kinds []model.NodeKind) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if n.Node.Kind == k {
			return true
		}
	}
	return false
}

// exactCandidates runs all four non-lexical tiers for one search query and
// returns their candidates in tier order, each already carrying its path. A
// node that matches more than one tier appears once, at its most specific tier.
// limit bounds each tier, so the result holds at most 4*limit candidates; the
// caller's heap and dedup own the global bound.
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
func exactCandidates(ctx context.Context, r exactReader, query string, kinds []model.NodeKind, limit int) ([]exactHit, error) {
	paths := newPathResolver(r)
	out, err := pathCandidates(ctx, r, query, kinds, limit)
	if err != nil {
		return nil, err
	}
	seen := make(map[model.NodeID]struct{}, len(out))
	for _, hit := range out {
		seen[hit.Node.Node.ID] = struct{}{}
	}
	for _, tier := range exactTiers {
		nodes, err := r.Nodes(ctx, nodeFilterFor(tier, query, kinds), "", limit)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if _, duplicate := seen[n.Node.ID]; duplicate || supersededByLowerTier(tier, n, query) {
				continue
			}
			p, err := paths.path(ctx, n.Node.FileID)
			if err != nil {
				return nil, err
			}
			seen[n.Node.ID] = struct{}{}
			out = append(out, exactHit{Tier: tier, Path: p, Node: n})
		}
	}
	return out, nil
}
