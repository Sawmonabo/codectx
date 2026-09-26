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
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/diskfree"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/scratch"
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
	// ReadOnly opens the store for a process that answers questions and
	// changes nothing. No writer connection is opened at all, so this process
	// can neither wait on the write transaction another process holds nor
	// delay it: the reader pool is `query_only` and reads a write-ahead log
	// snapshot, which never blocks on a writer. Every connection is opened
	// read-only at the FILE level besides, which is what query_only does not
	// do: see openPool, and the workspace such a process would otherwise
	// rewrite when it closed. A database that is not there is therefore not
	// created; it is the workspace nothing has published, reported as such. The schema fingerprint is
	// verified by reading instead of inside a write transaction, and every
	// mutating entry point refuses with CTX_INTERNAL, because a command that
	// mutates must be composed with the writer rather than discover at runtime
	// that it cannot.
	ReadOnly bool
	// LazyWriter opens a writing store that performs no write at all until
	// something actually writes. The writer pool is built, but database/sql
	// establishes no connection until a statement runs on it, so the only
	// write a writing open ever performed -- the schema check inside an
	// immediate transaction -- is what this removes: the schema is verified by
	// READING, and created only when the cache holds no tables at all, which
	// is a cache no run has ever built here and therefore one no run can be
	// holding a transaction against.
	//
	// It exists for the process that both answers and indexes: it must come up
	// and serve beside an index another process is already running, and a
	// schema check in a write transaction queues behind that run's ingestion
	// group and is refused `database is busy: begin` when the busy timeout
	// runs out. Ignored when ReadOnly is set: that store has no writer.
	LazyWriter bool
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
	if s.opts.ReadOnly {
		// There is no connection whose mode is a choice: this process commits
		// nothing. Reporting the readers' pinned FULL would claim a durability
		// the operator may not have configured, so the mode is unread here and
		// the caller says so rather than guessing.
		return "", internal("this process opened the store read-only and holds no writer connection")
	}
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

	// The writer: one goroutine (runWriter) owns the writer connection, and
	// every use of it -- an ingestion call, an exclusive write, a read of the
	// group's own writes, a flush, the final commit at close -- is a job handed
	// to it over jobs. writerDone is closed when the goroutine has exited, so a
	// caller of a closed store is answered instead of blocked. Both are nil on
	// a read-only store, which has no writer.
	jobs       chan writerJob
	writerDone chan struct{}
	// group is the ingestion group: one write transaction that every
	// ingestion call joins, so a run commits when the writer's page cache
	// would spill, when an exclusive writer needs the connection, at
	// activation or abort, at a flush and at close -- never per batch. Only
	// the writer goroutine reads or writes it.
	group *writeGroup
	// commits counts the ingestion groups this store has committed. It is a
	// disclosure of how a long cascade was broken up, read by the test that
	// holds the activation's compaction to the group bound.
	commits atomic.Int64
	// writerBusy is the nanoseconds the writer goroutine has spent running
	// jobs, commits and checkpoints included (WriterBusy).
	writerBusy atomic.Int64
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
	if opts.ReadOnly {
		// A read-only open creates neither the data directory nor the
		// database, so a database that is not there is the workspace nothing
		// has ever published -- reported here, in the typed answer that sends
		// the operator to `codectx index`, because the read-only handle cannot
		// reach the schema check to report it. The engine's read-only mode
		// refuses to create the file at all, and without this the operator
		// would read the driver's own "unable to open database file". What it
		// may create is beside an existing database: in a directory it can
		// write, the engine makes the log's shared-memory index and a
		// zero-length log where they are missing, because reading a log needs
		// them; in one it cannot write, nothing (see unchangingRead).
		if _, statErr := os.Stat(abs); errors.Is(statErr, fs.ErrNotExist) {
			return nil, notPublishedYet()
		} else if statErr != nil {
			return nil, internal("database path: " + statErr.Error())
		}
	} else if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, internal("database directory: " + err.Error())
	}
	// The engine unlinks the write-ahead log from inside its close, and it
	// holds the database exclusively for the whole of that call: a log freed
	// in place there is minutes of held lock on a large one, which every other
	// process on this workspace waits out at open. Freeing it must therefore be
	// a rename into the to-free set, and that set exists only where the
	// directory has been registered. Registering it HERE, from the store that
	// owns the file, is what makes that true of every process that opens a
	// database -- rather than only of one that happened to stage a lexical unit
	// first and claim the arena on the way. The claim itself stays lazy:
	// nothing is created until something is actually freed.
	scratch.For(filepath.Dir(abs))
	s := &Store{path: abs, opts: opts, freeBytes: diskfree.Available}
	busy := strconv.FormatInt(opts.BusyTimeout.Milliseconds(), 10)
	// The settings that govern reading, which every connection carries, and
	// the journal mode, which only a connection opened read-write can set: a
	// store opened read-only at the file level is refused that pragma on
	// exactly the database a reader is there to read, and it does not need it
	// -- the mode is a property of the file, already recorded in it, and the
	// read-only open is what makes the store unwritable rather than a pragma
	// the connection carries.
	reading := []pragma{
		{"busy_timeout", busy, busy},
		{"foreign_keys", "ON", "1"},
		{"temp_store", "FILE", "1"},
		{"mmap_size", "0", "0"},
	}
	common := slices.Concat(reading, []pragma{{"journal_mode", "WAL", "wal"}})
	// The writer is the only connection whose synchronous mode is a choice,
	// because it is the only connection that commits. Readers keep FULL.
	writerSync, err := synchronousPragma(opts.Synchronous)
	if err != nil {
		return nil, err
	}
	// Nothing sets a journal size limit: the engine's default rewinds the log
	// in place at every reset rather than truncating it, so the file keeps its
	// high-water length -- bounded by the log the largest group leaves -- and
	// every later group writes over the space it already holds. A run that
	// truncated its log at each reset would hand the filesystem gigabytes of
	// freed blocks in the middle of its work.
	writerPragmas := slices.Concat(common, []pragma{
		writerSync,
		{"cache_size", "-" + strconv.Itoa(opts.WriterCacheKiB), "-" + strconv.Itoa(opts.WriterCacheKiB)}})
	// A reader of a store this process also writes is pinned to FULL, which
	// says nothing about durability -- it never commits -- and keeps every
	// pooled connection's pragma set verified. A store opened read-only at the
	// file level cannot set that one either, for the same reason as the
	// journal mode.
	readerBase, readerSync := common, []pragma{{"synchronous", "FULL", "2"}}
	if opts.ReadOnly {
		readerBase, readerSync = reading, nil
	}
	readerPragmas := slices.Concat(readerBase, readerSync, []pragma{
		{"cache_size", "-" + strconv.Itoa(opts.ReaderCacheKiB), "-" + strconv.Itoa(opts.ReaderCacheKiB)},
		{"query_only", "ON", "1"}})

	// Whether this read-only open must declare the database unchanging is
	// settled before any pool is built, so every connection of every pool
	// carries the same view of the file.
	immutable := false
	if opts.ReadOnly {
		if immutable, err = unchangingRead(abs); err != nil {
			return nil, err
		}
	}
	if !opts.ReadOnly {
		if s.writer, err = openPool(abs, writerPragmas, "immediate", 1, false, false); err != nil {
			return nil, err
		}
		// The writer goroutine runs before the schema step, which writes.
		s.startWriter()
	}
	s.readers, err = openPool(abs, readerPragmas, "deferred", opts.ReadConnections, opts.ReadOnly, immutable)
	if err != nil {
		s.closeOpened()
		return nil, err
	}
	if err := s.checkEngine(ctx); err != nil {
		s.closeOpened()
		return nil, err
	}
	// A writer-bearing process creates the schema when the cache is empty; a
	// read-only one verifies the fingerprint it finds. The verification is the
	// same comparison, run on the reader pool, so it neither begins a write
	// transaction nor waits for one another process holds.
	switch {
	case opts.ReadOnly:
		err = s.verifySchema(ctx)
	case opts.LazyWriter:
		err = s.adoptSchema(ctx)
	default:
		err = s.initSchema(ctx)
	}
	if err != nil {
		s.closeOpened()
		return nil, err
	}
	// Posting streams hold one read transaction open for the whole candidate
	// walk of a query. They get their own pool so that holding one can never
	// starve the short reads (Match) the same query issues
	// while the stream is open, which on the shared reader pool would be a
	// deadlock as soon as two queries ran at once.
	s.postings, err = openPool(abs, readerPragmas, "deferred", opts.ReadConnections, opts.ReadOnly, immutable)
	if err != nil {
		s.closeOpened()
		return nil, err
	}
	// The tokenizer database holds no data; it exists so query text can be
	// split with the exact unicode61 tokenizer search_fts uses (Section 12.4).
	// Its cache is stated so the base footprint counts what it runs with.
	tokenizerCache := "-" + strconv.Itoa(config.TokenizerCacheKiB)
	s.tokenizer, err = openPool("", []pragma{{"cache_size", tokenizerCache, tokenizerCache}}, "deferred", 1, false, false)
	if err != nil {
		s.closeOpened()
		return nil, err
	}
	return s, nil
}

