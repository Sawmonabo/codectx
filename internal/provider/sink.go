package provider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Limits are the per-batch bounds of Section 11.1: a batch is capped by both
// records and retained bytes, and flushes at whichever is reached first.
// MaxRecordBytes is resources.max_provider_record_bytes; a record over it, or
// over BatchBytes, is CTX_RESOURCE_LIMIT naming the limit it broke.
type Limits struct {
	BatchRecords   int
	BatchBytes     int64
	MaxRecordBytes int64
}

// Validate rejects a limit that would mean unlimited or a record bound no
// batch could hold (config.validate enforces the same relation on the
// configured values).
func (l Limits) Validate() error {
	if l.BatchRecords <= 0 || l.BatchBytes <= 0 || l.MaxRecordBytes <= 0 {
		return resourceLimit(fmt.Sprintf("sink limits are %d records, %d batch bytes and %d record bytes; every bound must be positive",
			l.BatchRecords, l.BatchBytes, l.MaxRecordBytes))
	}
	if l.MaxRecordBytes > l.BatchBytes {
		return resourceLimit(fmt.Sprintf("max record bytes %d exceed batch bytes %d; such a record could never be flushed", l.MaxRecordBytes, l.BatchBytes))
	}
	return nil
}

// Pool is the retained-byte reservation shared by every sink of one indexing
// run (index.queue_bytes). Queued records and outstanding decode reservations
// are both charged against it, so the bytes providers hold before persistence
// are bounded across concurrent units, not only per batch.
type Pool struct {
	capacity int64

	mu      sync.Mutex
	used    int64
	changed chan struct{}
}

// NewPool bounds the pool at capacity bytes.
func NewPool(capacity int64) (*Pool, error) {
	if capacity <= 0 {
		return nil, resourceLimit("the sink byte pool capacity must be positive")
	}
	return &Pool{capacity: capacity, changed: make(chan struct{})}, nil
}

// tryAcquire charges n without blocking.
func (p *Pool) tryAcquire(n int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used+n > p.capacity {
		return false
	}
	p.used += n
	return true
}

// release returns n and wakes every waiter so it can retry.
func (p *Pool) release(n int64) {
	p.mu.Lock()
	p.used -= n
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// wait blocks until the pool changes or ctx ends. It never acquires; the
// caller retries tryAcquire, so no goroutine holds a sink lock while blocked.
func (p *Pool) wait(ctx context.Context) error {
	p.mu.Lock()
	ch := p.changed
	p.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return canceled(ctx.Err())
	}
}

// Used reports the bytes currently charged.
func (p *Pool) Used() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.used
}

// Retained-byte accounting. The estimate is a deterministic function of the
// record's fields: a fixed overhead per record and per evidence row that
// covers struct, slice and serialization-buffer allocation, plus every string
// the record carries. It is deliberately a superset of the estimate storage's
// UnitWriter applies to a handed-off batch, so a batch this sink admits under
// Limits is never rejected by the writer configured with the same bounds.
const (
	recordOverhead   = 256
	evidenceOverhead = 192
)

func evidenceBytes(list []model.Evidence) int64 {
	n := int64(len(list)) * evidenceOverhead
	for _, e := range list {
		n += int64(len(e.ID) + len(e.UnitID) + len(e.ProviderID) + len(e.ProviderVersion) + len(e.OriginRunID) +
			len(e.NodeID) + len(e.RelationID) + len(e.Precision) + len(e.FileID) + len(e.ContentHash) + len(e.NativeKey) + len(e.Detail))
	}
	return n
}

// NodeFactBytes is the retained-byte estimate of one node fact.
func NodeFactBytes(f model.NodeFact) int64 {
	n := f.Node
	return recordOverhead + int64(len(n.ID)+len(n.Kind)+len(n.Language)+len(n.Name)+len(n.QualifiedName)+len(n.Signature)+
		len(n.FileID)+len(n.ContentHash)+len(n.Metadata)+len(f.CanonicalKey)) + evidenceBytes(f.Evidence)
}

// RelationFactBytes is the retained-byte estimate of one relation fact.
func RelationFactBytes(f model.RelationFact) int64 {
	r := f.Relation
	return recordOverhead + int64(len(r.ID)+len(r.From)+len(r.Kind)+len(r.To)) + evidenceBytes(f.Evidence)
}

// AliasBytes is the retained-byte estimate of one native alias.
func AliasBytes(a model.NativeAlias) int64 {
	return recordOverhead + int64(len(a.ScopeKey)+len(a.NativeKey)+len(a.NodeID))
}

// SearchUnitBytes is the retained-byte estimate of one search document.
func SearchUnitBytes(u model.SearchUnit) int64 {
	return recordOverhead + int64(len(u.ID)+len(u.NodeID)+len(u.FileID)+len(u.Path)+len(u.Kind)+len(u.Name)+len(u.QualifiedName)+len(u.Signature)+len(u.Body))
}

