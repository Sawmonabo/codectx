package graph

import (
	"context"
	"fmt"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SourceLocator is the OPTIONAL seam a GraphReader may also implement: where
// the nodes of an answer are and where every occurrence of a relation is. It
// is what makes a graph answer self-locating (a call hierarchy's item range
// and its incoming calls' "from ranges"), so a caller needs no follow-up
// symbol or references lookup to learn where anything is.
//
// It is a separate interface rather than a widening of GraphReader so the
// traversal port stays narrow; the engine type-asserts it, and a reader
// without it costs the answer its locations and nothing else.
type SourceLocator interface {
	// NodeLocations reads the stored byte interval, file and path of each
	// node that has one. A node with no stored range is omitted rather than
	// given an invented one. Results need not align with ids.
	NodeLocations(ctx context.Context, ids []model.NodeID) ([]model.NodeLocation, error)
	// RelationOccurrences reads one page of a relation's evidence, keyset
	// on evidence id after `after`, at most limit rows, and returns the
	// located ones: an occurrence with no stored range is not a location and
	// is omitted. next is the key the following page starts after, empty
	// once the relation's evidence is exhausted.
	RelationOccurrences(ctx context.Context, rel model.RelationID, after model.EvidenceID,
		limit int) (page []model.RelationOccurrence, next model.EvidenceID, err error)
}

// occurrenceKind reports whether a graph answer lists the occurrences of a
// relation of kind k: a call's sites are what a call hierarchy names, an
// endpoint's route declaration is where it is mounted, a dependency's lock or
// manifest entry carries the version it resolved to (its evidence detail),
// and a reference carries what its evidence states, such as the visibility a
// declaration is exported with.
func occurrenceKind(k model.RelationKind) bool {
	switch k {
	case model.RelCalls, model.RelRoutesTo, model.RelDependsOn, model.RelReferences:
		return true
	}
	return false
}

// occurrenceBudget is how many occurrences one answer may carry: the wire's
// per-list page width. A traversal page ends before a relation whose
// occurrences would cross it (traverse's visitor), so the only relation that
// can meet it is one whose own sites outnumber a whole answer.
const occurrenceBudget = model.MaxRecordsPerResult

// located is the location half of one graph or path answer.
type located struct {
	locations   []model.NodeLocation
	occurrences []model.RelationOccurrence
	notices     []string
}

// locate reads where the answer's nodes are and every located occurrence of
// its call and route relations, in relation order. A relation whose sites do
// not all fit the answer's occurrence list -- only a relation with more sites
// than one answer carries, since a page ends before any other overflows -- is
// listed up to the list's width, and a notice gives its exact site count and
// where every one is read.
func (e *Engine) locate(ctx context.Context, nodes []model.NodeID, relations []model.Relation) (located, error) {
	var out located
	loc, ok := e.reader.(SourceLocator)
	if !ok {
		return out, nil
	}
	for _, chunk := range impactChunkNodes(dedupeNodes(nodes)) {
		got, err := loc.NodeLocations(ctx, chunk)
		if err != nil {
			return located{}, err
		}
		out.locations = append(out.locations, got...)
	}
	seen := map[model.RelationID]bool{}
	for _, r := range relations {
		if !occurrenceKind(r.Kind) || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		listed, total, err := e.relationSites(ctx, loc, r.ID, occurrenceBudget-len(out.occurrences))
		if err != nil {
			return located{}, err
		}
		out.occurrences = append(out.occurrences, listed...)
		if int64(len(listed)) < total {
			out.notices = append(out.notices, fmt.Sprintf(
				"relation %s has %d located occurrences; %d are listed, which is all one answer carries, "+
					"and `references` on its target pages every one", r.ID, total, len(listed)))
		}
	}
	return out, nil
}

// relationSites reads every located occurrence of rel, keeping at most room of
// them and counting the rest, so a cut is reported with the exact total.
func (e *Engine) relationSites(ctx context.Context, loc SourceLocator, rel model.RelationID,
	room int) ([]model.RelationOccurrence, int64, error) {
	var (
		kept  []model.RelationOccurrence
		total int64
		after model.EvidenceID
	)
	for {
		page, next, err := loc.RelationOccurrences(ctx, rel, after, model.MaxPageItems)
		if err != nil {
			return nil, 0, err
		}
		for _, o := range page {
			total++
			if len(kept) < room {
				kept = append(kept, o)
			}
		}
		if next == "" {
			return kept, total, nil
		}
		after = next
	}
}