// closeOpened stops the writer goroutine, if Open started one, and closes
// whichever pools Open has opened so far. Open builds them in order and a
// read-only store never has a writer, so every failure path releases exactly
// what exists rather than naming a fixed set.
func (s *Store) closeOpened() {
	_ = s.stopWriter()
	for _, db := range []*sql.DB{s.tokenizer, s.postings, s.readers, s.writer} {
		if db != nil {
			db.Close()
		}
	}
}

// pragma is one required per-connection setting: the value the DSN applies and
// the value reading it back must report.
type pragma struct {
	name, set, want string
}

// openPool builds one connection pool over path. readOnly opens every
// connection of it in the engine's own read-only mode; immutable additionally
// declares the file unchanging, which is what a store on media this process
// cannot write needs and nothing else may claim (see unchangingRead).
//
// readOnly is what query_only does not do: query_only refuses writes through SQL, and leaves the handle
// read-WRITE at the file level, so the last connection to close still runs the
// engine's write-ahead-log close -- checkpoint the log into the database,
// unlink the log and the shared-memory index. That is a rewrite of the whole
// published index by a command that promises to write nothing, and it is
// performed holding the database exclusively. The open mode is the only thing
// that stops it.
func openPool(path string, pragmas []pragma, txlock string, maxConns int, readOnly, immutable bool) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p.name+"("+p.set+")")
	}
	q.Set("_txlock", txlock)
	if readOnly {
		q.Set("mode", "ro")
	}
	if immutable {
		q.Set("immutable", "1")
	}
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

