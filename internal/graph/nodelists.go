package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// MetadataRowReader is the seam a reader offers when it can read the metadata
// rows of a node: the elements of a metadata field too long for the node row,
// which model.SplitMetadata stores as evidence rows on the node and names with
// a "<field>_rows" marker. It is optional, resolved by type assertion, so a
// reader without it answers "unavailable" rather than an empty list.
type MetadataRowReader interface {
	// MetadataRows returns every visible row of node's field, in ordinal
	// order. The list is bounded by the node's own file: the rows are that
	// file's elements.
	MetadataRows(ctx context.Context, node model.NodeID, field string) ([]model.MetadataRow, error)
}

// metadataRowReader is the seam of r, nil when r lacks it.
func metadataRowReader(r any) MetadataRowReader {
	m, _ := r.(MetadataRowReader)
	return m
}

// metadataRowsUnavailable is the answer of a reader that cannot read rows.
func metadataRowsUnavailable() error {
	return &model.Error{Code: model.CodeProviderUnavailable,
		Message: "this index cannot read node metadata rows: its reader does not page a node's evidence"}
}

// rowFragments reads an open import site's fragments from its metadata rows.
// want is the row count the site's metadata names; a reader that returns
// fewer serves a list the evidence clip cut, which the capability rows
// already report, and the fragments it has are used in order.
func rowFragments(ctx context.Context, r any, node model.NodeID, want int64) ([]string, error) {
	rr := metadataRowReader(r)
	if rr == nil {
		return nil, metadataRowsUnavailable()
	}
	rows, err := rr.MetadataRows(ctx, node, model.MetaFragments)
	if err != nil {
		return nil, err
	}
	if int64(len(rows)) > want {
		return nil, internalErr(fmt.Sprintf("open import site %s has %d fragment rows, its metadata names %d", node, len(rows), want))
	}
	raw, err := model.JoinMetadataRows(rows)
	if err != nil {
		return nil, err
	}
	var fragments []string
	if err := json.Unmarshal(raw, &fragments); err != nil {
		return nil, internalErr("open import site fragment rows are not strings: " + err.Error())
	}
	return fragments, nil
}

// nodeMetadataEndpoint binds a node-metadata continuation to this operation.
const nodeMetadataEndpoint = "graph.node_metadata"

// nodeMetadataQueryHashDomain is the hash domain of the query a continuation
// is bound to: the node and the field.
const nodeMetadataQueryHashDomain = "graph.node_metadata.query"

func nodeMetadataQueryHash(node model.NodeID, field string) string {
	h := model.NewHasher(nodeMetadataQueryHashDomain)
	h.AddString(string(node))
	h.AddString(field)
	return h.Sum()
}

