// Package sqlite is the one store of Section 12: it owns SQL, unit publication,
// generation pinning, retention metadata and transactions. It imports only
// internal/model and the pinned driver; no provider, CLI or config package.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/diskfree"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Options are the storage settings of Section 20.1 `[storage]` plus the
// `[index]` batch bounds the store enforces on unit writes. For the numeric
// settings zero selects the documented default; no value means unlimited.
type Options struct {
	BusyTimeout     time.Duration // busy_timeout, default 5s
	ReadConnections int           // read_connections, default 2
	WriterCacheKiB  int           // writer_cache_kib, default 1 GiB; also the ingestion group bound
	ReaderCacheKiB  int           // reader_cache_kib, default 4096
	BatchRecords    int           // index.batch_records, default 1000
	BatchBytes      int64         // index.batch_bytes, default 4 MiB
	// MaxJSONBytes bounds every stored JSON column that has no tighter model
	// ceiling (manifest requests, capsules). Default 8 MiB, the Section 20.1
	// context.max_manifest_bytes / max_capsule_bytes default.
	MaxJSONBytes int64
	// MaxEvidencePerFact is the effective per-fact evidence clip the providers
	// of this process emitted under: index.max_evidence_per_fact when the
	// operator set one, otherwise the model's record ceiling. Seal enforces it
	// over the union of fresh and carried occurrences, so a delta cannot leave
	// a fact holding more than the run's providers were allowed to publish.
	// Zero selects the record ceiling, which is what unlimited means here.
	MaxEvidencePerFact int
	// Synchronous is the synchronous mode of the WRITER connection:
	// "normal" (the default, and what config.SynchronousNormal spells) or
	// "full". Readers are pinned to FULL regardless -- they are query_only
	// and never write, so the mode is behaviourally inert for them, and
	// pinning it keeps every pooled connection's pragma set verified.
	// An empty value selects the default; an unrecognized value fails Open.
	// See docs/adr/ADR-0004-wal-synchronous-mode.md.
	Synchronous string
}

// synchronousPragma pairs the value the DSN applies with the value reading
// `PRAGMA synchronous` back must report, so the two halves cannot drift.
// The unknown case fails closed: the store never picks a durability mode the
// operator did not ask for.
//
// The spellings are the configuration package's own constants rather than
// literals repeated here, so the accepted set cannot drift from the validator
// that refuses everything outside it (internal/config/validate.go). A comment
// claiming the two sets match enforced nothing; sharing the constants does.
func synchronousPragma(mode string) (pragma, error) {
	switch mode {
	case config.SynchronousNormal:
		return pragma{"synchronous", "NORMAL", "1"}, nil
	case config.SynchronousFull:
		return pragma{"synchronous", "FULL", "2"}, nil
	}
	return pragma{}, invalid("storage.synchronous is %q; use %q or %q",
		mode, config.SynchronousNormal, config.SynchronousFull)
}

// SynchronousMode reads `PRAGMA synchronous` back from the WRITER connection
// and reports it in the configuration's own spelling. It is the writer's
// because the writer is the only connection whose mode is a choice: readers are
// pinned to FULL above, so asking a reader would report FULL for every
// workspace and say nothing about the durability the operator configured.
//
// It exists so a diagnostic can report the LIVE mode rather than echo the
// configured one. The two cannot drift while a store is open -- every pooled
// connection has its pragma set read back before it joins the pool -- but a
// check that reports what the configuration says has verified nothing, and
// ADR-0004 Decision 1a asks an operator to be able to read this mode back.
//
// A mode the mapping does not know is reported as the raw pragma value rather
// than guessed at or failed: this is a diagnostic read, and an unexpected
// number is itself the answer.
func (s *Store) SynchronousMode(ctx context.Context) (string, error) {
	var raw string
	if err := s.writerQueryRow(ctx, `PRAGMA synchronous`, &raw); err != nil {
		return "", wrap("read the synchronous mode", err)
	}
	switch raw {
	case "0":
		return "off", nil
	case "1":
		return config.SynchronousNormal, nil
	case "2":
		return config.SynchronousFull, nil
	case "3":
		return "extra", nil
	}
	return raw, nil
}

