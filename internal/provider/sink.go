package provider

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Limits are the per-batch bounds of Section 11.1: a batch is capped by both
// records and retained bytes, and flushes at whichever is reached first.
// MaxRecordBytes is resources.max_provider_record_bytes, and zero is
// unlimited: no shipped value refuses a record for being large. A record over
// a limit the USER set is admitted and REPORTED -- the sink counts it as a
// degradation the unit's capability rows carry -- because a size key names
// what the answer should tell the caller about, not what it should refuse.
// Nothing here is what bounds memory: the batch reservations and the pool are.
type Limits struct {
	BatchRecords   int
	BatchBytes     int64
	MaxRecordBytes int64
}

// Validate rejects a configuration that would mean unlimited or a record
// bound no batch could hold (config.validate enforces the same relation on
// the configured values). A bad bound is a configuration rejection, not
// resource exhaustion.
func (l Limits) Validate() error {
	// BatchRecords and BatchBytes size the machine rather than the repository:
	// they are reservations, and a zero reservation is a configuration error.
	// MaxRecordBytes is a repository bound, so zero means unlimited there.
	if l.BatchRecords <= 0 || l.BatchBytes <= 0 || l.MaxRecordBytes < 0 {
		return invalid(fmt.Sprintf("sink limits are %d records, %d batch bytes and %d record bytes; the batch reservations must be positive and the record bound may not be negative",
			l.BatchRecords, l.BatchBytes, l.MaxRecordBytes))
	}
	if l.MaxRecordBytes > 0 && l.MaxRecordBytes > l.BatchBytes {
		return invalid(fmt.Sprintf("max record bytes %d exceed batch bytes %d; a reporting threshold above the batch reservation could never be reached before the batch flushed", l.MaxRecordBytes, l.BatchBytes))
	}
	return nil
}

// MaxLiveSinks bounds the sinks one pool tracks at a time. The coordinator
// runs far fewer units concurrently; the bound exists so the registry is
// finite, not to size it.
const MaxLiveSinks = 1024

// Pool is the retained-byte reservation shared by every sink of one indexing
// run (index.queue_bytes). Queued records and outstanding decode reservations
// are both charged against it, so the bytes providers hold before persistence
// are bounded across concurrent units, not only per batch.
//
// The pool also tracks its live sinks. An acquirer that cannot be charged
// first asks every live sink, its own included, to persist what it has
// queued, so one sink that has stopped producing cannot pin the pool with a
// half-filled batch while another starves.
type Pool struct {
	capacity int64

	mu      sync.Mutex
	used    int64
	changed chan struct{}
	sinks   []*BatchSink
}

// NewPool bounds the pool at capacity bytes.
func NewPool(capacity int64) (*Pool, error) {
	if capacity <= 0 {
		return nil, invalid("the sink byte pool capacity must be positive")
	}
	return &Pool{capacity: capacity, changed: make(chan struct{})}, nil
}

// tryCharge charges n without blocking.
func (p *Pool) tryCharge(n int64) bool {
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
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.used -= n
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
}

// acquire charges n, blocking until it fits or ctx ends. The charge attempt
// and the subscription to the next release happen under one hold of p.mu, so
// a release between the two cannot be missed. Before waiting, the acquirer
// relieves pressure by flushing every live sink's queued batches; if that
// released anything it retries at once. Nothing is held while blocked.
func (p *Pool) acquire(ctx context.Context, n int64) error {
	// With max_provider_record_bytes unlimited, nothing upstream refuses a
	// record larger than the whole pool, and such a record can never fit
	// however much every other sink releases. Waiting for it would wedge the
	// unit with no diagnosis, so it is refused here, naming the reservation
	// that is too small rather than a repository bound the user did not set.
	if n > p.capacity {
		return resourceLimit(fmt.Sprintf("a single record of %d bytes does not fit the %d-byte sink pool (index.queue_bytes)", n, p.capacity))
	}
	for {
		if err := ctx.Err(); err != nil {
			// Checked before relieving: relief may issue one write per live
			// sink, and a canceled acquirer must not perform any of them.
			return model.Canceled(err)
		}
		p.mu.Lock()
		if p.used+n <= p.capacity {
			p.used += n
			p.mu.Unlock()
			return nil
		}
		ch := p.changed
		sinks := slices.Clone(p.sinks)
		p.mu.Unlock()

		relieved := false
		for _, s := range sinks {
			if s.relieve() {
				relieved = true
			}
		}
		if relieved {
			continue
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return model.Canceled(ctx.Err())
		}
	}
}

