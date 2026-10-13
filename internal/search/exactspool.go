package search

// The exact phase's continuation state: the served order of the exact tiers
// after a position, sorted once and laid into a spool the later pages read
// their own records out of.
//
// The first page of an answer walks the tiers and keeps the least limit+1 hits
// (exactHeap), which costs one walk and nothing on disk, so an answer read one
// page deep pays nothing here. The first continuation inside the exact phase
// walks the tiers once more, sorts every hit after its position (an external
// sort under the query memory admission's sort share), serves its page from the
// head of that run and spools the rest. Every later page reads its own records
// at the byte offset its cursor carries: its cost is its page, never the tier
// behind it.
//
// A spool lives as long as its cursor's lease (a process that records no lease
// binds it to the cursor's stamped expiry), is released when the exact phase
// is read to its end, and is otherwise reclaimed by the spool store's sweep.
// When it is gone, or the spool budget cannot hold it, the page walks from the
// position instead, which serves the same hits.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// exactSpoolPos is where a spooled exact phase stands: the byte offset the
// next page's first record starts at, how many records remain from there, and
// the tier and node of the last hit served, from which a page whose spool is
// gone walks instead.
type exactSpoolPos struct {
	Offset    int64        `json:"offset"`
	Remaining int64        `json:"remaining"`
	Rank      int          `json:"rank"`
	Node      model.NodeID `json:"node"`
}

// searchToken is a search continuation: the shared cursor, and when the exact
// phase is spooled, the spool's position. A spooled cursor names its spool in
// Cursor.SpoolID and carries no LastKey.
type searchToken struct {
	pagination.Cursor
	Exact *exactSpoolPos `json:"exact_spool,omitempty"`
}

// validate refuses a token that is not a search continuation.
func (t searchToken) validate() error {
	if err := t.Cursor.Validate(); err != nil {
		return err
	}
	if t.Endpoint != endpointSearch {
		return cursorInvalid("cursor was issued by a different endpoint")
	}
	if (t.Exact == nil) != (t.SpoolID == "") {
		return cursorInvalid("the search cursor names a spool without its position, or a position without its spool")
	}
	if e := t.Exact; e != nil && (e.Offset <= 0 || e.Remaining <= 0 || e.Rank < model.TierExactPath.Rank() ||
		e.Rank > model.TierExactName.Rank() || !model.ValidHexID(string(e.Node))) {
		return cursorInvalid("the search cursor carries a malformed spool position")
	}
	return nil
}

// decodeSearchToken verifies a search continuation.
func decodeSearchToken(signer *pagination.Signer, token string, now time.Time) (searchToken, error) {
	if signer == nil {
		return searchToken{}, &model.Error{Code: model.CodeInternal, Message: "search: no cursor signer is configured"}
	}
	payload, err := signer.Verify(token, pagination.PurposeCursor, now)
	if err != nil {
		return searchToken{}, err
	}
	var t searchToken
	if err := json.Unmarshal(payload, &t); err != nil {
		return searchToken{}, cursorInvalid("cursor payload is malformed")
	}
	if err := t.validate(); err != nil {
		return searchToken{}, err
	}
	return t, nil
}

// encodeSearchToken signs a search continuation.
func encodeSearchToken(signer *pagination.Signer, t searchToken) (string, error) {
	t.ExpiresAt = t.ExpiresAt.UTC().Truncate(time.Second)
	if err := t.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(t)
	if err != nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "search: cursor encoding: " + err.Error()}
	}
	return signer.Sign(pagination.PurposeCursor, payload, t.ExpiresAt)
}

