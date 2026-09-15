// Package reconcile is the deterministic identity resolution of Section 9.4.
// It owns resolution policy: which candidate fields form a canonical key,
// which stored aliases a candidate may match, and how equal matches are
// ordered. Providers never derive a canonical key themselves; they call the
// resolver and copy Resolution.CanonicalKey into the facts they publish.
//
// Every answer is a pure function of the candidate and the set of sealed
// dependency units the resolver was built over. Nothing here consults the
// unit being built, another running unit, wall time, map order or goroutine
// scheduling, so a source-equivalent unit produces identical identities no
// matter in which order its dependencies completed.
package reconcile

import (
	"cmp"
	"context"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// AliasStore is the one read the resolver performs: the distinct identities a
// scoped native key is aliased to by sealed units, ordered by canonical key.
// *sqlite.Store satisfies it.
type AliasStore interface {
	LookupAliases(ctx context.Context, units []model.UnitID, scopeKey, nativeKey string, limit int) ([]sqlite.StoredAlias, error)
}

// Resolver resolves candidates for one unit against that unit's completed
// declared dependencies. It is safe for concurrent use.
type Resolver struct {
	store AliasStore
	repo  model.RepositoryID
	deps  []model.UnitID
}

// New builds a resolver over the sealed units deps. The list is sorted and
// deduplicated so the same dependency set always yields the same resolver
// regardless of the order the coordinator collected it. A candidate resolved
// with no dependencies can only mint or stay unresolved.
func New(store AliasStore, repo model.RepositoryID, deps []model.UnitID) (*Resolver, error) {
	if store == nil {
		return nil, invalid("a resolver needs an alias store")
	}
	if !model.ValidHexID(string(repo)) {
		return nil, invalid("a resolver needs the repository identity")
	}
	sorted := slices.Clone(deps)
	slices.Sort(sorted)
	return &Resolver{store: store, repo: repo, deps: slices.Compact(sorted)}, nil
}

// Resolve applies the Section 9.4 order. An unambiguous or ambiguous alias
// match on the strong key, then on the native key, wins with basis
// native_key; the primary identity is the one with the smallest canonical key
// and the rest form the bounded ambiguous list in the same order, so equal
// candidates are retained rather than chosen by completion order. EVERY
// alternative the lookup returned is retained: a key aliased to many identities
// is a fact about the repository, so it is neither truncated (which would lose
// a may_refer_to alternative) nor refused (which failed the analysis unit for
// having many). Without an alias match the identity is minted by CanonicalKey.
//
// An alias match adopts the stored kind: the identity already exists in a
// completed dependency and a fact under it must carry that kind, which
// storage enforces. Every other attribute of the returned node is the
// candidate's own, because attribute ownership stays with the publishing unit.
func (r *Resolver) Resolve(ctx context.Context, c model.NodeCandidate) (model.Resolution, error) {
	if err := c.Validate(); err != nil {
		return model.Resolution{}, err
	}
	node := model.Node{Kind: c.Kind, Language: c.Language, Name: c.Name, QualifiedName: c.QualifiedName, Signature: c.Signature,
		FileID: c.FileID, ContentHash: c.ContentHash, Range: c.Range}
	for _, key := range []string{c.StrongKey, c.NativeKey} {
		if key == "" || len(r.deps) == 0 {
			continue
		}
		hits, err := r.store.LookupAliases(ctx, r.deps, c.ScopeKey, key, sqlite.MaxAliasLookup)
		if err != nil {
			return model.Resolution{}, err
		}
		if len(hits) == 0 {
			continue
		}
		// The store orders by canonical key; sorting again keeps the contract
		// local to this package rather than trusting the query alone.
		slices.SortFunc(hits, func(a, b sqlite.StoredAlias) int {
			return cmp.Or(cmp.Compare(a.CanonicalKey, b.CanonicalKey), cmp.Compare(a.NodeID, b.NodeID))
		})
		primary := hits[0]
		node.ID, node.Kind = primary.NodeID, primary.Kind
		res := model.Resolution{Node: node, Basis: model.MatchNativeKey, CanonicalKey: primary.CanonicalKey}
		for _, h := range hits[1:] {
			res.Ambiguous = append(res.Ambiguous, h.NodeID)
		}
		return res, res.Validate()
	}
	key, basis := CanonicalKey(c)
	node.ID = model.NewNodeID(r.repo, c.Kind, key)
	res := model.Resolution{Node: node, Basis: basis, CanonicalKey: key}
	return res, res.Validate()
}

// CanonicalKey is the single implementation of minted identity: the
// canonical_entity_key a candidate receives when no completed dependency
// already names it, with the match basis that key represents. It is a pure
// function of the candidate.
//
//   - A located candidate (file and range) keys on the file's path identity,
//     name, kind, language, qualified name and exact byte range:
//     Section 9.4's "exact language/path/range/kind/qualified-name match".
//     Two providers extracting the same declaration therefore mint the same
//     node without either knowing about the other.
//   - A candidate with a qualified name and a defining file but no range keys
//     on scope, qualified name, kind, language, signature and file:
//     "exact package/qualified-name/signature/defining-file match".
//   - A candidate with a qualified name and no file keys on the same tuple
//     without a file: the deterministic structural key of an entity that has
//     no source location, such as a package or an external dependency.
//   - Anything else is a provider-local unresolved entity keyed on scope,
//     native key, provider and kind. It never merges with another provider's
//     entity by short name (Section 9.4 forbids that), and the provider marks
//     the node's metadata resolution=unresolved.
func CanonicalKey(c model.NodeCandidate) (string, model.MatchBasis) {
	switch {
	case c.FileID != "" && c.Range != nil:
		return model.CanonicalNodeKey(string(c.FileID), c.Name, string(c.Kind), c.Language, c.QualifiedName,
			strconv.FormatUint(c.Range.Start.Byte, 10), strconv.FormatUint(c.Range.End.Byte, 10)), model.MatchSourceLocation
	case c.QualifiedName != "" && c.FileID != "":
		return model.CanonicalNodeKey(c.ScopeKey, c.QualifiedName, string(c.Kind), c.Language, c.Signature, string(c.FileID)), model.MatchQualifiedSignature
	case c.QualifiedName != "":
		return model.CanonicalNodeKey(c.ScopeKey, c.QualifiedName, string(c.Kind), c.Language, c.Signature, ""), model.MatchStructuralKey
	default:
		return model.CanonicalNodeKey(c.ScopeKey, c.NativeKey, c.ProviderID, string(c.Kind)), model.MatchUnresolved
	}
}

func invalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}