func (p *Pool) register(s *BatchSink) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sinks) >= MaxLiveSinks {
		return resourceLimit(fmt.Sprintf("the pool already tracks %d live sinks", MaxLiveSinks))
	}
	p.sinks = append(p.sinks, s)
	return nil
}

func (p *Pool) unregister(s *BatchSink) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sinks = slices.DeleteFunc(p.sinks, func(x *BatchSink) bool { return x == s })
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

// NodeFactBytes is the retained-byte estimate of one node fact: every string
// of the node, the canonical key and the evidence.
func NodeFactBytes(f model.NodeFact) int64 {
	n := f.Node
	return recordOverhead + int64(len(n.ID)+len(n.Kind)+len(n.Language)+len(n.Name)+len(n.QualifiedName)+len(n.Signature)+
		len(n.FileID)+len(n.ContentHash)+len(n.Metadata)+len(n.SemanticSource)+len(f.CanonicalKey)) + evidenceBytes(f.Evidence)
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

// DeltaSink is the optional extension of Sink for a provider that imports
// incrementally (Section 11.4). Its destination records, alongside each fact,
// every one of the producer's own id-independent keys — the keys the
// provider's next run classifies as changed, unchanged or removed. Storage
// needs them because a removed fact cannot be named any other way: a fact
// identity is derived from the resolved entities, which an edit changes, and a
// cross-file edge can disappear while every file holding its evidence is
// untouched.
//
// keys is either empty, meaning the batch carries no keys, or parallel to
// facts: keys[i] is every key backing facts[i] — at least one, each a
// lowercase hex digest, sorted and without duplicates. One fact is backed by
// several keys whenever the producer's keys are finer than the identities they
// resolve to, which is ordinary: a canonical edge is published once and
// derived from N occurrences, each with its own key. The destination drops the
// fact when any of them is replaced, which is the condition the producer
// re-emits the whole fact on.
//
// A destination that does not implement DeltaSink cannot serve a keyed batch;
// the sink refuses it rather than dropping the keys, because a unit written
// without them silently cannot be delta-refreshed.
type DeltaSink interface {
	Sink
	PutKeyedNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error
	PutKeyedRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error
}

// keyed pairs one queued fact with the producer's delta keys, nil when the
// producer supplied none. Facts are queued keyed so a batch sorts, flushes and
// is accounted for exactly once whether or not the provider is incremental.
type keyed[T any] struct {
	fact T
	keys []string
}

// Degradation is the sink's one degradation surface: a bound a unit's records
// exceeded, counted, so that exceeding it is reported rather than failing the
// unit. It is the sink's equivalent of the dropped-and-counted rows the
// bundled providers already publish, and RunUnit folds it into the unit's
// capability rows as bounded details the generation stores.
type Degradation struct {
	// Limit is the configuration key whose user-set value was exceeded.
	Limit string
	// Bound is the value the user set.
	Bound int64
	// Count is how many records and reservations exceeded it.
	Count uint64
	// Largest is the largest size observed over the bound.
	Largest int64
}

// batch is one bounded, typed accumulation awaiting persistence.
type batch[T any] struct {
	items []T
	bytes int64
}

// BatchSink is the Sink handed to a provider for one unit. It owns every
// record from acceptance until the destination has persisted it: a record is
// admitted only after its bytes are charged to the pool, a batch flushes as
// soon as the next record would exceed either limit, and a failed write
// latches the sink, discards every queued batch (returning its bytes) and
// cancels every producer of the unit. Nothing here reaches the database
// except through the destination, which in production is storage's
// UnitWriter. It is safe for concurrent use by a provider's workers; writes
// to the destination are serialized. The owner must call Discard when the
// unit is finished, whatever the outcome; RunUnit does.
type BatchSink struct {
	ctx    context.Context
	dst    Sink
	limits Limits
	pool   *Pool
	cancel context.CancelCauseFunc

	mu        sync.Mutex
	nodes     batch[keyed[model.NodeFact]]
	relations batch[keyed[model.RelationFact]]
	aliases   batch[model.NativeAlias]
	search    batch[model.SearchUnit]
	failed    error
	discarded bool
	records   uint64
	bytes     uint64

	// overLimit counts the records and reservations admitted over a user-set
	// MaxRecordBytes, with the largest size seen. They are the sink's whole
	// degradation surface; see Degradations.
	overLimit        uint64
	overLimitLargest int64

	// nodesKeyed and relationsKeyed latch whether this unit's node and
	// relation facts carry producer keys. The first Put of each kind fixes it
	// for the unit; see keyedness.
	nodesKeyed     keyedness
	relationsKeyed keyedness
}

// keyedness is a unit's latched decision about one kind of fact: whether it
// carries producer delta keys. It is a property of the unit, not of a batch.
// Deciding per batch would make delta coverage depend on where a flush
// happened to land — and a flush can be forced by another sink's pressure
// through Pool.acquire — so the same import could seal a key-deltable unit or
// a unit with unkeyed rows that only a retention bucket can ever replace.
type keyedness uint8

const (
	keyednessUnset keyedness = iota
	keyednessKeyed
	keyednessUnkeyed
)

// fix latches the unit's keyed-ness for one kind of fact and refuses a Put of
// the other kind.
func (k *keyedness) fix(keyed bool, what string) error {
	want := keyednessUnkeyed
	if keyed {
		want = keyednessKeyed
	}
	if *k == keyednessUnset {
		*k = want
		return nil
	}
	if *k == want {
		return nil
	}
	return &model.Error{Code: model.CodeProviderOutputInvalid,
		Message:     "a unit's " + what + " are either all keyed or all unkeyed; this unit already published the other kind",
		Remediation: "publish the producer's fact keys for every fact of the unit, or for none"}
}

// NewBatchSink binds a sink to its destination, limits and shared pool and
// registers it with the pool. ctx is the unit's run context; it is the
// context under which another acquirer may flush this sink's queued batches
// to relieve pool pressure. cancel is invoked with the write error when a
// required write fails, so every goroutine producing into this unit observes
// cancellation.
func NewBatchSink(ctx context.Context, dst Sink, limits Limits, pool *Pool, cancel context.CancelCauseFunc) (*BatchSink, error) {
	if ctx == nil || dst == nil || pool == nil || cancel == nil {
		return nil, invalid("a batch sink needs a context, a destination, a pool and a cancel function")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if limits.BatchBytes > pool.capacity {
		return nil, invalid(fmt.Sprintf("batch bytes %d exceed the pool capacity %d; a full batch could never be admitted", limits.BatchBytes, pool.capacity))
	}
	s := &BatchSink{ctx: ctx, dst: dst, limits: limits, pool: pool, cancel: cancel}
	if err := pool.register(s); err != nil {
		return nil, err
	}
	return s, nil
}

// Records and Bytes report what the sink accepted so far, for the provider's
// run counters.
func (s *BatchSink) Records() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.records }
func (s *BatchSink) Bytes() uint64   { s.mu.Lock(); defer s.mu.Unlock(); return s.bytes }

// noteOverLimit counts one record or reservation over the user-set record
// bound. It takes the sink lock and must not be called while it is held.
func (s *BatchSink) noteOverLimit(size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overLimit++
	if size > s.overLimitLargest {
		s.overLimitLargest = size
	}
}

// Degradations reports every bound this unit's records exceeded. It is empty
// for a unit that stayed within the bounds the user set, which is every unit
// under the shipped defaults, because no default bounds a record's size.
func (s *BatchSink) Degradations() []Degradation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.overLimit == 0 {
		return nil
	}
	return []Degradation{{
		Limit:   "max_provider_record_bytes",
		Bound:   s.limits.MaxRecordBytes,
		Count:   s.overLimit,
		Largest: s.overLimitLargest,
	}}
}