// batch is one bounded, typed accumulation awaiting persistence.
type batch[T any] struct {
	items []T
	bytes int64
}

// BatchSink is the Sink handed to a provider for one unit. It owns every
// record from acceptance until the destination has persisted it: a record is
// admitted only after its bytes are reserved in the pool, a batch flushes as
// soon as the next record would exceed either limit, and a failed write
// latches the sink, cancels every producer of the unit and discards what was
// queued. Nothing here reaches the database except through the destination,
// which in production is storage's UnitWriter. It is safe for concurrent use
// by a provider's workers; writes to the destination are serialized.
type BatchSink struct {
	dst    Sink
	limits Limits
	pool   *Pool
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	nodes     batch[model.NodeFact]
	relations batch[model.RelationFact]
	aliases   batch[model.NativeAlias]
	search    batch[model.SearchUnit]
	failed    error
	records   uint64
	bytes     uint64
}

// NewBatchSink binds a sink to its destination, limits and shared pool. cancel
// is invoked with the write error when a required write fails, so every
// goroutine producing into this unit observes cancellation.
func NewBatchSink(dst Sink, limits Limits, pool *Pool, cancel context.CancelCauseFunc) (*BatchSink, error) {
	if dst == nil || pool == nil || cancel == nil {
		return nil, invalid("a batch sink needs a destination, a pool and a cancel function")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if limits.BatchBytes > pool.capacity {
		return nil, resourceLimit(fmt.Sprintf("batch bytes %d exceed the pool capacity %d; a full batch could never be admitted", limits.BatchBytes, pool.capacity))
	}
	return &BatchSink{dst: dst, limits: limits, pool: pool, cancel: cancel}, nil
}

// Records and Bytes report what the sink accepted so far, for the provider's
// run counters.
func (s *BatchSink) Records() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.records }
func (s *BatchSink) Bytes() uint64   { s.mu.Lock(); defer s.mu.Unlock(); return s.bytes }

// Reserve charges n bytes against the pool before a provider decodes or
// buffers input of that size, blocking until the reservation fits and
// returning promptly on cancellation. The returned release must be called
// exactly once. A reservation over MaxRecordBytes is refused outright.
func (s *BatchSink) Reserve(ctx context.Context, n int64) (release func(), err error) {
	if n <= 0 {
		return nil, invalid("a reservation must be positive")
	}
	if n > s.limits.MaxRecordBytes {
		return nil, s.overLimit(n, "max_provider_record_bytes", s.limits.MaxRecordBytes)
	}
	for {
		if err := s.failure(); err != nil {
			return nil, err
		}
		if s.pool.tryAcquire(n) {
			var once sync.Once
			return func() { once.Do(func() { s.pool.release(n) }) }, nil
		}
		// Our own queued batches may be what fills the pool; flushing them
		// releases their share before we wait on anyone else.
		flushed, err := s.flushIfPending(ctx)
		if err != nil {
			return nil, err
		}
		if flushed {
			continue
		}
		if err := s.pool.wait(ctx); err != nil {
			return nil, err
		}
	}
}

// PutNodes accepts node facts. Batches are sorted by node ID before they are
// written so the transaction shape is a function of content, not of the
// order workers finished.
func (s *BatchSink) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	for _, f := range facts {
		if err := put(s, ctx, &s.nodes, f, NodeFactBytes(f)); err != nil {
			return err
		}
	}
	return nil
}

// PutRelations accepts relation facts.
func (s *BatchSink) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	for _, f := range facts {
		if err := put(s, ctx, &s.relations, f, RelationFactBytes(f)); err != nil {
			return err
		}
	}
	return nil
}

// PutAliases accepts native aliases.
func (s *BatchSink) PutAliases(ctx context.Context, aliases []model.NativeAlias) error {
	for _, a := range aliases {
		if err := put(s, ctx, &s.aliases, a, AliasBytes(a)); err != nil {
			return err
		}
	}
	return nil
}

// PutSearchUnits accepts search documents.
func (s *BatchSink) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	for _, d := range docs {
		if err := put(s, ctx, &s.search, d, SearchUnitBytes(d)); err != nil {
			return err
		}
	}
	return nil
}