func (o Options) withDefaults() Options {
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.ReadConnections <= 0 {
		o.ReadConnections = 2
	}
	if o.WriterCacheKiB <= 0 {
		o.WriterCacheKiB = defaultWriterCacheKiB
	}
	if o.ReaderCacheKiB <= 0 {
		o.ReaderCacheKiB = 4096
	}
	if o.BatchRecords <= 0 {
		o.BatchRecords = 1000
	}
	if o.BatchBytes <= 0 {
		o.BatchBytes = 4 << 20
	}
	if o.MaxJSONBytes <= 0 {
		o.MaxJSONBytes = 8 << 20
	}
	if o.MaxEvidencePerFact <= 0 {
		o.MaxEvidencePerFact = model.MaxEvidencePerFact
	}
	if o.Synchronous == "" {
		o.Synchronous = config.SynchronousNormal
	}
	return o
}

// Store is one workspace database: a single writer connection, a small reader
// pool and a private in-memory connection for query tokenization.
type Store struct {
	path      string
	freeBytes func(dir string) (uint64, bool) // the disk measurement a refused write is settled against
	opts      Options
	writer    *sql.DB
	readers   *sql.DB
	postings  *sql.DB
	tokenizer *sql.DB

	// The ingestion group: one write transaction that every ingestion call
	// joins, so a run commits when the writer's page cache would spill, when
	// another writer needs the database, at activation, and at close -- never
	// per batch. groupMu guards group and every statement issued on it.
	// writerMu is held by whichever holds the single writer connection: an
	// open group, or an exclusive write for its duration. Lock order is
	// groupMu then writerMu, and neither is held across a call that takes the
	// other in the opposite order. writerWanted counts exclusive writers
	// waiting for the connection; an ingestion call that ends with one waiting
	// commits the group before returning.
	groupMu      sync.Mutex
	group        *sql.Tx
	writerMu     sync.Mutex
	writerWanted atomic.Int32
	// groupLog is the write-ahead log as it was when the open group began.
	// Nothing reaches the log while a group's dirty pages fit the writer's
	// page cache, so a log that differs from this is one the cache has
	// started spilling into.
	groupLog logMark
	// Version is the embedded engine's sqlite_version() as observed at open.
	Version string
	// SourceID is sqlite_source_id() as observed at open.
	SourceID string
}

// timeLayout is fixed width so stored timestamps compare correctly as text.
// RFC3339Nano trims trailing zeros and would misorder "..00Z" after "..00.5Z".
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, corrupt("stored timestamp %q is not readable", s)
	}
	return t, nil
}

