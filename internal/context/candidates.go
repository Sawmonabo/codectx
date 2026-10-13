// This file holds the name-only call candidate tier of a context plan
// (ADR-0015 decision 3): the candidates of every open call site the walk
// reaches join the plan after every precise entry, never as required scope.
package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// admitCandidates finds the open call sites among one walk page's entries and
// admits each site's name-only candidates into the sink as Possible records.
//
// A candidate is never a root and never required: it is pushed to the entity
// fold with no routes, as an optional record carrying its closeness step and
// how many compatible declarations its name has, and the fold keeps a precise
// record of the same entity over it. The fold also counts the candidates that
// survive, for the manifest notice.
//
// When the lookup answers CTX_PROVIDER_UNAVAILABLE the reason is recorded on
// the scope verdict and no further site is asked for the rest of the compile:
// the manifest says candidates are unavailable rather than that there are none.
// Every other failure ends the compile.
//
// only, when set, admits just the candidates declared in those files: the
// closure reads the candidates its sites outside the kept files lend to the
// kept files, and no others (closeKeptFiles).
func (c *Compiler) admitCandidates(ctx context.Context, eng *graph.Engine, gen model.GenerationID,
	entries []model.ImpactEntry, in *seedIngest, only map[model.FileID]bool) error {
	if len(entries) == 0 || in.scope.CandidatesUnavailable != "" {
		return nil
	}
	r, err := eng.Reader()
	if err != nil {
		return contextErr(ctx, err)
	}
	ids := make([]model.NodeID, len(entries))
	for i, e := range entries {
		ids[i] = e.NodeID
	}
	nodes, err := r.NodesByID(ctx, ids)
	if err != nil {
		return contextErr(ctx, err)
	}
	byID := make(map[model.NodeID]model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	// Sites are asked in the page's own order, never the reader's, so the seq
	// every candidate takes is the same on every compile of the plan.
	for _, e := range entries {
		n, ok := byID[e.NodeID]
		if !ok {
			continue
		}
		open, err := namedOpenSite(n)
		if err != nil {
			return err
		}
		if !open {
			continue
		}
		if err := c.admitSiteCandidates(ctx, r, gen, n, in, only); err != nil {
			return err
		}
		if in.scope.CandidatesUnavailable != "" {
			return nil
		}
	}
	return nil
}

// namedOpenSite reports whether n is an open call site that names a callee:
// its metadata says `resolution: unresolved` with a known `reason` other than
// `computed`. A computed site, and an open import site (always computed), has
// no name to look up and so no candidates. Only the two keys are read, so the
// other metadata a node carries never decides it.
func namedOpenSite(n model.Node) (bool, error) {
	if len(n.Metadata) == 0 {
		return false, nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(n.Metadata, &meta); err != nil {
		return false, metadataErr(n.ID, err)
	}
	rawRes, ok := meta[model.MetaResolution]
	if !ok {
		return false, nil
	}
	var res model.NameResolution
	if err := json.Unmarshal(rawRes, &res); err != nil {
		return false, metadataErr(n.ID, err)
	}
	if res != model.ResolutionUnresolved {
		return false, nil
	}
	rawReason, ok := meta[model.MetaReason]
	if !ok {
		// An unresolved call placeholder with no reason is not an open site.
		return false, nil
	}
	var reason model.OpenSiteReason
	if err := json.Unmarshal(rawReason, &reason); err != nil {
		return false, metadataErr(n.ID, err)
	}
	return reason.Valid() && reason != model.ReasonComputed, nil
}

// metadataErr is the typed failure for a sealed node whose metadata does not
// decode: the store validated it at seal time, so this is a defect, not a
// node to skip.
func metadataErr(id model.NodeID, err error) *model.Error {
	return &model.Error{Code: model.CodeInternal,
		Message:     boundReason(fmt.Sprintf("node %s carries metadata that does not decode: %v", id, err)),
		Remediation: "this is a defect in the published facts; report it with the command you ran"}
}

// admitSiteCandidates pages one site's candidates to exhaustion and pushes
// each as a Possible record. A cursor pins the generation, so the generation
// is named on the first page only.
func (c *Compiler) admitSiteCandidates(ctx context.Context, r graph.GraphReader, gen model.GenerationID,
	site model.Node, in *seedIngest, only map[model.FileID]bool) error {
	cursor := ""
	for {
		page, err := graph.Candidates(ctx, r, model.CandidateRequest{
			GenerationID: pinnedGeneration(gen, cursor),
			Site:         site.ID,
			Page:         model.PageRequest{Limit: c.pageLimit(), Cursor: cursor},
		})
		if err != nil {
			var typed *model.Error
			if errors.As(err, &typed) && typed.Code == model.CodeProviderUnavailable {
				why, _ := model.TruncateField(typed.Message, model.MaxReasonBytes)
				if why == "" {
					why = "the candidate lookup is unavailable"
				}
				in.scope.CandidatesUnavailable = why
				return nil
			}
			return contextErr(ctx, err)
		}
		if len(page.Candidates) > 0 {
			if err := in.pushCandidates(ctx, r, site, page, only); err != nil {
				return err
			}
		}
		if page.Cursor == "" {
			return nil
		}
		if page.Cursor == cursor {
			// A cursor that does not advance would page the same candidates
			// forever.
			return &model.Error{Code: model.CodeInternal,
				Message:     boundReason(fmt.Sprintf("the candidate lookup for the open call site %q handed back the cursor it was given", site.Name)),
				Remediation: "this is a defect in the candidate lookup; report it with the command you ran"}
		}
		cursor = page.Cursor
	}
}

// pushCandidates reads one candidate page's nodes in one batch and pushes one
// record per candidate, in the page's ranked order; with only set, just the
// candidates declared in those files.
func (in *seedIngest) pushCandidates(ctx context.Context, r graph.GraphReader, site model.Node,
	page model.CandidatePage, only map[model.FileID]bool) error {
	ids := make([]model.NodeID, len(page.Candidates))
	for i, cd := range page.Candidates {
		ids[i] = cd.NodeID
	}
	nodes, err := r.NodesByID(ctx, ids)
	if err != nil {
		return contextErr(ctx, err)
	}
	byID := make(map[model.NodeID]model.Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, cd := range page.Candidates {
		n, ok := byID[cd.NodeID]
		if !ok {
			// The lookup and the reader are pinned to one generation, so a
			// candidate the reader does not publish is a defect.
			return &model.Error{Code: model.CodeInternal,
				Message: boundReason(fmt.Sprintf("the candidate %s of the open call site %q is not a node of the pinned generation",
					cd.NodeID, site.Name)),
				Remediation: "this is a defect in the candidate lookup; report it with the command you ran"}
		}
		if only != nil && !only[n.FileID] {
			continue
		}
		closeness, err := closenessOrdinal(cd.Closeness)
		if err != nil {
			return err
		}
		cand := candidate{
			NodeID:      n.ID,
			FileID:      n.FileID,
			Requirement: model.RequirementOptional,
			Origin:      originCandidate,
			Reasons: []string{boundReason(fmt.Sprintf(
				"the open call site %q may call this declaration: matched by name only (%s, %s), no analysis bound the call",
				site.Name, cd.Resolution, cd.Closeness))},
			Possible:   true,
			Closeness:  closeness,
			Compatible: page.Total,
		}
		if n.Range != nil {
			cand.StartByte = int64(n.Range.Start.Byte)
		}
		seq := in.seq
		in.seq++
		if err := in.entitySort.Add(candRecOf(cand, seq)); err != nil {
			return err
		}
	}
	return nil
}

// closenessOrdinal is a closeness step's place in the candidate order of
// ADR-0015 decision 3: the site's own file first, anywhere else last.
func closenessOrdinal(c model.CandidateCloseness) (int32, error) {
	switch c {
	case model.ClosenessSameFile:
		return 0, nil
	case model.ClosenessSameDirectory:
		return 1, nil
	case model.ClosenessImported:
		return 2, nil
	case model.ClosenessSameUnit:
		return 3, nil
	case model.ClosenessElsewhere:
		return 4, nil
	}
	return 0, &model.Error{Code: model.CodeInternal,
		Message:     boundReason(fmt.Sprintf("the candidate lookup answered the unknown closeness step %q", c)),
		Remediation: "this is a defect in the candidate lookup; report it with the command you ran"}
}