// put admits one record: it rejects an oversize record before anything is
// queued, flushes the batch that would overflow, reserves the record's bytes
// (flushing its own queued batches or waiting on the pool without holding
// the sink lock) and only then appends.
func put[T any](s *BatchSink, ctx context.Context, b *batch[T], item T, size int64) error {
	if size > s.limits.MaxRecordBytes {
		return s.overLimit(size, "max_provider_record_bytes", s.limits.MaxRecordBytes)
	}
	for {
		s.mu.Lock()
		if s.failed != nil {
			err := s.failed
			s.mu.Unlock()
			return err
		}
		if len(b.items)+1 > s.limits.BatchRecords || b.bytes+size > s.limits.BatchBytes {
			if err := flush(s, ctx, b); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if s.pool.tryAcquire(size) {
			b.items = append(b.items, item)
			b.bytes += size
			s.records++
			s.bytes += uint64(size)
			s.mu.Unlock()
			return nil
		}
		if s.pendingLocked() > 0 {
			err := s.flushAllLocked(ctx)
			s.mu.Unlock()
			if err != nil {
				return err
			}
			continue
		}
		s.mu.Unlock()
		if err := s.pool.wait(ctx); err != nil {
			return err
		}
	}
}

// Flush persists every queued batch in reference order: nodes first, because
// relation, alias and search rows reference node identities the same unit may
// have minted; then relations, aliases and search documents.
func (s *BatchSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return s.failed
	}
	return s.flushAllLocked(ctx)
}

func (s *BatchSink) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func (s *BatchSink) pendingLocked() int64 {
	return s.nodes.bytes + s.relations.bytes + s.aliases.bytes + s.search.bytes
}

func (s *BatchSink) flushIfPending(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return false, s.failed
	}
	if s.pendingLocked() == 0 {
		return false, nil
	}
	return true, s.flushAllLocked(ctx)
}

func (s *BatchSink) flushAllLocked(ctx context.Context) error {
	if err := flush(s, ctx, &s.nodes); err != nil {
		return err
	}
	if err := flush(s, ctx, &s.relations); err != nil {
		return err
	}
	if err := flush(s, ctx, &s.aliases); err != nil {
		return err
	}
	return flush(s, ctx, &s.search)
}

// flush writes one batch under the sink lock. Any batch other than nodes
// first flushes the pending nodes, so a relation, alias or search row never
// reaches the writer before an identity it references. The pool bytes are
// released whether the write succeeded or not: on failure the records are
// discarded, the failure is latched and every producer is cancelled with it.
func flush[T any](s *BatchSink, ctx context.Context, b *batch[T]) error {
	if _, isNodes := any(b).(*batch[model.NodeFact]); !isNodes && len(s.nodes.items) > 0 {
		if err := flush(s, ctx, &s.nodes); err != nil {
			return err
		}
	}
	if len(b.items) == 0 {
		return nil
	}
	items, bytes := b.items, b.bytes
	b.items, b.bytes = nil, 0
	err := s.write(ctx, items)
	s.pool.release(bytes)
	if err != nil {
		s.fail(err)
		return err
	}
	return nil
}

// fail latches the first write failure and cancels the unit's producers.
func (s *BatchSink) fail(err error) {
	if s.failed == nil {
		s.failed = err
		s.cancel(err)
	}
}

// write sorts one batch by its stable key and hands it to the destination.
func (s *BatchSink) write(ctx context.Context, items any) error {
	switch list := items.(type) {
	case []model.NodeFact:
		slices.SortFunc(list, func(a, b model.NodeFact) int { return cmp.Compare(a.Node.ID, b.Node.ID) })
		return s.dst.PutNodes(ctx, list)
	case []model.RelationFact:
		slices.SortFunc(list, func(a, b model.RelationFact) int { return cmp.Compare(a.Relation.ID, b.Relation.ID) })
		return s.dst.PutRelations(ctx, list)
	case []model.NativeAlias:
		slices.SortFunc(list, func(a, b model.NativeAlias) int {
			return cmp.Or(cmp.Compare(a.ScopeKey, b.ScopeKey), cmp.Compare(a.NativeKey, b.NativeKey), cmp.Compare(a.NodeID, b.NodeID))
		})
		return s.dst.PutAliases(ctx, list)
	case []model.SearchUnit:
		slices.SortFunc(list, func(a, b model.SearchUnit) int { return cmp.Compare(a.ID, b.ID) })
		return s.dst.PutSearchUnits(ctx, list)
	}
	return &model.Error{Code: model.CodeInternal, Message: "batch sink received an unknown record type"}
}

// overLimit is the typed single-record refusal: Details name the limit broken
// so the diagnosis is not "batch too large" but which bound and by how much.
func (s *BatchSink) overLimit(size int64, limit string, bound int64) error {
	return resourceLimit(fmt.Sprintf("a single record of %d bytes exceeds %s (%d); it cannot be admitted in any batch", size, limit, bound)).
		WithDetail("limit", limit).WithDetail("record_bytes", strconv.FormatInt(size, 10)).WithDetail("limit_bytes", strconv.FormatInt(bound, 10))
}

// canceled types a context end the way internal/process does: errors.Is still
// sees the context error and errors.As finds the CTX_CANCELED code, so the CLI
// never reports a deliberate stop as an invalid command.
func canceled(err error) error {
	typed := &model.Error{Code: model.CodeCanceled, Message: "the unit was canceled before its output was persisted"}
	if errors.Is(err, context.DeadlineExceeded) {
		typed.Message = "the deadline expired before the unit's output was persisted"
	}
	return errors.Join(typed, err)
}