// Reserve charges n bytes against the pool before a provider decodes or
// buffers input of that size, blocking until the reservation fits and
// returning promptly on cancellation. The returned release must be called
// exactly once. A reservation over a user-set MaxRecordBytes is counted as a
// degradation and then made: the pool is what bounds what the run holds, and
// refusing here would fail the unit over a bound the user set to be told
// about. A reservation larger than the whole pool is still refused by
// Pool.acquire, which names the reservation that is too small.
func (s *BatchSink) Reserve(ctx context.Context, n int64) (release func(), err error) {
	if n <= 0 {
		return nil, invalid("a reservation must be positive")
	}
	if s.limits.MaxRecordBytes > 0 && n > s.limits.MaxRecordBytes {
		s.noteOverLimit(n)
	}
	if err := s.failure(); err != nil {
		return nil, err
	}
	if err := s.pool.acquire(ctx, n); err != nil {
		return nil, err
	}
	if err := s.failure(); err != nil {
		s.pool.release(n)
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { s.pool.release(n) }) }, nil
}

// PutNodes accepts node facts. Batches are sorted by node ID before they are
// written so the transaction shape is a function of content, not of the
// order workers finished.
func (s *BatchSink) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	return s.PutKeyedNodes(ctx, facts, nil)
}

// PutKeyedNodes accepts node facts with the producer's delta keys.
func (s *BatchSink) PutKeyedNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error {
	if err := checkKeys(len(facts), keys); err != nil {
		return err
	}
	if len(facts) == 0 {
		return nil
	}
	if err := s.fixKeyedness(&s.nodesKeyed, len(keys) != 0, "node facts"); err != nil {
		return err
	}
	for i, f := range facts {
		k := keysAt(keys, i)
		if err := put(s, ctx, &s.nodes, keyed[model.NodeFact]{fact: f, keys: k}, NodeFactBytes(f)+keyBytes(k)); err != nil {
			return err
		}
	}
	return nil
}