// NodeMetadata answers one page of a node field's metadata rows, in ordinal
// order. A continuation is a keyset on the last ordinal served, bound to the
// generation, the node and the field, and holds a cursor lease on the
// generation like every other continuation.
//
// Each page reads the field's whole row list and serves the rows after the
// cursor's ordinal. The list is bounded by one file (the rows are its
// elements), so this costs one node's evidence per page, never the
// repository's.
func (e *Engine) NodeMetadata(ctx context.Context, req model.NodeMetadataRequest) (page model.Page[model.MetadataRow], err error) {
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.Page[model.MetadataRow]{}, err
	}
	reader, err := e.Reader()
	if err != nil {
		return model.Page[model.MetadataRow]{}, err
	}
	rr := metadataRowReader(reader)
	if rr == nil {
		return model.Page[model.MetadataRow]{}, metadataRowsUnavailable()
	}
	var cancel context.CancelFunc
	if deadline, bounded := e.queryDeadline(ctx); bounded {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.Page[model.MetadataRow]{}, err
		}
		defer e.gate.Release()
	}
	binding := reader.Binding()
	queryHash := nodeMetadataQueryHash(req.NodeID, req.Field)
	after := int64(-1)
	if req.Page.Cursor != "" {
		if after, err = e.resumeNodeMetadata(ctx, req.Page.Cursor, binding, queryHash); err != nil {
			return model.Page[model.MetadataRow]{}, err
		}
	}
	var notices []string
	limit, notice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	notices = appendNotice(notices, notice)
	if limit > model.MaxPageItems {
		if req.Page.Limit > 0 {
			notices = appendNotice(notices, fmt.Sprintf(
				"page.limit: effective %d, served %d (the wire ceiling)", limit, model.MaxPageItems))
		}
		limit = model.MaxPageItems
	}
	rows, err := rr.MetadataRows(ctx, req.NodeID, req.Field)
	if err != nil {
		return model.Page[model.MetadataRow]{}, err
	}
	items := make([]model.MetadataRow, 0, min(limit, len(rows)))
	more := false
	for _, r := range rows {
		if r.Ordinal <= after {
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		items = append(items, r)
	}
	caps, err := e.completeness(ctx)
	if err != nil {
		return model.Page[model.MetadataRow]{}, err
	}
	meta := model.QueryMeta{Binding: binding, Completeness: caps, Notices: notices}
	if more {
		token, why, err := e.nodeMetadataCursor(ctx, binding, queryHash, items[len(items)-1].Ordinal)
		if err != nil {
			return model.Page[model.MetadataRow]{}, err
		}
		meta.NextCursor = token
		if token == "" {
			meta.Truncated, meta.TruncationReason = true, why
		}
	}
	page = model.Page[model.MetadataRow]{Meta: meta, Items: items}
	if err := page.Validate(); err != nil {
		return model.Page[model.MetadataRow]{}, err
	}
	for i := range items {
		if err := items[i].Validate(); err != nil {
			return model.Page[model.MetadataRow]{}, err
		}
	}
	return page, nil
}

// resumeNodeMetadata verifies a presented continuation and returns the last
// ordinal it served. A token for another endpoint, generation, node or field
// is refused: its ordinal means nothing here. The continuation is consumed:
// its lease is released once its position is read.
func (e *Engine) resumeNodeMetadata(ctx context.Context, token string, binding model.Binding, queryHash string) (int64, error) {
	if e.signer == nil {
		return 0, cursorInvalid("continuations are not available in this workspace")
	}
	c, err := e.signer.DecodeCursor(token, nodeMetadataEndpoint, e.now())
	if err != nil {
		return 0, err
	}
	if c.GenerationID != binding.GenerationID || c.AnalysisKey != binding.AnalysisKey {
		return 0, cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	if c.QueryHash != queryHash {
		return 0, cursorInvalid("cursor was issued for a different node or field")
	}
	if c.SpoolID != "" {
		return 0, cursorInvalid("cursor carries spooled state this endpoint does not produce")
	}
	after, err := strconv.ParseInt(c.LastKey, 10, 64)
	if err != nil || after < 0 {
		return 0, cursorInvalid("cursor sort key is not a metadata row ordinal")
	}
	e.releaseConsumed(ctx, "", c.LeaseID)
	return after, nil
}

// nodeMetadataCursor signs the continuation after ordinal last, or says why
// there cannot be one.
func (e *Engine) nodeMetadataCursor(ctx context.Context, binding model.Binding, queryHash string, last int64) (token, why string, err error) {
	if e.signer == nil {
		return "", "this workspace does not offer query continuations, so the remaining rows cannot be paged", nil
	}
	if !model.ValidHexID(string(binding.AnalysisKey)) {
		return "", "this generation is not yet analysed, so its rows cannot be continued", nil
	}
	var leaseID string
	if e.leases.Retains() {
		lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", "", err
		}
		leaseID = lease.ID
	}
	token, err = e.signer.EncodeCursor(pagination.Cursor{
		Endpoint:     nodeMetadataEndpoint,
		GenerationID: binding.GenerationID,
		AnalysisKey:  binding.AnalysisKey,
		QueryHash:    queryHash,
		LastKey:      strconv.FormatInt(last, 10),
		LeaseID:      leaseID,
		ExpiresAt:    e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	})
	if err != nil {
		return "", "", e.releaseLease(ctx, leaseID, err)
	}
	return token, "", nil
}