// dirAccess is what a probe of the data directory learned about whether this
// process may create files in it.
type dirAccess int

const (
	dirAccessUnknown dirAccess = iota
	dirWritable
	dirReadOnly
)

// unchangingRead decides, before any connection exists, whether a read-only
// open of this store must declare the database unchanging, which is the one way
// a store on media this process cannot write can be read at all.
//
// A write-ahead-log database is read through the shared-memory index beside the
// log, and a reader CREATES that index where it is missing. In a data directory
// this process may not write -- read-only media, a workspace an operator has
// locked down, a mount taken read-only for an audit -- it cannot, so the
// decision is made from the directory itself (see probeDirAccess) and from what
// lies beside the database, never from how an attempted open failed:
//
//   - A writable directory, or one whose writability this platform cannot
//     probe, is opened the ordinary read-only way.
//   - An unwritable directory with no log beside the database, or a zero-length
//     one, is read as an unchanging file. A log with no frames holds nothing
//     the database lacks, and a new frame needs a writer that can write the
//     directory this process cannot.
//   - An unwritable directory whose log holds frames AND whose index is beside
//     it is opened the ordinary read-only way: the engine reads the log through
//     the existing index without writing either.
//   - An unwritable directory whose log holds frames and has no index is
//     refused (logUnreadable). The log is the newest state of the database;
//     reading the database as unchanging without it would answer from an older
//     state, and without an index the engine cannot read the log at all.
func unchangingRead(path string) (bool, error) {
	dir := filepath.Dir(path)
	access, err := probeDirAccess(dir)
	if err != nil {
		return false, internal("data directory " + dir + ": " + err.Error())
	}
	if access != dirReadOnly {
		return false, nil
	}
	log, err := os.Stat(path + walSuffix)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true, nil
	case err != nil:
		return false, internal("write-ahead log: " + err.Error())
	case log.Size() == 0:
		return true, nil
	}
	switch _, err := os.Stat(path + shmSuffix); {
	case err == nil:
		return false, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, logUnreadable(path, dir, log.Size())
	default:
		return false, internal("write-ahead log index: " + err.Error())
	}
}

