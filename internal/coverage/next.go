package coverage

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Next answers `context next`: the first required_full file of this session's
// manifest that is not yet full_served for this actor, with its path, the
// pinned content hash, the stored size, the offset to resume at and how many
// required files remain. Section 19.2 marks the tool metadata only, so this
// path never opens a Source, never reads a byte and never signs a receipt --
// the read endpoint is the only way to obtain source, and only a confirmed
// receipt grants coverage.
//
// The walk is the frozen Task 15 manifest contract and nothing more: entries
// persist in Section 15.3 tie-break order, ordinals 0..n-1 are the canonical
// reading order and the required_full entries occupy a prefix. Next therefore
// pages ordinals ascending and stops at the first entry that is not
// required_full. It never sorts and never re-ranks; re-deriving an order here
// would be a second ranking implementation.
func (s *Service) Next(ctx context.Context, req model.SessionRequest) (model.NextContextItem, error) {
	if err := req.Validate(); err != nil {
		return model.NextContextItem{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()

	// Gate the actor before anything else: Store.Session skips its actor check
	// on an empty actor, and UnconfirmedChunks below takes no actor at all.
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.NextContextItem{}, err
	}
	// One aggregate call for the counts. Paging Coverage for them would cost
	// 1250 round trips on a 250k-file session.
	c, err := s.sessions.CoverageSummary(ctx, rec.ID, rec.ActorID)
	if err != nil {
		return model.NextContextItem{}, err
	}
	// Remaining counts against FullyRead, not the reported Served: this number
	// is paired with Action, and the walk below stops at the first file that is
	// not full_served without consulting waivers. Subtracting Served instead
	// would let a waived-and-fully-read file raise Remaining while the walk
	// finds nothing unserved, and `context next` would answer action=complete
	// beside remaining=1. A waiver is still never fabricated coverage -- a
	// waived and UNREAD file is unread here, is walked like any other, and
	// raises Remaining -- and whether any waiver blocks readiness remains Task
	// 17's question, not this one.
	remaining := c.Required - c.FullyRead
	if remaining < 0 {
		remaining = 0
	}

	item := model.NextContextItem{Binding: rec.Binding, Remaining: remaining}
	cov, err := s.nextUnserved(ctx, rec)
	switch {
	case errors.Is(err, errNoSource):
		// Nothing readable remains. An incomplete scope is not completion: the
		// manifest could not resolve every boundary, so the actor reviews the
		// scope rather than concluding the read is done.
		// The manifest is read only here: ScopeComplete is the one field this
		// path needs, and on the hot path the entry walk already proves the
		// manifest is readable.
		manifest, err := s.sessions.Manifest(ctx, rec.ManifestID)
		if err != nil {
			return model.NextContextItem{}, err
		}
		item.Action = actionComplete
		if !manifest.ScopeComplete {
			item.Action = actionReviewScope
		}
	case err != nil:
		return model.NextContextItem{}, err
	default:
		// The pinned session row is the only place the path lives: coverage
		// and manifest entries carry identifiers alone, and `context next`
		// names a file the operator has to be able to find. The batch reader is
		// asked for the one file this branch named -- Next answers with a
		// single item, so a page of paths would be a page of one -- and it is
		// scoped by session_files, so it can never name a file outside the
		// actor's pinned scope.
		paths, err := s.sessions.SessionFilePaths(ctx, rec.ID, rec.ActorID, []model.FileID{cov.FileID})
		if err != nil {
			return model.NextContextItem{}, err
		}
		path, ok := paths[cov.FileID]
		if !ok {
			// A file the session pinned that the snapshot no longer holds at
			// that hash is the same manifest/session disagreement coverageOf
			// reports. Absence is the batch reader's answer for it, and it is
			// not the caller's argument that is wrong.
			return model.NextContextItem{}, typedErrf(model.CodeScopeIncomplete,
				"file %s is in this session's scope but not in the pinned snapshot", cov.FileID)
		}
		item.FileID = cov.FileID
		item.Path = path
		item.ContentHash = cov.ContentHash
		item.Requirement = cov.Requirement
		item.Size = cov.Size
		item.Offset, err = s.resumeOffset(ctx, rec, cov)
		if err != nil {
			return model.NextContextItem{}, err
		}
		item.Action, err = s.nextAction(ctx, rec, item.Offset >= uint64(cov.Size))
		if err != nil {
			return model.NextContextItem{}, err
		}
	}
	if err := item.Validate(); err != nil {
		return model.NextContextItem{}, err
	}
	return item, nil
}

// resumeOffset is where the actor should resume reading this file: the first
// byte of it this actor has not confirmed.
//
// ConfirmedBytes is the size of the disjoint served union, so it is the end of
// the confirmed prefix for a client that reads forward and over-states it when
// a client confirmed a later range with a hole before it. An over-stated offset
// sends the actor past bytes nobody read, and on a partially served file it can
// leave the file permanently short of full coverage. The prefix end is
// therefore asked of storage, which answers containment of [0,p) in SQL.
//
// The union size bounds the prefix end from above, so ConfirmedBytes is both
// the first guess and the top of the search: the forward-reading client is one
// query, and a client that confirmed out of order costs a binary search over
// [0,ConfirmedBytes] -- at most 1 + log2(ConfirmedBytes) bounded boolean
// questions, and never an interval list crossing into Go.
func (s *Service) resumeOffset(ctx context.Context, rec sqlite.SessionRecord, cov model.FileCoverage) (uint64, error) {
	hi := cov.ConfirmedBytes
	if hi > cov.Size {
		hi = cov.Size
	}
	if hi <= 0 {
		// Nothing is confirmed, so the whole file is still to read. Storage is
		// not asked: the empty interval is not a range it can be asked about.
		return 0, nil
	}
	confirmed := func(end int64) (bool, error) {
		return s.sessions.RangeConfirmed(ctx, rec.ID, rec.ActorID, cov.FileID, cov.ContentHash,
			model.ByteRange{Start: 0, End: uint64(end)})
	}
	ok, err := confirmed(hi)
	if err != nil {
		return 0, err
	}
	if ok {
		return uint64(hi), nil
	}
	// Invariant: [0,lo) is confirmed and [0,hi) is not, so the first uncovered
	// byte is in [lo,hi). lo starts at zero, which is vacuously confirmed.
	lo := int64(0)
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		ok, err := confirmed(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return uint64(lo), nil
}

// nextAction names what the actor should do with the file Next selected.
// Reading is the answer unless reading cannot make progress:
//
//   - atEOF means the resume offset has reached the stored size while the file
//     is still not full_served, which is the empty required file whose
//     zero-length EOF chunk is issued but not yet confirmed. With a chunk
//     outstanding the missing step is the acknowledgment, not another read.
//   - at the unconfirmed cap the read endpoint refuses to issue anything, so
//     acknowledging outstanding receipts is the only productive move.
func (s *Service) nextAction(ctx context.Context, rec sqlite.SessionRecord, atEOF bool) (string, error) {
	unconfirmed, err := s.sessions.UnconfirmedChunks(ctx, rec.ID)
	if err != nil {
		return "", err
	}
	// Zero is unlimited (Limits.MaxUnconfirmedChunksPerSession); the one
	// spelling of that comparison lives on Limits, so this endpoint and the
	// read endpoint cannot drift about what "capped" means.
	if (atEOF && unconfirmed > 0) || s.limits.unconfirmedCapped(unconfirmed) {
		return actionAcknowledgeReceipt, nil
	}
	return actionReadSource, nil
}

// nextUnserved walks the manifest's required_full prefix in ascending ordinal
// order and returns the coverage of the first entry that is not full_served for
// this actor. It reports errNoSource when the prefix is exhausted.
func (s *Service) nextUnserved(ctx context.Context, rec sqlite.SessionRecord) (model.FileCoverage, error) {
	limit := s.metadataPageLimit()
	// Coverage pages by file_id while the manifest walks by ordinal, so a page
	// fetched for one entry is kept for the entries that share it.
	seen := make(map[model.FileID]model.FileCoverage)
	// Store.ManifestEntries selects `ordinal > ?`, so the first page starts at
	// -1 rather than 0; starting at 0 would skip the first required file.
	after := -1
	for {
		entries, err := s.sessions.ManifestEntries(ctx, rec.ManifestID, after, limit)
		if err != nil {
			return model.FileCoverage{}, err
		}
		if len(entries) == 0 {
			return model.FileCoverage{}, errNoSource
		}
		for _, e := range entries {
			if e.Requirement != model.RequirementFull {
				// The end of the required_full prefix: everything after it is
				// recommended or optional and is never required reading.
				return model.FileCoverage{}, errNoSource
			}
			after = e.Ordinal
			if e.FileID == "" {
				// A symbol-scoped entry carries no file to read.
				continue
			}
			cov, err := s.coverageOf(ctx, rec, e.FileID, seen, limit)
			if err != nil {
				return model.FileCoverage{}, err
			}
			if cov.State == model.CoverageFullServed {
				continue
			}
			return cov, nil
		}
	}
}

// coverageOf reads one file's coverage row through the paged Coverage reader,
// caching every record the page carries. Coverage is a keyset page over
// `file_id > after`, so asking for the identifier immediately below the wanted
// one returns it as the page's first row.
func (s *Service) coverageOf(ctx context.Context, rec sqlite.SessionRecord, file model.FileID, seen map[model.FileID]model.FileCoverage, limit int) (model.FileCoverage, error) {
	if cov, ok := seen[file]; ok {
		return cov, nil
	}
	before, err := previousFileID(file)
	if err != nil {
		return model.FileCoverage{}, err
	}
	page, err := s.sessions.Coverage(ctx, rec.ID, rec.ActorID, before, limit)
	if err != nil {
		return model.FileCoverage{}, err
	}
	for _, cov := range page {
		seen[cov.FileID] = cov
	}
	if cov, ok := seen[file]; ok {
		return cov, nil
	}
	// OpenSession populates session_files from the manifest, so a manifest
	// entry with no coverage row means the session's pinned scope and the
	// manifest disagree. That is not a file to skip silently.
	return model.FileCoverage{}, typedErrf(model.CodeScopeIncomplete,
		"manifest entry %s has no coverage row in this session", file)
}

// previousFileID is the largest identifier strictly below file: the decoded
// 32-byte value minus one. The decrement is over the decoded bytes and not the
// hex digits, because the storage reader decodes the keyset key as a blob and
// rejects anything that is not a well-formed identifier. The zero identifier
// has no predecessor, and the empty key pages from the start.
func previousFileID(file model.FileID) (model.FileID, error) {
	raw, err := model.DecodeID(string(file))
	if err != nil {
		return "", typedErrf(model.CodeArgumentInvalid, "manifest entry carries a malformed file identifier")
	}
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == 0 {
			raw[i] = 0xff
			continue
		}
		raw[i]--
		return model.FileID(hex.EncodeToString(raw)), nil
	}
	return "", nil
}

// metadataPageLimit is the page size this metadata path asks for. Next carries
// no source bytes, so it spends only the page ceiling: the configured
// MaxPageItems, itself bounded by the model ceiling.
func (s *Service) metadataPageLimit() int {
	limit := s.limits.MaxPageItems
	if limit <= 0 || limit > model.MaxPageItems {
		return model.MaxPageItems
	}
	return limit
}
