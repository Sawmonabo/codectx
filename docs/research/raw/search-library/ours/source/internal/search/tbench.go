package search

// Benchmark harness only (throwaway worktree). BenchLexical is Search's
// first-page path with the exact tiers removed: the request is validated, the
// active generation pinned, the filter pinned, the lexical tier streamed into
// the deduplication collector, the deduplicated run walked into the page heap
// and the continuation spool, the page hydrated, and the cursor signed. It
// differs from Search in exactly those removed exact tiers, and in returning
// the served page's search keys and counts beside the hits.

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// BenchFilter is the request's filters as the product takes them:
// SearchRequest.Kinds (`codectx search --kind`) and SearchRequest.Paths (the
// path filter the MCP search tool takes).
type BenchFilter struct {
	Kinds []model.NodeKind
	Paths []string
}

func (f BenchFilter) request(query string, limit int) model.SearchRequest {
	return model.SearchRequest{Query: query, Kinds: f.Kinds, Paths: f.Paths, Page: model.PageRequest{Limit: limit}}
}

// BenchAnswer is one answered query.
type BenchAnswer struct {
	// Docs is every lexical document the tier matched (occurrences > 0)
	// that the request's filters keep.
	Docs int64
	// Served is the deduplicated candidate count the answer pages over.
	Served int64
	// Keys are the served page's search keys, in served order.
	Keys []string
	Hits []model.SearchHit
	// Cursor is the continuation token; Spool the spool it names ("" when
	// the answer fit one page). The caller releases it with BenchRelease.
	Cursor, Spool string
}

// BenchLexical answers query's first page of at most limit hits.
func (s *Service) BenchLexical(ctx context.Context, query string, limit int, f BenchFilter) (BenchAnswer, error) {
	var ans BenchAnswer
	req := f.request(query, limit)
	if err := req.Validate(); err != nil {
		return ans, err
	}
	filter, err := newHitFilter(req)
	if err != nil {
		return ans, err
	}
	ctx, _ = sqlite.WithPageClamps(ctx)
	now := s.now()
	hash := searchQueryHash(req)
	reader, err := s.store.PinGeneration(ctx, s.repo, 0, s.ttl)
	if err != nil {
		return ans, err
	}
	defer reader.Close()
	binding := reader.Binding()
	limit, _ = s.pageLimit(req.Page.Limit)
	if err := filter.pin(ctx, s.store, binding.SnapshotID); err != nil {
		return ans, err
	}

	c, err := newCollector(s.spools.SortDir(), s.runBytes)
	if err != nil {
		return ans, err
	}
	defer c.Close()
	searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
	_, err = s.lexical.search(searchCtx, readerPostings{reader}, binding.AnalysisKey, req.Query, func(h lexicalHit) error {
		d := h.Doc
		if !filter.keep(d.Path) || !matchesKinds(d.Kind, req.Kinds) {
			return nil
		}
		ans.Docs++
		r := ranked{Tier: model.TierLexicalFTS, ScoreMicros: h.ScoreMicros, Path: d.Path,
			StartByte: d.Bytes.Start, NodeID: d.NodeID, SearchKey: d.ID, RowID: d.RowID, Occurrences: 1}
		bytes := d.Bytes
		return c.add(r, hitFacts{span: &bytes, hit: model.SearchHit{NodeID: d.NodeID, FileID: d.FileID,
			Path: d.Path, Kind: d.Kind, Name: d.Name, QualifiedName: d.QualifiedName, Signature: d.Signature}},
			reasonFor(model.TierLexicalFTS))
	})
	searchCancel()
	if err != nil {
		return ans, err
	}
	run, err := c.deduped()
	if err != nil {
		return ans, err
	}
	defer run.Close()

	// firstPage, keeping the page's search keys.
	h := &pageHeap{limit: limit}
	w := &rawWriter{svc: s, binding: binding, hash: hash, now: now, retains: reader.Continuable()}
	err = run.Each(func(v scored) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		v.ScoreMicros = v.Folded
		ans.Served++
		if w.started() {
			if err := w.append(ctx, v); err != nil {
				return err
			}
			h.offer(v)
			return nil
		}
		h.offer(v)
		if !h.full() {
			return nil
		}
		return w.start(ctx, h.items)
	})
	if err != nil {
		w.abandon(ctx)
		return ans, err
	}
	page := h.page()
	for _, v := range page {
		ans.Keys = append(ans.Keys, v.SearchKey)
	}
	if ans.Hits, err = s.hydrated(ctx, reader, page); err != nil {
		w.abandon(ctx)
		return ans, err
	}
	spool, err := w.close(ctx)
	if err != nil {
		return ans, err
	}
	if spool != "" {
		cur := s.newSearchCursor(binding, hash, w.lease, now)
		cur.SpoolID, cur.Served, cur.Total = spool, int64(len(ans.Hits)), ans.Served
		if ans.Cursor, err = s.signSearchCursor(cur); err != nil {
			s.spools.Release(spool)
			s.releaseLease(ctx, w.lease)
			return ans, err
		}
		ans.Spool = spool
	}
	return ans, nil
}

// BenchRelease drops an answer's continuation spool.
func (s *Service) BenchRelease(a BenchAnswer) {
	if a.Spool != "" {
		s.spools.Release(a.Spool)
	}
}

// BenchDocPage is the document-level answer (no deduplication): every matched
// document the filters keep, ordered by cmpScored, the first limit served. It is the ordering the
// other arm's product mode reproduces, and it is computed outside any timing.
func (s *Service) BenchDocPage(ctx context.Context, query string, limit int, f BenchFilter) (int64, []string, error) {
	req := f.request(query, limit)
	if err := req.Validate(); err != nil {
		return 0, nil, err
	}
	filter, err := newHitFilter(req)
	if err != nil {
		return 0, nil, err
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, 0, s.ttl)
	if err != nil {
		return 0, nil, err
	}
	defer reader.Close()
	if err := filter.pin(ctx, s.store, reader.Binding().SnapshotID); err != nil {
		return 0, nil, err
	}
	h := &pageHeap{limit: limit}
	var n int64
	_, err = s.lexical.search(ctx, readerPostings{reader}, reader.Binding().AnalysisKey, query, func(hit lexicalHit) error {
		d := hit.Doc
		if !filter.keep(d.Path) || !matchesKinds(d.Kind, req.Kinds) {
			return nil
		}
		n++
		bytes := d.Bytes
		h.offer(scored{ranked: ranked{Tier: model.TierLexicalFTS, ScoreMicros: hit.ScoreMicros, Path: d.Path,
			StartByte: d.Bytes.Start, NodeID: d.NodeID, SearchKey: d.ID, RowID: d.RowID, Occurrences: 1},
			Hit: model.SearchHit{NodeID: d.NodeID, FileID: d.FileID, Path: d.Path, Kind: d.Kind, Name: d.Name,
				QualifiedName: d.QualifiedName, Signature: d.Signature}, Span: &bytes})
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	var keys []string
	for _, v := range h.page() {
		keys = append(keys, v.SearchKey)
	}
	return n, keys, nil
}