// logUnreadable is the refusal of a store whose newest state is in a log this
// process can neither read nor fold into the database. It is the operator's to
// fix, in the family a data directory the process must write and cannot is
// reported in.
func logUnreadable(path, dir string, logBytes int64) *model.Error {
	return &model.Error{Code: model.CodeConfigInvalid,
		Message: fmt.Sprintf("the write-ahead log beside %s holds %d bytes not yet folded into the database, "+
			"and %s cannot be written, so the log's index cannot be created and the log cannot be read; "+
			"reading the database without it would answer from an older state", path, logBytes, dir),
		Remediation: "Make " + dir + " writable and run `codectx index` once, which folds the log into the " +
			"database; or copy the data directory to writable storage and set storage.data_dir to the copy.",
		Details: map[string]string{"wal_bytes": strconv.FormatInt(logBytes, 10)}}
}

// walSuffix and shmSuffix name the write-ahead log and its shared-memory index
// beside a database, which is the engine's own naming and not a choice this
// package makes.
const (
	walSuffix = "-wal"
	shmSuffix = "-shm"
)

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
	if err := s.readers.QueryRowContext(ctx, `SELECT sqlite_version(), sqlite_source_id()`).Scan(&s.Version, &s.SourceID); err != nil {
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

// Close commits any open ingestion group, stops the writer goroutine and
// releases every pool. It is safe to call once.
func (s *Store) Close() error {
	first := s.stopWriter()
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

// writerJob is one use of the writer connection, run by the writer goroutine.
// run executes there and nowhere else, which is what lets it touch s.writer
// and s.group without a lock. A run must never hand another job to the
// writer -- through ingest, write, readOwn, writerQueryRow or Flush -- because
// the goroutine that would serve it is the one running it. stop ends the
// goroutine once run returns.
type writerJob struct {
	run  func() error
	done chan writerResult
	stop bool
}

// writerResult is what the writer goroutine hands back to the caller of one
// job: its error, or the value it panicked with.
type writerResult struct {
	err      error
	panicked bool
	panicVal any
}

// writeGroup is the open ingestion group: its transaction, the statement
// cache that lives exactly as long as it (stmtcache.go), and the count of
// bytes written to a write-ahead log through the process's file system when
// it began. Nothing reaches the log while a group's dirty pages fit the
// writer's page cache, so a count that has moved since is one the cache has
// started spilling into.
type writeGroup struct {
	tx    *sql.Tx
	stmts *stmtCache
	log   int64
}

// startWriter starts the goroutine that owns the writer connection.
func (s *Store) startWriter() {
	s.jobs = make(chan writerJob)
	s.writerDone = make(chan struct{})
	go s.runWriter()
}

// runWriter serves jobs one at a time until a stop job has run. The hand-off
// channel is unbuffered, so the queue is the callers blocked on it -- bounded
// by their number, served in the order they arrived -- and an exclusive write
// that arrives behind ingestion calls commits the group they built before it
// runs, which is the ordering an exclusive writer needs.
func (s *Store) runWriter() {
	defer close(s.writerDone)
	for job := range s.jobs {
		start := time.Now()
		res := s.runJob(job.run)
		s.writerBusy.Add(int64(time.Since(start)))
		job.done <- res
		if job.stop {
			return
		}
	}
}

// runJob runs one job. A panic inside it must not end the goroutine, which
// would leave every later caller waiting on a writer that is gone: the open
// group is abandoned, since a panic mid-batch leaves it in no state the run
// can build on, and the value goes back to the caller, who panics with it in
// its own goroutine. An exclusive write's own transaction has already been
// rolled back by the time the panic reaches here (see write).
func (s *Store) runJob(run func() error) (res writerResult) {
	defer func() {
		if v := recover(); v != nil {
			s.abandonGroup()
			res = writerResult{panicked: true, panicVal: v}
		}
	}()
	return writerResult{err: run()}
}

// onWriter hands run to the writer goroutine and waits for its result. The
// hand-off gives up when the caller's context ends or the store is closed;
// once the goroutine has taken the job, the caller waits for it to finish,
// because run writes into the caller's own variables. The goroutine runs it
// under the caller's context, so a caller whose context ends loses its own
// statements promptly. A goroutine that exits without answering -- a job that
// ended it through runtime.Goexit -- is reported as a closed store rather than
// waited on forever.
func (s *Store) onWriter(ctx context.Context, run func() error, stop bool) error {
	if s.jobs == nil {
		return readOnlyRefusal()
	}
	job := writerJob{run: run, done: make(chan writerResult, 1), stop: stop}
	select {
	case s.jobs <- job:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.writerDone:
		return storeClosed()
	}
	var res writerResult
	select {
	case res = <-job.done:
	case <-s.writerDone:
		// A stop job answers and then exits, so both can be ready at once;
		// its answer is the one that counts.
		select {
		case res = <-job.done:
		default:
			return storeClosed()
		}
	}
	if res.panicked {
		panic(res.panicVal)
	}
	return res.err
}

// stopWriter commits the open group and ends the writer goroutine. It is what
// Close and every failure path of Open call; on a store without a writer, or
// one whose writer has already stopped, it does nothing.
func (s *Store) stopWriter() error {
	if s.jobs == nil {
		return nil
	}
	select {
	case <-s.writerDone:
		return nil
	default:
	}
	return s.onWriter(context.Background(), s.commitGroup, true)
}

// storeClosed is the answer to a caller of a store whose writer has stopped.
// It is CTX_INTERNAL: only a composition that uses a store after closing it
// can reach it.
func storeClosed() *model.Error { return internal("the store is closed") }

// WriterBusy is the writer's busy time: how long the goroutine that owns the
// writer connection has spent running jobs, commits and checkpoints included,
// since the store opened. The index build reports it against a stage's wall
// (ADR-0012 decision 5): the share of the wall the one writer the log admits
// was busy says whether writing is what bounds the stage. A read-only store has
// no writer and reports zero.
func (s *Store) WriterBusy() time.Duration { return time.Duration(s.writerBusy.Load()) }

// write runs fn in one immediate write transaction of its own on the single
// writer connection, committed before it returns. It is the path for state
// that must be visible to every connection the moment the call returns --
// sessions, leases, heartbeats, retention, the query planner's statistics --
// and it commits any open ingestion group first, since the group holds the
// connection. The transaction keeps no statement cache. Any error rolls back;
// the caller sees the first typed failure.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.onWriter(ctx, func() error {
		if err := s.commitGroup(); err != nil {
			return err
		}
		tx, err := s.writer.BeginTx(ctx, nil)
		if err != nil {
			return wrap("begin", err)
		}
		// Rolled back on every path that does not commit, a panic in fn
		// included, so the connection is never left inside a transaction.
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return wrap("commit", err)
		}
		return nil
	}, false)
}