// fixKeyedness latches one kind's keyed-ness under the sink lock.
func (s *BatchSink) fixKeyedness(k *keyedness, keyed bool, what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return k.fix(keyed, what)
}

// PutRelations accepts relation facts.
func (s *BatchSink) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	return s.PutKeyedRelations(ctx, facts, nil)
}

// PutKeyedRelations accepts relation facts with the producer's delta keys.
func (s *BatchSink) PutKeyedRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error {
	if err := checkKeys(len(facts), keys); err != nil {
		return err
	}
	if len(facts) == 0 {
		return nil
	}
	if err := s.fixKeyedness(&s.relationsKeyed, len(keys) != 0, "relation facts"); err != nil {
		return err
	}
	for i, f := range facts {
		k := keysAt(keys, i)
		if err := put(s, ctx, &s.relations, keyed[model.RelationFact]{fact: f, keys: k}, RelationFactBytes(f)+keyBytes(k)); err != nil {
			return err
		}
	}
	return nil
}

// checkKeys enforces that a keyed batch keys every fact with at least one
// non-empty key. A partly keyed batch would leave rows a later refresh could
// neither replace nor remove. The destination enforces the rest of the key
// contract (digest shape, ordering, no duplicates), which is where it is
// stored and therefore where it must hold.
func checkKeys(facts int, keys [][]string) error {
	if len(keys) == 0 {
		return nil
	}
	if len(keys) != facts {
		return invalid(fmt.Sprintf("a keyed batch carries %d key lists for %d facts", len(keys), facts))
	}
	for _, list := range keys {
		if len(list) == 0 {
			return invalid("a keyed batch keys every fact or none; one fact has no key")
		}
		for _, k := range list {
			if k == "" {
				return invalid("a fact key is empty")
			}
		}
	}
	return nil
}

func keysAt(keys [][]string, i int) []string {
	if i < len(keys) {
		return keys[i]
	}
	return nil
}

func keyBytes(keys []string) int64 {
	var n int64
	for _, k := range keys {
		n += int64(len(k))
	}
	return n
}

