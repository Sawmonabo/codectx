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
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Options are the numeric storage settings of Section 20.1 `[storage]` plus the
// `[index]` batch bounds the store enforces on unit writes. Zero selects the
// documented default; no value means unlimited.
type Options struct {
	BusyTimeout       time.Duration // busy_timeout, default 5s
	ReadConnections   int           // read_connections, default 2
	WriterCacheKiB    int           // writer_cache_kib, default 8192
	ReaderCacheKiB    int           // reader_cache_kib, default 4096
	WALHighWaterBytes int64         // wal_high_water_bytes, default 64 MiB
	BatchRecords      int           // index.batch_records, default 1000
	BatchBytes        int64         // index.batch_bytes, default 4 MiB
	// MaxJSONBytes bounds every stored JSON column that has no tighter model
	// ceiling (manifest requests, capsules). Default 8 MiB, the Section 20.1
	// context.max_manifest_bytes / max_capsule_bytes default.
	MaxJSONBytes int64
	// Content is the content store's verified range reader. The database keeps
	// no copy of source text (ADR-0003 §2.1), so the one write path that has to
	// re-index text it did not receive -- the delta carry-over, which copies a
	// previous unit's lexical documents -- resolves each document's bytes
	// through this reader. A store opened without one refuses that carry-over
	// with a typed error rather than indexing a document without its body.
	Content BlobReader
}

// BlobReader reads one verified byte range of one retained content-store
// object. It is the range reader Section 10.3 defines, narrowed to what
// storage needs; *snapshot.CAS satisfies it.
type BlobReader interface {
	ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error)
}

func (o Options) withDefaults() Options {
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.ReadConnections <= 0 {
		o.ReadConnections = 2
	}
	if o.WriterCacheKiB <= 0 {
		o.WriterCacheKiB = 8192
	}
	if o.ReaderCacheKiB <= 0 {
		o.ReaderCacheKiB = 4096
	}
	if o.WALHighWaterBytes <= 0 {
		o.WALHighWaterBytes = 64 << 20
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
	return o
}

// Store is one workspace database: a single writer connection, a small reader
// pool and a private in-memory connection for query tokenization.
type Store struct {
	path      string
	opts      Options
	writer    *sql.DB
	readers   *sql.DB
	tokenizer *sql.DB
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
	opts = opts.withDefaults()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, internal("database path: " + err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, internal("database directory: " + err.Error())
	}
	s := &Store{path: abs, opts: opts}
	busy := strconv.FormatInt(opts.BusyTimeout.Milliseconds(), 10)
	common := []pragma{
		{"busy_timeout", busy, busy},
		{"foreign_keys", "ON", "1"},
		{"journal_mode", "WAL", "wal"},
		{"synchronous", "FULL", "2"},
		{"temp_store", "FILE", "1"},
		{"mmap_size", "0", "0"},
	}
	writerPragmas := slices.Concat(common, []pragma{{"cache_size", "-" + strconv.Itoa(opts.WriterCacheKiB), "-" + strconv.Itoa(opts.WriterCacheKiB)}})
	readerPragmas := slices.Concat(common, []pragma{
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
	// The tokenizer database holds no data; it exists so query text can be
	// split with the exact unicode61 tokenizer search_fts uses (Section 12.4).
	s.tokenizer, err = openPool("", nil, "deferred", 1)
	if err != nil {
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

// Close releases every pool. It is safe to call once.
func (s *Store) Close() error {
	var first error
	for _, db := range []*sql.DB{s.tokenizer, s.readers, s.writer} {
		if db == nil {
			continue
		}
		if err := db.Close(); err != nil && first == nil {
			first = wrap("close", err)
		}
	}
	return first
}

// write runs fn in one immediate write transaction on the single writer
// connection. Any error rolls back; the caller sees the first typed failure.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
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

// read runs fn in one short deferred read transaction on the reader pool, so
// every statement inside it sees one consistent snapshot of the database.
func (s *Store) read(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return wrap("begin read", err)
	}
	defer tx.Rollback()
	return fn(tx)
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
		if outOfSpace(se.Code()) {
			return &model.Error{Code: model.CodeDiskFull,
				Message:     "database write failed: the filesystem could not grow the database or its write-ahead log",
				Remediation: "free space under the data directory; if it has space, the filesystem rejected the write and the data directory may need to move"}
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

// outOfSpace reports whether an extended result code says the filesystem had no
// room for a write. SQLITE_FULL is the obvious one and was the only one mapped;
// it is not the one a full disk usually produces. Growing the -shm file of the
// write-ahead index raises SQLITE_IOERR_SHMSIZE (4874) and growing the database
// or the log itself raises the write/sync/truncate members of the same family,
// whose PRIMARY code is SQLITE_IOERR (10) -- so a switch on `code & 0xff` sees
// only SQLITE_IOERR, matches no arm, and reports CTX_INTERNAL for a disk that
// is simply full (proved on a filled tmpfs: connect failed with 4874).
//
// The full extended code is therefore tested, and only the members a space
// exhaustion actually raises are listed: SQLITE_IOERR_READ, _CORRUPTFS, _DATA
// and the locking members must keep falling through to their own families,
// because a read failure or a corrupt filesystem is not a full one. These
// members can also be raised by failing hardware, which is why the remediation
// names both causes rather than asserting the disk is full.
func outOfSpace(code int) bool {
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
