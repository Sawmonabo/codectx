// Package ledger records what a run cost: a tree of spans per run, written by
// one collector goroutine into a database of its own beside the index store,
// and readable by any other process while the run is still going.
//
// It imports internal/model and the pinned engine, and nothing else of the
// product. The store, the coordinator and the providers open spans, so this
// package can never import them.
package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// schemaSQL is this database's complete DDL. A change to it is a new
// fingerprint, and a file carrying another fingerprint is refused rather than
// altered: there is no migration runner here and none is wanted, because the
// rows are a record of runs that have already happened.
//
//go:embed schema.sql
var schemaSQL string

// Fingerprint is the SHA-256 of the embedded DDL. It is this file's own and is
// deliberately not folded into anything the index store keys on: a change here
// must never re-key an analysis unit.
var Fingerprint = func() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}()

// FileName is the ledger database's name beside the index store's own file in
// the workspace directory. It lives under the same directory lock and is
// deleted with the same cache.
const FileName = "ledger.db"

// busyTimeout is how long a connection waits for the single writer before it
// reports the file busy. The collector's transactions are a flush long, so the
// only waiter is the retention path, and it waits rather than failing.
const busyTimeout = 5 * time.Second

// timeLayout is fixed width so stored timestamps compare correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, corrupt("stored timestamp %q is not readable", s)
	}
	return t, nil
}

// pragma is one required per-connection setting: the value the DSN applies and
// the value reading it back must report.
type pragma struct{ name, set, want string }

// cacheKiB is every ledger connection's page cache, stated rather than left
// to the engine's default so the footprint the process counts for it is the
// one the connections run with. The file's rows are small and its reads are
// one run and a page of spans, so a cache this size holds a reader's whole
// working set.
const cacheKiB = 2 << 10

// writerConnections and readerConnections are the two pools' sizes: the
// collector's one writer, and the connections of one Reader.
const (
	writerConnections = 1
	readerConnections = 2
)

// readerHandles is how many Readers one process can hold open at once: one on
// the indexing path -- the learned peaks while a plan is built, the run
// reading itself back, or the probe of another process's progress while this
// one waits for the workspace -- and one a status surface opens beside it in a
// serving process.
const readerHandles = 2

// FootprintBytes is the memory the run ledger's page caches can hold in one
// process: the writer's connection and every connection of every reader it
// can hold open at once. The process's base footprint counts it
// (config.BaseFootprint), because nothing else reserves it.
const FootprintBytes int64 = (writerConnections + readerHandles*readerConnections) * cacheKiB << 10

func commonPragmas() []pragma {
	busy := strconv.FormatInt(busyTimeout.Milliseconds(), 10)
	cache := "-" + strconv.Itoa(cacheKiB)
	return []pragma{
		{"busy_timeout", busy, busy},
		{"foreign_keys", "ON", "1"},
		{"journal_mode", "WAL", "wal"},
		{"temp_store", "FILE", "1"},
		{"mmap_size", "0", "0"},
		{"cache_size", cache, cache},
	}
}

// writerPragmas is the collector's connection. synchronous=normal is the
// durability this file wants: under the write-ahead log it fsyncs at a
// checkpoint rather than at every flush, so a power loss costs the tail of one
// run's accounting and never the index it describes.
func writerPragmas() []pragma {
	return append(commonPragmas(), pragma{"synchronous", "NORMAL", "1"})
}

// readerPragmas is every other process's. query_only is what makes a reader
// structurally unable to disturb a live run, and the connection is opened
// read-only besides, so it cannot even create the file.
//
// It carries neither journal_mode nor synchronous: both are properties of
// writing, a read-only connection cannot set either, and asking one to would
// fail on exactly the file a reader is there to read.
func readerPragmas() []pragma {
	busy := strconv.FormatInt(busyTimeout.Milliseconds(), 10)
	cache := "-" + strconv.Itoa(cacheKiB)
	return []pragma{
		{"busy_timeout", busy, busy},
		{"foreign_keys", "ON", "1"},
		{"temp_store", "FILE", "1"},
		{"mmap_size", "0", "0"},
		{"cache_size", cache, cache},
		{"query_only", "ON", "1"},
	}
}

// openPool opens the file. readOnly opens it in the engine's own read-only
// mode, which is what stops a reader on a workspace that has never been
// indexed from creating an empty ledger: query_only refuses writes through
// SQL, it does not stop the file being made.
func openPool(path string, pragmas []pragma, txlock string, maxConns int, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p.name+"("+p.set+")")
	}
	q.Set("_txlock", txlock)
	if readOnly {
		q.Set("mode", "ro")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	base, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internal("ledger connector: " + err.Error())
	}
	db := sql.OpenDB(verifiedConnector{Connector: base, pragmas: pragmas})
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	return db, nil
}

// initSchema creates the schema in an empty file or verifies an existing one.
func initSchema(ctx context.Context, db *sql.DB, path string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("begin", err)
	}
	defer tx.Rollback()
	var tables int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return wrap("sqlite_master", err)
	}
	if tables == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return wrap("create the ledger schema", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ledger_meta(singleton, fingerprint) VALUES(1, ?)`, Fingerprint); err != nil {
			return wrap("ledger_meta", err)
		}
		return wrap("commit", tx.Commit())
	}
	var fingerprint string
	if err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM ledger_meta WHERE singleton = 1`).Scan(&fingerprint); err != nil || fingerprint != Fingerprint {
		found := fingerprint
		if err != nil {
			found = "unreadable"
		}
		return &model.Error{Code: model.CodeSchemaMismatch,
			Message:     "run ledger schema fingerprint " + found + " does not match this binary's " + Fingerprint,
			Details:     map[string]string{"path": path},
			Remediation: "delete the run ledger beside the index cache, or rebuild the cache with `codectx index --rebuild`"}
	}
	return wrap("commit", tx.Commit())
}

// verifiedConnector reads every pragma back on each physical connection: the
// DSN applies them per connection and this proves each one took.
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
		return nil, internal("ledger connection does not support QueryContext")
	}
	for _, p := range c.pragmas {
		got, err := queryScalar(ctx, q, "PRAGMA "+p.name)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if !strings.EqualFold(got, p.want) {
			conn.Close()
			return nil, corrupt("ledger connection pragma %s reads back %q, want %q", p.name, got, p.want)
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

// Path is the ledger database beside the index store's own file.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// ensureDir creates the ledger's directory with the same mode the store's own
// file gets, so a ledger opened before the store does not widen it.
func ensureDir(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return internal("ledger directory: " + err.Error())
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

// wrap maps a driver failure to its error family. A typed error and a context
// cancellation pass through unchanged.
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
		switch se.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return &model.Error{Code: model.CodeWorkspaceBusy, Message: "run ledger is busy: " + op, Retryable: true}
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return &model.Error{Code: model.CodeStorageCorrupt, Message: "run ledger is not readable: " + se.Error(),
				Remediation: "delete the run ledger beside the index cache; it is accounting, not index content"}
		}
	}
	return internal("run ledger " + op + ": " + err.Error())
}
