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
// task, its seeds in sorted order, the phase and the four budget fields. The
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

// contextEntry projects one selected candidate into its stored row.
//
// EstimatedBytes is the Section 15.4 worst-case wire size of the selected
// source PLUS the measured canonical-JSON length of the row's own metadata:
// headers, reasons, evidence paths, delimiters and escaping are in the budget,
// so the overhead is measured on the real row rather than assumed. The
// measurement is taken before the two estimate fields are filled, so it is a
// function of the metadata alone and cannot depend on its own magnitude.
func contextEntry(c candidate, ordinal int) (model.ContextEntry, error) {
	e := model.ContextEntry{
		Ordinal:       ordinal,
		NodeID:        c.NodeID,
		FileID:        c.FileID,
		Requirement:   c.Requirement,
		ScoreMicros:   c.ScoreMicros,
		Reasons:       boundedReasons(c.Reasons),
		EvidencePaths: boundedPaths(c.Paths),
	}
	meta, err := serializedBytes(e)
	if err != nil {
		return model.ContextEntry{}, err
	}
	wire, err := wireEncodedBytes(c.SizeBytes)
	if err != nil {
		return model.ContextEntry{}, err
	}
	e.EstimatedBytes = wire + meta
	if e.EstimatedTokens, err = estimateTokens(e.EstimatedBytes); err != nil {
		return model.ContextEntry{}, err
	}
	return e, nil
}

// boundedReasons applies the Section 15.3 explanation caps. Truncation is at a
// byte boundary because the cap is a byte cap; a reason is a bounded English
// sentence, never a value another layer parses.
func boundedReasons(reasons []string) []string {
	if len(reasons) > model.MaxReasonsPerEntry {
		reasons = reasons[:model.MaxReasonsPerEntry]
	}
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		if len(r) > model.MaxReasonBytes {
			r = r[:model.MaxReasonBytes]
		}
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// boundedPaths projects the retained evidence routes into stored relation id
// lists, capped at model.MaxReasonPathsPerEntry routes of MaxRelationsPerPath
// edges. Section 15.3 reports extra routes as a count (candidate.MorePaths)
// rather than growing an exponential path list, so nothing beyond the cap is
// enumerated here.
func boundedPaths(paths []model.RelationPath) [][]model.RelationID {
	if len(paths) > model.MaxReasonPathsPerEntry {
		paths = paths[:model.MaxReasonPathsPerEntry]
	}
	var out [][]model.RelationID
	for _, p := range paths {
		rels := p.Relations
		if len(rels) > model.MaxRelationsPerPath {
			rels = rels[:model.MaxRelationsPerPath]
		}
		if len(rels) == 0 {
			continue
		}
		out = append(out, append([]model.RelationID(nil), rels...))
	}
	return out
}

// manifestRows turns the budget pass's packed plan into the persisted shape.
//
// packed holds one group of candidates per slice, in packing order. Ordinals
// are assigned by ONE total sort over every selected candidate using the
// Section 15.3 tie-break chain, so requirement rank is non-decreasing across
// the whole manifest and required_full therefore occupies a prefix. That is the
// contract `context next` walks: ordinals 0..n-1 are the actor's canonical
// reading order, and slices reference those ordinals rather than duplicating
// the rows.
//
// A candidate that the budget pass packed into two slices (an oversized
// component split at a file boundary) keeps ONE entry, referenced from both:
// duplicating it would make the same bytes count twice against MaxFiles and
// give the actor two ordinals for one file.
func manifestRows(packed [][]candidate) ([]model.ContextEntry, []model.ContextSlice, error) {
	flat := make([]candidate, 0)
	seen := make(map[string]struct{})
	for _, group := range packed {
		for _, c := range group {
			if _, dup := seen[c.entityID()]; dup {
				continue
			}
			seen[c.entityID()] = struct{}{}
			flat = append(flat, c)
		}
	}
	sort.Slice(flat, func(i, j int) bool { return flat[i].less(flat[j]) })

	entries := make([]model.ContextEntry, 0, len(flat))
	ordinal := make(map[string]int, len(flat))
	for i, c := range flat {
		e, err := contextEntry(c, i)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, e)
		ordinal[c.entityID()] = i
	}

	slices := make([]model.ContextSlice, 0, len(packed))
	for _, group := range packed {
		sl := model.ContextSlice{Index: len(slices)}
		for _, c := range group {
			o, ok := ordinal[c.entityID()]
			if !ok {
				// Unreachable: every packed candidate was flattened above.
				// Reported rather than skipped, because a silently dropped
				// entry is exactly the hidden omission Section 15.4 forbids.
				return nil, nil, &model.Error{Code: model.CodeInternal,
					Message: "a packed context candidate has no manifest ordinal"}
			}
			sl.EntryOrdinals = append(sl.EntryOrdinals, o)
			sl.EstimatedBytes += entries[o].EstimatedBytes
			sl.EstimatedTokens += entries[o].EstimatedTokens
		}
		if len(sl.EntryOrdinals) == 0 {
			// An empty slice is not a deliverable unit and model.ContextSlice
			// rejects one; dropping it keeps slice indices dense.
			continue
		}
		sort.Ints(sl.EntryOrdinals)
		slices = append(slices, sl)
	}
	return entries, slices, nil
}

// exclusionRows records every candidate the compiler did not select, each with
// a nonempty reason, so an omission is visible rather than silent (Section
// 15.4). Ordering follows the same total tie-break chain the entries use, so
// two compiles of one request list exclusions identically.
//
// A candidate carrying no reason is a defect in the pass that excluded it: the
// alternative — inventing a reason here — would hide which pass dropped it.
func exclusionRows(excluded []candidate) ([]model.ExcludedContextEntry, error) {
	ordered := append([]candidate(nil), excluded...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].less(ordered[j]) })
	out := make([]model.ExcludedContextEntry, 0, len(ordered))
	for i, c := range ordered {
		if strings.TrimSpace(c.Excluded) == "" {
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "an excluded context candidate carries no reason"}
		}
		reason := c.Excluded
		if len(reason) > model.MaxReasonBytes {
			reason = reason[:model.MaxReasonBytes]
		}
		out = append(out, model.ExcludedContextEntry{
			Ordinal:   i,
			Reference: model.ContextReference{NodeID: c.NodeID, FileID: c.FileID, Path: c.Path},
			Reason:    reason,
		})
	}
	return out, nil
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
// Persisting is the last step of a compile for a reason: a timeout or
// cancellation must return an explicit incomplete answer and leave no manifest
// behind, so nothing here is written incrementally.
func (c *Compiler) persistManifest(ctx context.Context, b model.Binding, req model.ContextRequest,
	budget model.Budget, packed [][]candidate, excluded []candidate,
	completeness []model.CapabilityState, scopeComplete bool) (model.ContextManifest, error) {
	entries, slices, err := manifestRows(packed)
	if err != nil {
		return model.ContextManifest{}, err
	}
	exclusions, err := exclusionRows(excluded)
	if err != nil {
		return model.ContextManifest{}, err
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