// Open opens or initializes the database at path (its parent directory is
// created 0700). It verifies the embedded engine version, applies and checks
// the Section 12.1 pragmas on every pooled connection, and fails closed with
// CTX_SCHEMA_MISMATCH on a foreign schema. A rebuild is a different path chosen
// by the caller; this function never alters an incompatible database.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if engineConfigErr != nil {
		return nil, engineConfigErr
	}
	opts = opts.withDefaults()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, internal("database path: " + err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, internal("database directory: " + err.Error())
	}
	s := &Store{path: abs, opts: opts, freeBytes: diskfree.Available}
	busy := strconv.FormatInt(opts.BusyTimeout.Milliseconds(), 10)
	common := []pragma{
		{"busy_timeout", busy, busy},
		{"foreign_keys", "ON", "1"},
		{"journal_mode", "WAL", "wal"},
		{"temp_store", "FILE", "1"},
		{"mmap_size", "0", "0"},
	}
	// The writer truncates the log whenever the engine resets it, so the
	// log's size tells the store whether a group has started spilling (a
	// group that never spills writes nothing to the log until it commits) and
	// a run never leaves a log the size of its largest group on disk.
	const journalSizeLimit = "0"
	// The writer is the only connection whose synchronous mode is a choice,
	// because it is the only connection that commits. Readers keep FULL.
	writerSync, err := synchronousPragma(opts.Synchronous)
	if err != nil {
		return nil, err
	}
	writerPragmas := slices.Concat(common, []pragma{
		writerSync,
		{"journal_size_limit", journalSizeLimit, journalSizeLimit},
		{"cache_size", "-" + strconv.Itoa(opts.WriterCacheKiB), "-" + strconv.Itoa(opts.WriterCacheKiB)}})
	readerPragmas := slices.Concat(common, []pragma{
		{"synchronous", "FULL", "2"},
		{"cache_size", "-" + strconv.Itoa(opts.ReaderCacheKiB), "-" + strconv.Itoa(opts.ReaderCacheKiB)},
		{"query_only", "ON", "1"}})

	s.writer, err = openPool(abs, writerPragmas, "immediate", 1)
	if err != nil {
		return nil, err
	}
	if err := s.checkEngine(ctx); err != nil {
		s.writer.Close()
		return nil, err
	}
	if err := s.initSchema(ctx); err != nil {
		s.writer.Close()
		return nil, err
	}
	s.readers, err = openPool(abs, readerPragmas, "deferred", opts.ReadConnections)
	if err != nil {
		s.writer.Close()
		return nil, err
	}
	// Posting streams hold one read transaction open for the whole candidate
	// walk of a query. They get their own pool so that holding one can never
	// starve the short reads (Match, SearchDocuments) the same query issues
	// while the stream is open, which on the shared reader pool would be a
	// deadlock as soon as two queries ran at once.
	s.postings, err = openPool(abs, readerPragmas, "deferred", opts.ReadConnections)
	if err != nil {
		s.readers.Close()
		s.writer.Close()
		return nil, err
	}
	// The tokenizer database holds no data; it exists so query text can be
	// split with the exact unicode61 tokenizer search_fts uses (Section 12.4).
	s.tokenizer, err = openPool("", nil, "deferred", 1)
	if err != nil {
		s.postings.Close()
		s.readers.Close()
		s.writer.Close()
		return nil, err
	}
	return s, nil
}

// pragma is one required per-connection setting: the value the DSN applies and
// the value reading it back must report.
type pragma struct {
	name, set, want string
}

func openPool(path string, pragmas []pragma, txlock string, maxConns int) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p.name+"("+p.set+")")
	}
	q.Set("_txlock", txlock)
	var dsn string
	if path == "" {
		dsn = ":memory:?" + q.Encode()
	} else {
		dsn = (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	}
	if err := pacedvfs.Register(); err != nil {
		return nil, internal(err.Error())
	}
	base, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internal("sqlite connector: " + err.Error())
	}
	db := sql.OpenDB(verifiedConnector{Connector: base, pragmas: pragmas})
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// verifiedConnector wraps the driver connector so every physical connection
// database/sql opens has its pragmas read back before it joins the pool.
// Setting a pragma on one pooled connection is insufficient (Section 12.1);
// the DSN applies them to each connection and this verifies each one.
type verifiedConnector struct {
	driver.Connector
	pragmas []pragma
}

func (c verifiedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, wrap("connect", err)
	}
	q, ok := conn.(driver.QueryerContext)
	if !ok {
		conn.Close()
		return nil, internal("sqlite connection does not support QueryContext")
	}
	for _, p := range c.pragmas {
		got, err := queryScalar(ctx, q, "PRAGMA "+p.name)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if !strings.EqualFold(got, p.want) {
			conn.Close()
			return nil, corrupt("connection pragma %s reads back %q, want %q", p.name, got, p.want)
		}
	}
	return conn, nil
}

func queryScalar(ctx context.Context, q driver.QueryerContext, query string) (string, error) {
	rows, err := q.QueryContext(ctx, query, nil)
	if err != nil {
		return "", wrap(query, err)
	}
	defer rows.Close()
	dest := make([]driver.Value, len(rows.Columns()))
	if err := rows.Next(dest); err != nil {
		return "", wrap(query, err)
	}
	if len(dest) == 0 {
		return "", corrupt("%s returned no value", query)
	}
	return fmt.Sprint(dest[0]), nil
}