// ingest runs fn inside the ingestion group, opening the group when none is
// open. fn is atomic on its own -- it runs inside a savepoint, so a refused
// batch rolls back alone and the units already in the group are kept -- and
// the group commits when the writer's page cache would spill. The group's
// transaction is begun under a context that outlives any one caller: a caller
// whose context ends loses its own statement, never the run.
func (s *Store) ingest(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.attribute(s.onWriter(ctx, func() error { return s.ingestGroup(ctx, false, fn) }, false))
}

// ingestAndCommit is ingest followed by the group's commit, whether fn
// succeeded or was rolled back, for the calls whose outcome must be durable
// when they return: a generation's activation or abort.
func (s *Store) ingestAndCommit(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return s.attribute(s.onWriter(ctx, func() error {
		err := s.ingestGroup(ctx, true, fn)
		// The run is over: the writer's page cache, which the run filled with
		// its groups, goes back to the process so a long-lived server does not
		// keep a run's working set resident.
		_, _ = s.writer.ExecContext(context.WithoutCancel(ctx), `PRAGMA shrink_memory`)
		return err
	}, false))
}

// ingestGroup is the body of an ingestion job. It runs on the writer
// goroutine.
func (s *Store) ingestGroup(ctx context.Context, commit bool, fn func(tx *sql.Tx) error) error {
	if s.group == nil {
		tx, err := s.writer.BeginTx(context.WithoutCancel(ctx), nil)
		if err != nil {
			return wrap("begin", err)
		}
		s.group = &writeGroup{tx: tx, stmts: newStmtCache(tx), log: pacedvfs.LogBytes()}
	}
	tx := s.group.tx
	if _, err := tx.ExecContext(ctx, `SAVEPOINT ingest`); err != nil {
		return wrap("savepoint", err)
	}
	if err := fn(tx); err != nil {
		if _, rbErr := tx.ExecContext(context.WithoutCancel(ctx), `ROLLBACK TO ingest`); rbErr != nil {
			// The savepoint could not be unwound: the group is no longer a
			// state the run can build on, so it ends here.
			s.abandonGroup()
			return errors.Join(err, wrap("rollback to savepoint", rbErr))
		}
		if _, relErr := tx.ExecContext(context.WithoutCancel(ctx), `RELEASE ingest`); relErr != nil {
			s.abandonGroup()
			return errors.Join(err, wrap("release savepoint", relErr))
		}
		if commitErr := s.commitGroupIfDue(commit); commitErr != nil {
			return errors.Join(err, commitErr)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE ingest`); err != nil {
		s.abandonGroup()
		return wrap("release savepoint", err)
	}
	return s.commitGroupIfDue(commit)
}

// commitGroupIfDue commits the open group when the caller asks for it or when
// the writer's page cache has begun spilling to the log. It runs on the
// writer goroutine.
func (s *Store) commitGroupIfDue(force bool) error {
	if s.group == nil {
		return nil
	}
	if !force && pacedvfs.LogBytes() == s.group.log {
		return nil
	}
	return s.commitGroup()
}

// commitGroup commits the open group, if any, closing its statement cache
// first, and folds its log into the database. It runs on the writer
// goroutine.
func (s *Store) commitGroup() error {
	g := s.group
	if g == nil {
		return nil
	}
	s.group = nil
	g.stmts.close()
	if err := g.tx.Commit(); err != nil {
		return s.attribute(wrap("commit", err))
	}
	s.commits.Add(1)
	// The engine checkpoints on its own once the log holds a thousand
	// frames; a group smaller than that would otherwise leave its frames for
	// the next group to stack on. A checkpoint that finds a reader on the log
	// folds what it can and is not a failure of the commit.
	_, _ = s.writer.ExecContext(context.Background(), `PRAGMA wal_checkpoint(PASSIVE)`)
	return nil
}

// abandonGroup rolls the open group back, closing its statement cache first.
// It runs on the writer goroutine.
func (s *Store) abandonGroup() {
	g := s.group
	if g == nil {
		return
	}
	s.group = nil
	g.stmts.close()
	g.tx.Rollback()
}

// groupStmts is the open group's statement cache, or nil when no group is
// open. Only a job reads it, and one goroutine runs every job, so a caller
// inside an ingestion call holds the cache alone for its length.
func (s *Store) groupStmts() *stmtCache {
	if s.group == nil {
		return nil
	}
	return s.group.stmts
}

// WALBoundBytes is the largest write-ahead log an ingestion group leaves
// behind: the writer's page cache, which the group fills before it commits,
// plus one batch of spilled pages. The log is rewound in place at every reset
// and keeps its high-water length, so a log past this bound is one whose
// frames a group actually reached: a reader kept the old ones open across
// several groups, or the log was never checkpointed.
func (s *Store) WALBoundBytes() int64 { return 2 * int64(s.opts.WriterCacheKiB) << 10 }

// Flush commits the open ingestion group, if any. The index build calls it at
// each provider boundary, so the next provider's reads on the reader pool see
// the units the previous one sealed; a caller that opens a connection of its
// own -- a test or a tool reading the database file beside the store -- calls
// it to see what the store has written so far. Close commits whatever a
// process leaves open.
func (s *Store) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return wrap("flush", err)
	}
	if s.opts.ReadOnly {
		return nil
	}
	return s.onWriter(ctx, s.commitGroup, false)
}

// readOwn runs fn where it sees the ingestion group's own writes: on the
// group's transaction while one is open, and on the reader pool otherwise.
// It is the read path of the ingestion side -- unit states, aliases of
// dependency units, the snapshot and blobs a capture just recorded -- whose
// callers reason about what this run has already stored. Query paths read
// through read and never see a group in progress. Only the group branch is a
// job: with no group open, the read runs on the reader pool in the caller's
// goroutine and never delays the writer. It is never called from inside a
// job: a producer stream an ingestion call drains reads through the pool
// instead (UnitInputs), and sees the last commit.
func (s *Store) readOwn(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if s.opts.ReadOnly {
		return s.read(ctx, fn)
	}
	onGroup := false
	err := s.onWriter(ctx, func() error {
		if s.group == nil {
			return nil
		}
		onGroup = true
		return fn(s.group.tx)
	}, false)
	if onGroup || err != nil {
		return s.attribute(err)
	}
	return s.read(ctx, fn)
}

// writerQueryRow issues one row query on the writer connection, through the
// open group when there is one so the probe never waits on the run.
func (s *Store) writerQueryRow(ctx context.Context, query string, dest ...any) error {
	return s.onWriter(ctx, func() error {
		if s.group != nil {
			return s.group.tx.QueryRowContext(ctx, query).Scan(dest...)
		}
		return s.writer.QueryRowContext(ctx, query).Scan(dest...)
	}, false)
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

// readOnlyRefusal is what every mutating entry point returns on a store opened
// read-only. It is CTX_INTERNAL rather than an operator-facing family because
// it can only be reached by composing a mutating command with the read-only
// composition: the operator has no input that produces it.
func readOnlyRefusal() *model.Error {
	return internal("this process opened the store read-only; a command that changes the workspace must be composed with the writer")
}

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
// disk that is simply full.
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
// more, the error becomes CTX_INTERNAL carrying the engine's message and code
// and the figure. It states what was measured and stops there: free space
// under the directory is blind to a user quota, a container limit and another
// filesystem mounted underneath, so a refusal with gigabytes free may still be
// the writer's own limit rather than the device, and the remediation lists all
// three. A disk the platform cannot measure keeps CTX_DISK_FULL and says the
// measurement was unavailable.
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
		settled.Message += fmt.Sprintf("; %d bytes were free under %s when the write was refused", free, dir)
		settled.Remediation = "check for a quota on the writing user, a limit on the container or filesystem holding " +
			dir + ", and the kernel log for its device"
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