// exactRecord encodes one exact hit: the fields cmpExact orders by and served
// projects, nothing more. The node id is its 32 raw bytes, and the file id a
// presence byte and its 32 raw bytes, not their 64-character hex spellings;
// strings are length-prefixed; a node with no byte interval carries a zero
// flag and no offsets.
func exactRecord(e exactEntry) ([]byte, error) {
	n := e.Node.Node
	node, err := model.DecodeID(string(n.ID))
	if err != nil {
		return nil, err
	}
	var file []byte
	if n.FileID != "" {
		if file, err = model.DecodeID(string(n.FileID)); err != nil {
			return nil, err
		}
	}
	strs := [...]string{e.Path, string(n.Kind), n.Name, n.QualifiedName, n.Signature}
	size := 1 + len(node) + 1 + len(file) + 1 + 2*binary.MaxVarintLen64
	for _, s := range strs {
		size += binary.MaxVarintLen64 + len(s)
	}
	b := make([]byte, 0, size)
	b = append(b, byte(e.Tier.Rank()))
	b = append(b, node...)
	if file == nil {
		b = append(b, 0)
	} else {
		b = append(append(b, 1), file...)
	}
	for _, s := range strs {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	if e.Node.Bytes == nil {
		return append(b, 0), nil
	}
	b = append(b, 1)
	b = binary.AppendUvarint(b, e.Node.Bytes.Start)
	return binary.AppendUvarint(b, e.Node.Bytes.End), nil
}

// errExactRecord is a record exactRecord did not write.
var errExactRecord = errors.New("search: a spooled exact hit is malformed")

// exactFromRecord decodes what exactRecord wrote.
func exactFromRecord(b []byte) (exactEntry, error) {
	const idBytes = model.IDHexLen / 2
	if len(b) < 1+idBytes+1 {
		return exactEntry{}, errExactRecord
	}
	tier, ok := tierOfRank(int(b[0]))
	if !ok {
		return exactEntry{}, errExactRecord
	}
	var e exactEntry
	e.Tier = tier
	e.Node.Node.ID = model.NodeID(hex.EncodeToString(b[1 : 1+idBytes]))
	b = b[1+idBytes:]
	switch b[0] {
	case 0:
		b = b[1:]
	case 1:
		if len(b) < 1+idBytes {
			return exactEntry{}, errExactRecord
		}
		e.Node.Node.FileID = model.FileID(hex.EncodeToString(b[1 : 1+idBytes]))
		b = b[1+idBytes:]
	default:
		return exactEntry{}, errExactRecord
	}
	var strs [5]string
	for i := range strs {
		n, w := binary.Uvarint(b)
		if w <= 0 || uint64(len(b)-w) < n {
			return exactEntry{}, errExactRecord
		}
		strs[i], b = string(b[w:w+int(n)]), b[w+int(n):]
	}
	e.Path = strs[0]
	e.Node.Node.Kind, e.Node.Node.Name, e.Node.Node.QualifiedName = model.NodeKind(strs[1]), strs[2], strs[3]
	e.Node.Node.Signature, e.Node.Node.SemanticSource = strs[4], model.SemanticCanonical
	if len(b) < 1 {
		return exactEntry{}, errExactRecord
	}
	if b[0] == 0 {
		if len(b) != 1 {
			return exactEntry{}, errExactRecord
		}
		return e, nil
	}
	b = b[1:]
	start, w := binary.Uvarint(b)
	if w <= 0 {
		return exactEntry{}, errExactRecord
	}
	end, w2 := binary.Uvarint(b[w:])
	if w2 <= 0 || w+w2 != len(b) || end < start {
		return exactEntry{}, errExactRecord
	}
	e.Node.Bytes = &model.ByteRange{Start: start, End: end}
	return e, nil
}

// tierOfRank is the exact tier of a rank.
func tierOfRank(rank int) (model.SearchTier, bool) {
	for _, t := range []model.SearchTier{model.TierExactPath, model.TierExactQualifiedName,
		model.TierQualifiedNamePrefix, model.TierExactName} {
		if t.Rank() == rank {
			return t, true
		}
	}
	return "", false
}

// exactRecordBytes charges one buffered hit to the sort's run budget: its
// strings and the fixed part of the entry.
func exactRecordBytes(e exactEntry) int64 {
	n := e.Node.Node
	return int64(len(e.Path)+len(n.ID)+len(n.FileID)+len(n.Kind)+len(n.Name)+len(n.QualifiedName)+
		len(n.Signature)) + exactEntryFixedBytes
}

// exactEntryFixedBytes is an exactEntry's size less its strings' bytes: the
// struct with its string headers, the byte interval and the sort's own slot.
const exactEntryFixedBytes = 512

// sortedAfter walks the qualified-name prefix tier, the one exact tier no
// index holds in served order, and answers every servable hit after `after`
// (nil: from the tier's start), in served order, as a sorted run the caller
// closes. Memory is the sort's run share of the query memory admission; the
// rest is on disk.
//
// A walk the request's deadline ends still answers a run: what the walk had
// read, sorted, beside the deadline error, so the page can serve the head of
// it truncated rather than nothing (an answer cut short by its time limit
// serves the results gathered so far, as a search engine's timed-out query
// does). Any other failure answers no run.
func (s *Service) sortedAfter(ctx context.Context, reader exactReader, rule *tierRule, filter hitFilter,
	after *exactEntry) (*pagination.SortedRun[exactEntry], error) {
	sorter, err := pagination.NewExternalSort(s.spools.SortDir(), 0, exactRecord, exactFromRecord, cmpExact)
	if err != nil {
		return nil, err
	}
	sorter = sorter.WithRunBytes(pagination.SortRunBytes(s.queryMemory), exactRecordBytes)
	defer sorter.Close()
	prefix := model.TierQualifiedNamePrefix.Rank()
	walkErr := exactCandidates(ctx, reader, rule, model.MaxPageItems, tierSpan{from: prefix, to: prefix},
		func(e exactHit) error {
			entry := exactEntry{e}
			if !servableExact(e, filter) || (after != nil && cmpExact(entry, *after) <= 0) {
				return nil
			}
			return sorter.Add(entry)
		})
	if walkErr != nil && !(isDeadline(walkErr) && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return nil, walkErr
	}
	run, err := sorter.Sorted()
	if err != nil {
		return nil, errors.Join(walkErr, err)
	}
	return run, walkErr
}

// errPageTaken stops a read once a page has its records.
var errPageTaken = errors.New("search: the page has its exact hits")

// headOf answers the first n hits of a sorted run.
func headOf(run *pagination.SortedRun[exactEntry], n int) ([]exactEntry, error) {
	out := make([]exactEntry, 0, min(int64(n), run.Len()))
	err := run.Each(func(e exactEntry) error {
		if len(out) == n {
			return errPageTaken
		}
		out = append(out, e)
		return nil
	})
	if err != nil && !errors.Is(err, errPageTaken) {
		return nil, err
	}
	return out, nil
}

// spooledPage reads the next min(limit, remaining) hits of a spooled exact
// phase and the offset the page after them starts at. A spool that is gone,
// expired or not this cursor's answers gone=true, and the caller walks from
// the position instead.
func (s *Service) spooledPage(ctx context.Context, c pagination.Cursor, at exactSpoolPos, limit int,
	now time.Time) (entries []exactEntry, next int64, gone bool, err error) {
	want := int(min(int64(limit), at.Remaining))
	entries = make([]exactEntry, 0, want)
	next, err = s.spools.OpenAt(ctx, c, now, at.Offset, func(record []byte) error {
		e, err := exactFromRecord(record)
		if err != nil {
			return err
		}
		entries = append(entries, e)
		if len(entries) == want {
			return pagination.ErrStopSpool
		}
		return nil
	})
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == model.CodeCursorInvalid {
		return nil, 0, true, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if len(entries) != want {
		return nil, 0, false, cursorInvalid("the search cursor's spool is shorter than the answer it names")
	}
	return entries, next, false, nil
}

// spoolRest lays the run's hits after its first `skip` into a new spool bound
// to c, and answers the spool's id and the offset its first record starts at.
// A spool budget too small to hold them answers ok=false with nothing left
// behind: the caller mints a positional continuation, which serves the same
// hits by walking.
func (s *Service) spoolRest(run *pagination.SortedRun[exactEntry], skip int, c pagination.Cursor) (id string,
	offset int64, ok bool, err error) {
	sp, err := s.spools.Create(c)
	if err != nil {
		return "", 0, false, refusedOrErr(err)
	}
	offset = sp.Written()
	at := 0
	err = run.Each(func(e exactEntry) error {
		if at++; at <= skip {
			return nil
		}
		record, err := exactRecord(e)
		if err != nil {
			return err
		}
		return sp.Append(record)
	})
	if cerr := sp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := s.spools.Release(sp.ID()); rerr != nil {
			return "", 0, false, errors.Join(err, rerr)
		}
		return "", 0, false, refusedOrErr(err)
	}
	return sp.ID(), offset, true, nil
}

// refusedOrErr turns a full spool budget into "no spool" (nil): the answer is
// complete without one. Every other failure is returned.
func refusedOrErr(err error) error {
	if pagination.IsBudgetExhausted(err) {
		return nil
	}
	return err
}

// exactRest is a page's sorted remainder on its way to a spool: the run and
// how many of its first hits the page served.
type exactRest struct {
	run  *pagination.SortedRun[exactEntry]
	skip int
}

// spoolNext mints the continuation of a page whose exact hits came from a fresh
// sort: a lease (when this process records them), a spool of the hits the page
// did not serve, and the token naming both. ok=false means no spool could be
// made (the budget is full); the caller then mints a positional continuation.
func (s *Service) spoolNext(ctx context.Context, reader *sqlite.PinnedReader, b model.Binding, hash string,
	now time.Time, rest exactRest, last searchPosition) (string, bool, error) {
	var leaseID string
	if reader.Continuable() {
		lease, err := s.leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", false, err
		}
		leaseID = lease.ID
	}
	c := newCursor(endpointSearch, b, leaseID, hash, now.Add(s.ttl))
	c.ExpiresAt = c.ExpiresAt.Truncate(time.Second)
	id, offset, ok, err := s.spoolRest(rest.run, rest.skip, c)
	if err != nil || !ok {
		s.releaseLease(ctx, leaseID)
		return "", false, err
	}
	c.SpoolID = id
	token, err := encodeSearchToken(s.signer, searchToken{Cursor: c, Exact: &exactSpoolPos{Offset: offset,
		Remaining: rest.run.Len() - int64(rest.skip), Rank: last.rank, Node: last.node}})
	if err != nil {
		s.releaseSpool(id)
		s.releaseLease(ctx, leaseID)
		return "", false, err
	}
	return token, true, nil
}

// releaseSpool removes a spool nothing will read again. A failure is logged:
// the sweep reclaims what a release could not.
func (s *Service) releaseSpool(id string) {
	if err := s.spools.Release(id); err != nil {
		s.log.Warn("a search spool could not be released", "component", "search", "error", err.Error())
	}
}

// spoolContinue mints the continuation of a page read from a spool that still
// holds hits: the same spool under the same lease, renewed when this process
// records leases, at the offset the page stopped. A leaseless spool keeps the
// expiry its header was stamped with.
func (s *Service) spoolContinue(ctx context.Context, t searchToken, at exactSpoolPos, now time.Time) (string, error) {
	next := t
	if next.LeaseID != "" && s.leases.Retains() {
		if _, err := s.leases.Renew(ctx, next.LeaseID); err != nil {
			return "", err
		}
		next.ExpiresAt = now.Add(s.ttl)
	}
	next.Exact = &at
	return encodeSearchToken(s.signer, next)
}