// checkEngine records sqlite_version()/sqlite_source_id() and rejects an
// embedded engine older than 3.51.3 or the withdrawn 3.52.0 (Section 12.1).
// A module name does not prove the WAL-reset fix is present; the version does.
func (s *Store) checkEngine(ctx context.Context) error {
	if err := s.writer.QueryRowContext(ctx, `SELECT sqlite_version(), sqlite_source_id()`).Scan(&s.Version, &s.SourceID); err != nil {
		return wrap("sqlite_version", err)
	}
	var v [3]int
	parts := strings.Split(s.Version, ".")
	if len(parts) != 3 {
		return corrupt("embedded SQLite reports unparseable version %q", s.Version)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return corrupt("embedded SQLite reports unparseable version %q", s.Version)
		}
		v[i] = n
	}
	older := v[0] < 3 || (v[0] == 3 && (v[1] < 51 || (v[1] == 51 && v[2] < 3)))
	if older || (v[0] == 3 && v[1] == 52 && v[2] == 0) {
		return &model.Error{Code: model.CodeStorageCorrupt,
			Message:     fmt.Sprintf("embedded SQLite %s is affected by the WAL-reset defect; 3.51.3 or newer (not 3.52.0) is required", s.Version),
			Remediation: "rebuild codectx with the pinned modernc.org/sqlite release"}
	}
	return nil
}

// Close commits any open ingestion group and releases every pool. It is safe
// to call once.
func (s *Store) Close() error {
	var first error
	s.groupMu.Lock()
	if err := s.commitGroupLocked(); err != nil {
		first = err
	}
	s.groupMu.Unlock()
	for _, db := range []*sql.DB{s.tokenizer, s.postings, s.readers, s.writer} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && first == nil {
			first = wrap("close", err)
		}
	}
	return first
}

// defaultWriterCacheKiB is the writer connection's page cache, and with it
// the size of an ingestion group: the group commits the moment the cache
// would spill a dirty page to the log. Up to that point every page the group
// dirties lives in the cache, however many times it is dirtied, and the
// commit appends each one to the log exactly once; a group that spilled
// would write hot pages to the log again and again, in place, and would
// rewrite every frame's checksum at commit, so the log would no longer be an
// append-only file the disk receives as one sequential stream. A commit writes the group's
// distinct pages once to the log and the checkpoint copies them once more,
// so the bytes an index run sends to disk are the distinct pages its groups
// touch, not the rows it stores; content-addressed identities land on pages
// spread across their whole index, and a page pays for itself only when a
// group lands many rows on it, which a group the size of the index does. The
// cache is allocated as it fills, so a small run never takes the whole
// budget, and the run returns it at activation.
const defaultWriterCacheKiB = 1 << 20

// write runs fn in one immediate write transaction of its own on the single
// writer connection, committed before it returns. It is the path for state
// that must be visible to every connection the moment the call returns --
// sessions, leases, heartbeats, retention, the query planner's statistics --
// and it ends any open ingestion group first, since the group holds the
// connection. Any error rolls back; the caller sees the first typed failure.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.writerWanted.Add(1)
	defer s.writerWanted.Add(-1)
	s.groupMu.Lock()
	err := s.commitGroupLocked()
	s.groupMu.Unlock()
	if err != nil {
		return err
	}
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return wrap("begin", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return wrap("commit", err)
	}
	return nil
}

// ingest runs fn inside the ingestion group, opening the group when none is
// open. fn is atomic on its own -- it runs inside a savepoint, so a refused
// batch rolls back alone and the units already in the group are kept -- and
// the group commits when the writer's page cache would spill or an exclusive
// writer is waiting. The group's transaction is begun under a context that
// outlives any one caller: a caller whose context ends loses its own
// statement, never the run.
func (s *Store) ingest(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.attribute(s.ingestGroup(ctx, false, fn))
}

// ingestAndCommit is ingest followed by the group's commit, whether fn
// succeeded or was rolled back, for the calls whose outcome must be durable
// when they return: a generation's activation or abort.
func (s *Store) ingestAndCommit(ctx context.Context, fn func(tx *sql.Tx) error) error {
	err := s.attribute(s.ingestGroup(ctx, true, fn))
	// The run is over: the writer's page cache, which the run filled with
	// its groups, goes back to the process so a long-lived server does not
	// keep a run's working set resident.
	s.writerMu.Lock()
	_, _ = s.writer.ExecContext(context.WithoutCancel(ctx), `PRAGMA shrink_memory`)
	s.writerMu.Unlock()
	return err
}

