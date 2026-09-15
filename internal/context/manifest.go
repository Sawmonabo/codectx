// This file is owned by Task 15 lane L5. It holds
// Section 15.1 manifest identity, canonical projection and persistence through Store.PutManifest, in Section 15.3 tie-break order.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// hashInt is the canonical decimal spelling of an integer inside a hash
// preimage. Every component is length-framed by model.Hasher, so the decimal
// form is unambiguous and no separator is needed around it.
func hashInt(n int64) string { return strconv.FormatInt(n, 10) }

// requestHash is the Section 15.1 semantic identity of a compile request: the
// task, its seeds in sorted order, the phase and the five budget fields. The
// seed count is hashed alongside the joined list so a seed that itself contains
// the field separator cannot forge a different split.
//
// The budget is hashed exactly as requested, zeros included: a zero field means
// "use the configured default", and the configured defaults are already part of
// identity through Config.ContextPolicyHash.
func requestHash(req model.ContextRequest) string {
	seeds := append([]string(nil), req.Seeds...)
	sort.Strings(seeds)
	return model.H(requestHashDomain,
		req.Task,
		hashInt(int64(len(seeds))),
		strings.Join(seeds, hashFieldSep),
		string(req.Phase),
		hashInt(req.Budget.MaxEstimatedTokens),
		hashInt(req.Budget.MaxBytes),
		hashInt(int64(req.Budget.MaxFiles)),
		hashInt(int64(req.Budget.MaxSlices)),
		// max_manifest_bytes is a caller budget that decides whether a plan is
		// admitted at all, so two requests differing only in it are two
		// different requests and must not share a manifest id.
		hashInt(req.Budget.MaxManifestBytes),
	)
}

// manifestIdentity computes a request's manifest id BEFORE it is compiled,
// which is what makes "repeated compile requests reuse the immutable manifest"
// a Store.Manifest lookup rather than a recompile. It returns the request hash
// too, because the manifest header stores it and the canonical projection
// hashes it.
//
// The preimage is fixed arity and its component order is frozen HERE and
// nowhere else: analysis key, snapshot, request hash, policy version, context
// policy hash. A second spelling anywhere would make the reuse lookup silently
// never hit, which looks like a slow compiler rather than a bug.
func manifestIdentity(b model.Binding, req model.ContextRequest, cfg config.Config) (model.ManifestID, string) {
	rh := requestHash(req)
	return model.ManifestID(model.H(manifestIDDomain,
		string(b.AnalysisKey), string(b.SnapshotID), rh, compilerPolicyVersion, cfg.ContextPolicyHash())), rh
}

// canonicalManifestHash hashes the canonical projection of a compiled plan: the
// snapshot and analysis key it is bound to, the request and policy it came
// from, the budget it was packed against, whether its scope is complete, and
// every entry, slice and exclusion in stored order.
//
// It deliberately excludes CreatedAt, the LOCAL generation id, session and run
// ids, cursor tokens and timings, so compiling the same request again after the
// store is closed and reopened reproduces this digest exactly. A differing hash
// under the same manifest id is the determinism alarm PutManifest raises as
// CTX_VERSION_CONFLICT, not an error to work around.
func canonicalManifestHash(b model.Binding, reqHash string, budget model.Budget, scopeComplete bool,
	entries []model.ContextEntry, slices []model.ContextSlice, excluded []model.ExcludedContextEntry) string {
	h := model.NewHasher(canonicalHashDomain)
	h.AddString(string(b.SnapshotID))
	h.AddString(string(b.AnalysisKey))
	h.AddString(reqHash)
	h.AddString(compilerPolicyVersion)
	h.AddString(hashInt(budget.MaxEstimatedTokens))
	h.AddString(hashInt(budget.MaxBytes))
	h.AddString(hashInt(int64(budget.MaxFiles)))
	h.AddString(hashInt(int64(budget.MaxSlices)))
	h.AddString(hashInt(budget.MaxManifestBytes))
	h.AddString(strconv.FormatBool(scopeComplete))

	h.AddString(hashInt(int64(len(entries))))
	for _, e := range entries {
		h.AddString(hashInt(int64(e.Ordinal)))
		h.AddString(string(e.NodeID))
		h.AddString(string(e.FileID))
		h.AddString(string(e.Requirement))
		h.AddString(hashInt(e.ScoreMicros))
		h.AddString(hashInt(e.EstimatedBytes))
		h.AddString(hashInt(e.EstimatedTokens))
		h.AddString(hashInt(int64(len(e.Reasons))))
		for _, r := range e.Reasons {
			h.AddString(r)
		}
		h.AddString(hashInt(int64(len(e.EvidencePaths))))
		for _, p := range e.EvidencePaths {
			h.AddString(hashInt(int64(len(p))))
			for _, id := range p {
				h.AddString(string(id))
			}
		}
	}
	h.AddString(hashInt(int64(len(slices))))
	for _, s := range slices {
		h.AddString(hashInt(int64(s.Index)))
		h.AddString(hashInt(s.EstimatedBytes))
		h.AddString(hashInt(s.EstimatedTokens))
		h.AddString(hashInt(int64(len(s.EntryOrdinals))))
		for _, o := range s.EntryOrdinals {
			h.AddString(hashInt(int64(o)))
		}
	}
	h.AddString(hashInt(int64(len(excluded))))
	for _, x := range excluded {
		h.AddString(hashInt(int64(x.Ordinal)))
		h.AddString(string(x.Reference.NodeID))
		h.AddString(string(x.Reference.FileID))
		h.AddString(x.Reference.Path)
		h.AddString(x.Reason)
	}
	return h.Sum()
}

