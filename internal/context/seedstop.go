// This file holds the lexical read's stop (ADR-0016 decision 3): each lexical
// match's origin contribution, which file counts as a witness, and the rule
// that ends the read once no unread match can rank above the packed files.
package context

import (
	"context"
	"math/bits"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// lexicalContribution is a lexical match's origin contribution (ADR-0016
// decision 1): its score relative to top, the score of the first
// lexical-tier match of this task's read, scaled to lexicalContributionMax and
// floored. A hit of a non-lexical tier -- the task text as an exact path or
// name -- takes lexicalContributionMax: such hits precede the lexical tier in
// the read, so the read order is non-increasing in the contribution.
//
// A score or top of zero or less contributes nothing, and a score at or above
// top contributes lexicalContributionMax, so the result never exceeds it. The
// product is taken in 128 bits, so no int64 overflow and no float is involved.
func lexicalContribution(score, top int64, tier model.SearchTier) (int64, error) {
	if tier != model.TierLexical {
		return lexicalContributionMax, nil
	}
	if score <= 0 || top <= 0 {
		return 0, nil
	}
	if score >= top {
		return lexicalContributionMax, nil
	}
	hi, lo := bits.Mul64(uint64(lexicalContributionMax), uint64(score))
	// bits.Div64 panics when the quotient does not fit 64 bits, which is
	// exactly hi >= top. score < top bounds the quotient below
	// lexicalContributionMax, so this is the overflow check the arithmetic
	// requires rather than a reachable answer.
	if hi >= uint64(top) {
		return 0, overflowErr(lexicalContributionMax, score)
	}
	q, _ := bits.Div64(hi, lo, uint64(top))
	return int64(q), nil
}

// lexicalStop is the stop's running state over one task's lexical read
// (ADR-0016 decision 3). The read admits a match only after admit has
// decided on it, so the match that stops the read -- which may sit in the next
// step -- is examined and never admitted.
type lexicalStop struct {
	// full disables the stop: every match is read.
	full bool
	// maxBytes and maxTokens are the per-slice budget a witness file's encoded
	// source must fit; needed is how many distinct witness files end the read
	// (F + 1 + the round's extra).
	maxBytes, maxTokens int64
	needed              int
	// top is the first lexical-tier match's score, set when the read meets it.
	top    int64
	topSet bool
	// seen holds the witness files counted so far; floor is the contribution
	// of the match at which the needed-th witness first appeared, valid once
	// floorSet.
	seen     map[model.FileID]bool
	floor    int64
	floorSet bool
	// sizes is the current step's file sizes, by file id.
	sizes map[model.FileID]int64
	stop  seedStop
}

func newLexicalStop(full bool, b resolvedBudget, round roundInputs) *lexicalStop {
	return &lexicalStop{
		full: full, maxBytes: b.MaxBytes, maxTokens: b.MaxTokens,
		needed: b.MaxFiles + 1 + int(round.Extra),
		seen:   map[model.FileID]bool{},
		stop:   seedStop{FileBudget: b.MaxFiles},
	}
}

// loadPage reads the sizes of the files one step of matches names, through
// FilesByID over the step's distinct file ids in batches of its bound
// (model.MaxPageItems), since a step of the in-process read can be deeper than
// one batch. It reads nothing once the floor is known or when the stop is
// disabled, since no further witness is counted then. A file the pinned
// snapshot lacks is absent from the result and is therefore no witness.
func (s *lexicalStop) loadPage(ctx context.Context, reader *sqlite.PinnedReader, hits []model.SearchHit) error {
	s.sizes = nil
	if s.full || s.floorSet {
		return nil
	}
	seen := map[model.FileID]bool{}
	ids := make([]model.FileID, 0, len(hits))
	for _, h := range hits {
		if h.FileID != "" && !seen[h.FileID] && !s.seen[h.FileID] {
			seen[h.FileID] = true
			ids = append(ids, h.FileID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	s.sizes = make(map[model.FileID]int64, len(ids))
	for batch := range slices.Chunk(ids, model.MaxPageItems) {
		files, err := reader.FilesByID(ctx, batch)
		if err != nil {
			return contextErr(ctx, err)
		}
		for _, fv := range files {
			s.sizes[fv.ID] = fv.Size
		}
	}
	return nil
}

// admit decides on the next match of the read. It returns the match's
// contribution and whether it is read; a match that is not read ends the read
// and is recorded as the stop's next match. Once the floor is known, a match
// is read only while lexicalTau of its contribution is at least the floor, so
// matches of equal score are read together whatever the step size.
func (s *lexicalStop) admit(hit model.SearchHit) (int64, bool, error) {
	if hit.Tier == model.TierLexical && !s.topSet {
		s.top, s.topSet = hit.ScoreMicros, true
		s.stop.TopScore = hit.ScoreMicros
	}
	c, err := lexicalContribution(hit.ScoreMicros, s.top, hit.Tier)
	if err != nil {
		return 0, false, err
	}
	if !s.full && s.floorSet {
		tau, err := lexicalTau(c)
		if err != nil {
			return 0, false, err
		}
		if tau < s.floor {
			s.stop.Stopped = true
			s.stop.NextScore, s.stop.NextContribution, s.stop.Tau = hit.ScoreMicros, c, tau
			return c, false, nil
		}
	}
	s.stop.Read++
	if s.full || s.floorSet {
		return c, true, nil
	}
	witness, err := s.witness(hit.FileID)
	if err != nil {
		return 0, false, err
	}
	if witness {
		s.seen[hit.FileID] = true
		s.stop.Witnesses = append(s.stop.Witnesses, hit.FileID)
		if len(s.stop.Witnesses) >= s.needed {
			s.floor, s.floorSet = c, true
		}
	}
	return c, true, nil
}

// witness reports whether a read match's file is a new witness: a file of the
// pinned snapshot whose encoded source fits one slice, by bytes and by the
// token estimate, and that the read has not counted yet.
func (s *lexicalStop) witness(id model.FileID) (bool, error) {
	if id == "" || s.seen[id] {
		return false, nil
	}
	size, ok := s.sizes[id]
	if !ok {
		return false, nil
	}
	wire, err := wireEncodedBytes(size)
	if err != nil {
		return false, err
	}
	if wire > s.maxBytes {
		return false, nil
	}
	tokens, err := estimateTokens(wire)
	if err != nil {
		return false, err
	}
	return tokens <= s.maxTokens, nil
}