func (s *Store) ingestGroup(ctx context.Context, commit bool, fn func(tx *sql.Tx) error) error {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	if s.group == nil {
		s.writerMu.Lock()
		tx, err := s.writer.BeginTx(context.WithoutCancel(ctx), nil)
		if err != nil {
			s.writerMu.Unlock()
			return wrap("begin", err)
		}
		s.group = tx
		s.groupLog = s.logMark()
	}
	tx := s.group
	if _, err := tx.ExecContext(ctx, `SAVEPOINT ingest`); err != nil {
		return wrap("savepoint", err)
	}
	if err := fn(tx); err != nil {
		if _, rbErr := tx.ExecContext(context.WithoutCancel(ctx), `ROLLBACK TO ingest`); rbErr != nil {
			// The savepoint could not be unwound: the group is no longer a
			// state the run can build on, so it ends here.
			s.abandonGroupLocked()
			return errors.Join(err, wrap("rollback to savepoint", rbErr))
		}
		if _, relErr := tx.ExecContext(context.WithoutCancel(ctx), `RELEASE ingest`); relErr != nil {
			s.abandonGroupLocked()
			return errors.Join(err, wrap("release savepoint", relErr))
		}
		if commitErr := s.commitGroupIfDueLocked(commit); commitErr != nil {
			return errors.Join(err, commitErr)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE ingest`); err != nil {
		s.abandonGroupLocked()
		return wrap("release savepoint", err)
	}
	return s.commitGroupIfDueLocked(commit)
}

// commitGroupIfDueLocked commits the open group when the caller asks for it,
// when an exclusive writer is waiting, or when the writer's page cache has
// begun spilling to the log. groupMu is held.
func (s *Store) commitGroupIfDueLocked(force bool) error {
	if s.group == nil {
		return nil
	}
	if !force && s.writerWanted.Load() == 0 && s.logMark() == s.groupLog {
		return nil
	}
	return s.commitGroupLocked()
}

// logMark identifies the state of the write-ahead log by its size and its
// header. A group's first frame either restarts the log, which rewrites the
// header with a new salt and checkpoint sequence while the file keeps its
// old length until the commit truncates it, or, when a reader still holds the
// old frames, appends past them and grows the file; one of the two changes
// either way, and neither changes while no frame is written.
type logMark struct {
	size   int64
	header [32]byte
}

func (s *Store) logMark() logMark {
	var m logMark
	f, err := os.Open(s.path + "-wal")
	if err != nil {
		m.size = -1
		return m
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		m.size = st.Size()
	}
	_, _ = f.ReadAt(m.header[:], 0)
	return m
}

// commitGroupLocked commits the open group, if any, folds its log into the
// database, and releases the writer connection. groupMu is held.
func (s *Store) commitGroupLocked() error {
	if s.group == nil {
		return nil
	}
	tx := s.group
	s.group = nil
	err := tx.Commit()
	if err == nil {
		// The engine checkpoints on its own once the log holds a thousand
		// frames; a group smaller than that would otherwise leave its frames
		// for the next group to stack on. A checkpoint that finds a reader on
		// the log folds what it can and is not a failure of the commit.
		_, _ = s.writer.ExecContext(context.Background(), `PRAGMA wal_checkpoint(PASSIVE)`)
	}
	s.writerMu.Unlock()
	if err != nil {
		return s.attribute(wrap("commit", err))
	}
	return nil
}

// abandonGroupLocked rolls the open group back and releases the writer
// connection. groupMu is held.
func (s *Store) abandonGroupLocked() {
	if s.group == nil {
		return
	}
	tx := s.group
	s.group = nil
	tx.Rollback()
	s.writerMu.Unlock()
}

// WALBoundBytes is the largest write-ahead log an ingestion group leaves
// behind: the writer's page cache, which the group fills before it commits,
// plus one batch of spilled pages. The engine truncates the log when it resets
// it, so a log past this bound with no run open is one a reader kept open
// across several groups, or one that was never checkpointed.
func (s *Store) WALBoundBytes() int64 { return 2 * int64(s.opts.WriterCacheKiB) << 10 }

// Flush commits the open ingestion group, if any. Nothing in the indexing path
// calls it: a run's work reaches the disk with its publication, and Close
// commits whatever a process leaves open. It exists for a caller that opens a
// connection of its own -- a test or a tool reading the database file beside
// the store -- and must see what the store has written so far.
func (s *Store) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return wrap("flush", err)
	}
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	return s.commitGroupLocked()
}

// readOwn runs fn where it sees the ingestion group's own writes: on the
// group's transaction while one is open, and on the reader pool otherwise.
// It is the read path of the ingestion side -- unit states, aliases of
// dependency units, the snapshot and blobs a capture just recorded -- whose
// callers reason about what this run has already stored. Query paths read
// through read and never see a group in progress. It holds the group for the
// length of fn, so it is never called from inside an ingestion call: a
// producer stream an ingestion call drains reads through the pool instead
// (UnitInputs), and sees the last commit.
func (s *Store) readOwn(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.groupMu.Lock()
	if s.group != nil {
		defer s.groupMu.Unlock()
		return s.attribute(fn(s.group))
	}
	s.groupMu.Unlock()
	return s.read(ctx, fn)
}

// writerQueryRow issues one row query on the writer connection, through the
// open group when there is one so the probe never waits on the run.
func (s *Store) writerQueryRow(ctx context.Context, query string, dest ...any) error {
	s.groupMu.Lock()
	if s.group != nil {
		defer s.groupMu.Unlock()
		return s.group.QueryRowContext(ctx, query).Scan(dest...)
	}
	s.groupMu.Unlock()
	return s.writer.QueryRowContext(ctx, query).Scan(dest...)
}

// read runs fn in one short deferred read transaction on the reader pool, so
// every statement inside it sees one consistent snapshot of the database.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.attribute(wrap("begin read", err))
	}
	defer tx.Rollback()
	return s.attribute(fn(tx))
}

// exec1 runs a statement that must affect exactly one row.
func exec1(ctx context.Context, tx *sql.Tx, conflict *model.Error, query string, args ...any) error {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return wrap(query, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrap(query, err)
	}
	if n != 1 {
		return conflict
	}
	return nil
}

// Error helpers. Every failure leaving this package is a *model.Error.

func internal(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}

func corrupt(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeStorageCorrupt, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func conflict(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeVersionConflict, Message: fmt.Sprintf(format, args...)}
}

// wrap maps a driver error to its Section 22 family. A typed error passes
// through; a context cancellation is returned unchanged so callers can
// distinguish it from a crash.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		if refusedWrite(se.Code()) {
			return writeRefused(op, se.Code(), se.Error())
		}
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return &model.Error{Code: model.CodeWorkspaceBusy, Message: "database is busy: " + op, Retryable: true}
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return &model.Error{Code: model.CodeStorageCorrupt, Message: "database is not readable: " + se.Error(),
				Remediation: "run codectx doctor --deep; rebuild the cache with index --rebuild if it reports corruption"}
		case sqlite3.SQLITE_CONSTRAINT:
			return &model.Error{Code: model.CodeArgumentInvalid, Message: "rejected by schema constraint: " + se.Error()}
		}
	}
	return internal(op + ": " + err.Error())
}

// refusedWrite reports whether an extended result code says the filesystem
// refused to grow or persist the database, its log or its shared-memory
// index. SQLITE_FULL is the obvious one, but it is not the one a full disk
// usually produces: growing the -shm file raises SQLITE_IOERR_SHMSIZE (4874)
// and growing the database or the log raises the write/sync/truncate members
// of the same family, whose PRIMARY code is SQLITE_IOERR (10) -- so a switch
// on `code & 0xff` would see only SQLITE_IOERR and report CTX_INTERNAL for a
// disk that is simply full (proved on a filled tmpfs: connect failed with
// 4874).
//
// The full extended code is therefore tested, and only the members a space
// exhaustion actually raises are listed: SQLITE_IOERR_READ, _CORRUPTFS, _DATA
// and the locking members keep falling through to their own families, because
// a read failure or a corrupt filesystem is not a full one. The same members
// are also what a failing device raises, which is why the store settles the
// two by measuring the disk (see Store.attribute) rather than by the code.
func refusedWrite(code int) bool {
	switch code {
	case sqlite3.SQLITE_FULL,
		sqlite3.SQLITE_IOERR_WRITE,
		sqlite3.SQLITE_IOERR_FSYNC,
		sqlite3.SQLITE_IOERR_DIR_FSYNC,
		sqlite3.SQLITE_IOERR_TRUNCATE,
		sqlite3.SQLITE_IOERR_SHMSIZE,
		sqlite3.SQLITE_IOERR_SHMMAP:
		return true
	}
	// SQLITE_FULL carries extended members of its own (SQLITE_FULL is 13; the
	// primary code is what a non-extended build reports), so the primary code
	// is tested too rather than only the exact constant.
	return code&0xff == sqlite3.SQLITE_FULL
}

// writeRefused is the error for a write the filesystem refused, before the
// store has measured the disk. It carries the engine's own message and code
// so that whichever family it settles into, the reader sees what the engine
// saw and never a cause the store guessed.
func writeRefused(op string, code int, engineMessage string) *model.Error {
	return &model.Error{Code: model.CodeDiskFull,
		Message: fmt.Sprintf("%s: the filesystem refused the write: %s (engine code %d)", op, engineMessage, code),
		Details: map[string]string{detailEngineCode: strconv.Itoa(code), detailEngineMessage: engineMessage}}
}

// detailEngineCode and detailEngineMessage carry the engine's result on a
// refused write; their presence is what marks the error as not yet settled
// against the disk.
const (
	detailEngineCode    = "engine_code"
	detailEngineMessage = "engine_message"
	detailFreeBytes     = "free_bytes"
)

// attribute settles a refused write against the disk. The engine reports a
// full disk and a failing device with the same result codes, so the store
// measures the space free under the database at the moment of the failure
// and says which it saw: with less than one ingestion group free the refusal
// is a full disk's and the error stays CTX_DISK_FULL, naming the figure; with
// more, the disk is not full, the device or its driver refused the operation,
// and the error becomes CTX_INTERNAL carrying the engine's message and code
// and the figure that rules a full disk out. A disk the platform cannot
// measure keeps CTX_DISK_FULL and says the measurement was unavailable.
// Every other error passes through unchanged.
func (s *Store) attribute(err error) error {
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeDiskFull || typed.Details[detailEngineCode] == "" {
		return err
	}
	dir := filepath.Dir(s.path)
	free, ok := s.freeBytes(dir)
	settled := &model.Error{Code: typed.Code, Message: typed.Message, Details: map[string]string{}}
	for k, v := range typed.Details {
		settled.Details[k] = v
	}
	switch {
	case !ok:
		settled.Message += "; the free space under " + dir + " could not be measured"
		settled.Remediation = "check the free space and the device under " + dir
	case free < uint64(s.opts.WriterCacheKiB)<<10:
		settled.Details[detailFreeBytes] = strconv.FormatUint(free, 10)
		settled.Message += fmt.Sprintf("; %d bytes are free under %s, less than one ingestion group", free, dir)
		settled.Remediation = "free space under " + dir
	default:
		settled.Code = model.CodeInternal
		settled.Details[detailFreeBytes] = strconv.FormatUint(free, 10)
		settled.Message += fmt.Sprintf("; %d bytes are free under %s, so the disk is not full: the device or its driver refused the operation", free, dir)
		settled.Remediation = "check the kernel log for the device holding " + dir
	}
	return settled
}

// idBlob decodes a public hex identifier to the 32 bytes SQLite stores.
func idBlob(field, id string) ([]byte, error) {
	raw, err := model.DecodeID(id)
	if err != nil {
		return nil, invalid("%s: %v", field, err)
	}
	return raw, nil
}

// optionalBlob decodes an identifier that may be absent, yielding NULL.
func optionalBlob(field, id string) (any, error) {
	if id == "" {
		return nil, nil
	}
	return idBlob(field, id)
}

func idHex(raw []byte) string { return hex.EncodeToString(raw) }

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