// reuseManifest is the Section 15.1 immutable-reuse path: a request whose
// identity is already stored returns the stored header instead of recompiling.
//
// Store.Manifest reports an unknown id as CTX_ARGUMENT_INVALID, the same code a
// malformed id returns, and matching the message is forbidden. That ambiguity
// is safe HERE and only here: id comes from manifestIdentity, which is a
// model.H digest and therefore always well formed, so the only way to reach
// that code is a genuine miss. A caller that ever passes an id it did not
// compute must distinguish the two before reusing this helper.
func (c *Compiler) reuseManifest(ctx context.Context, id model.ManifestID) (model.ContextManifest, bool, error) {
	m, err := c.store.Manifest(ctx, id)
	if err == nil {
		return m, true, nil
	}
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
		return model.ContextManifest{}, false, nil
	}
	return model.ContextManifest{}, false, contextErr(ctx, err)
}

// persistManifest writes the compiled plan and returns the immutable header.
//
// p is the budget pass's plan, already in the Section 15.3 tie-break order with
// ordinals 0..n-1 and required_full as a prefix. Ordinals are assigned exactly
// once, where the packing decided them: re-deriving the rows here from the
// packed candidates would be a second ordering that agrees with the first only
// by accident, and the budget would then have been checked against sizes the
// manifest does not carry.
//
// Persisting is the last step of a compile for a reason: a timeout or
// cancellation must return an explicit incomplete answer and leave no manifest
// behind, so nothing here is written incrementally.
func (c *Compiler) persistManifest(ctx context.Context, b model.Binding, req model.ContextRequest,
	budget model.Budget, p planParts, entries []model.ContextEntry,
	exclusions []model.ExcludedContextEntry, completeness []model.CapabilityState,
	scopeComplete bool) (model.ContextManifest, error) {
	slices := p.Slices
	// P-I counts what it emitted independently of what the sink collected. A
	// disagreement means a sink dropped a row, which would persist a manifest
	// whose header counts are right and whose rows are not -- so it fails here,
	// where the defect is, rather than as a puzzling count mismatch in storage.
	if int64(len(entries)) != p.Entries || int64(len(exclusions)) != p.Excluded {
		return model.ContextManifest{}, &model.Error{Code: model.CodeInternal,
			Message: "the compiled plan's emitted rows disagree with the counts the budget pass reported"}
	}
	for _, x := range exclusions {
		// Every omission must be visible. A blank reason would persist as a
		// row that says an entity was dropped and not why, which is exactly
		// the silent omission Section 15.4 forbids; the pass that excluded it
		// is the defect, and inventing a reason here would hide which pass.
		if strings.TrimSpace(x.Reason) == "" {
			return model.ContextManifest{}, &model.Error{Code: model.CodeInternal,
				Message: "an excluded context entry carries no reason"}
		}
	}
	id, reqHash := manifestIdentity(b, req, c.cfg)
	m := model.ContextManifest{
		ID:             id,
		Binding:        b,
		Phase:          req.Phase,
		RequestHash:    reqHash,
		PolicyVersion:  compilerPolicyVersion,
		CanonicalHash:  canonicalManifestHash(b, reqHash, budget, scopeComplete, entries, slices, exclusions),
		Budget:         budget,
		EntryCount:     len(entries),
		SliceCount:     len(slices),
		Completeness:   completeness,
		ScopeComplete:  scopeComplete,
		EstimateMethod: model.EstimateMethodUTF8Bytes,
		CreatedAt:      c.now().UTC(),
	}
	requestJSON, err := json.Marshal(req)
	if err != nil {
		return model.ContextManifest{}, &model.Error{Code: model.CodeInternal,
			Message: "context request is not serializable: " + err.Error()}
	}
	if err := c.store.PutManifest(ctx, m, requestJSON, entries, slices, exclusions); err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	return m, nil
}