// unkey splits a sorted batch into the parallel slices the destination takes.
// keyed says whether this unit's facts of that kind carry keys, which is
// latched for the whole unit rather than inferred from the batch: inferring it
// would make a flush boundary that happened to hold only unkeyed items write
// rows no key could ever replace.
func unkey[T any](list []keyed[T], keyed bool) ([]T, [][]string) {
	facts := make([]T, len(list))
	if !keyed {
		for i, item := range list {
			facts[i] = item.fact
		}
		return facts, nil
	}
	keys := make([][]string, len(list))
	for i, item := range list {
		facts[i], keys[i] = item.fact, item.keys
	}
	return facts, keys
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

// put admits one record: it counts a record over the user-set record bound,
// flushes the batch that would overflow, charges the record's bytes (without
// holding the sink lock while blocked) and only then appends. The overflow
// check is repeated after a blocking charge because another worker may have
// appended meanwhile.
//
// An oversize record is admitted, never refused. Every record type here is
// identity-bearing -- a relation is its endpoints, an alias is its keys, and a
// node's qualified name is the key generated symbols are told apart by -- so
// cutting "the largest field" to fit would either collapse two identities into
// one or, for Node.Metadata, leave a JSON payload storage then rejects. The
// bound is therefore reported, and the batch reservations and the pool remain
// what bound the heap.
func put[T any](s *BatchSink, ctx context.Context, b *batch[T], item T, size int64) error {
	if s.limits.MaxRecordBytes > 0 && size > s.limits.MaxRecordBytes {
		s.noteOverLimit(size)
	}
	s.mu.Lock()
	if err := admit(s, ctx, b, size); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.pool.tryCharge(size) {
		appendItem(s, b, item, size)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if err := s.pool.acquire(ctx, size); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := admit(s, ctx, b, size); err != nil {
		s.pool.release(size)
		return err
	}
	appendItem(s, b, item, size)
	return nil
}

// admit checks the latch under the sink lock and flushes the batch that would
// overflow.
func admit[T any](s *BatchSink, ctx context.Context, b *batch[T], size int64) error {
	if s.failed != nil {
		return s.failed
	}
	if s.discarded {
		return invalid("the sink was discarded; the unit is finished")
	}
	if len(b.items)+1 > s.limits.BatchRecords || b.bytes+size > s.limits.BatchBytes {
		return flush(s, ctx, b)
	}
	return nil
}

// appendItem queues one charged record under the sink lock.
func appendItem[T any](s *BatchSink, b *batch[T], item T, size int64) {
	b.items = append(b.items, item)
	b.bytes += size
	s.records++
	s.bytes += uint64(size)
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

// Discard ends the sink: whatever is still queued is dropped and its bytes
// returned to the pool in one release, and the sink leaves the pool's live
// set. It is idempotent and a no-op after a successful Flush left nothing
// queued. The owner calls it on every path so a provider that erred or was
// canceled with records queued never shrinks the pool for the rest of the
// run.
func (s *BatchSink) Discard() {
	s.mu.Lock()
	freed := s.dropLocked()
	s.discarded = true
	s.mu.Unlock()
	s.pool.release(freed)
	s.pool.unregister(s)
}

// dropLocked empties every batch and returns the bytes they held.
func (s *BatchSink) dropLocked() int64 {
	freed := s.pendingLocked()
	s.nodes = batch[keyed[model.NodeFact]]{}
	s.relations = batch[keyed[model.RelationFact]]{}
	s.aliases = batch[model.NativeAlias]{}
	s.search = batch[model.SearchUnit]{}
	return freed
}

// relieve is the pool's request to persist this sink's queued batches so an
// acquirer can proceed. It runs under the sink's own lock, so it serializes
// with the sink's producers, and under the sink's own run context, so a
// failure belongs to this unit. It reports whether any bytes were returned.
func (s *BatchSink) relieve() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil || s.discarded || s.pendingLocked() == 0 {
		return false
	}
	// A failed flush releases the batch and discards the rest, so bytes are
	// returned either way.
	_ = s.flushAllLocked(s.ctx)
	return true
}

func (s *BatchSink) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func (s *BatchSink) pendingLocked() int64 {
	return s.nodes.bytes + s.relations.bytes + s.aliases.bytes + s.search.bytes
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
// reaches the writer before an identity it references. The written batch's
// bytes are released whether the write succeeded or not; on failure the
// remaining batches are dropped and released too, the failure is latched and
// every producer is cancelled with it.
func flush[T any](s *BatchSink, ctx context.Context, b *batch[T]) error {
	if _, isNodes := any(b).(*batch[keyed[model.NodeFact]]); !isNodes && len(s.nodes.items) > 0 {
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
		s.failLocked(err)
		return err
	}
	return nil
}

// failLocked latches the first write failure, returns every still-queued
// byte to the pool and cancels the unit's producers.
func (s *BatchSink) failLocked(err error) {
	if s.failed != nil {
		return
	}
	s.failed = err
	s.pool.release(s.dropLocked())
	s.cancel(err)
}

// write sorts one batch by its stable key and hands it to the destination.
func (s *BatchSink) write(ctx context.Context, items any) error {
	switch list := items.(type) {
	case []keyed[model.NodeFact]:
		slices.SortFunc(list, func(a, b keyed[model.NodeFact]) int { return cmp.Compare(a.fact.Node.ID, b.fact.Node.ID) })
		facts, keys := unkey(list, s.nodesKeyed == keyednessKeyed)
		if keys == nil {
			return s.dst.PutNodes(ctx, facts)
		}
		dst, ok := s.dst.(DeltaSink)
		if !ok {
			return keyless()
		}
		return dst.PutKeyedNodes(ctx, facts, keys)
	case []keyed[model.RelationFact]:
		slices.SortFunc(list, func(a, b keyed[model.RelationFact]) int { return cmp.Compare(a.fact.Relation.ID, b.fact.Relation.ID) })
		facts, keys := unkey(list, s.relationsKeyed == keyednessKeyed)
		if keys == nil {
			return s.dst.PutRelations(ctx, facts)
		}
		dst, ok := s.dst.(DeltaSink)
		if !ok {
			return keyless()
		}
		return dst.PutKeyedRelations(ctx, facts, keys)
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

// keyless is the refusal to write a keyed batch to a destination that cannot
// record keys. Dropping them would seal a unit that looks complete and cannot
// be delta-refreshed.
func keyless() error {
	return &model.Error{Code: model.CodeInternal,
		Message: "the sink destination does not record provider fact keys; a keyed batch cannot be persisted"}
}
