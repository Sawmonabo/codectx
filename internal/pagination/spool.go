package pagination

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Spools manages bounded, disk-backed continuation state for traversals whose
// frontier cannot live in a token (Section 14.3). Each spool is a private
// 0600 file under dir bound to one lease, generation and query; it lives
// exactly as long as its lease and counts against one shared byte budget.
type Spools struct {
	dir      string
	maxBytes int64
	leases   LeaseStore

	// mu guards used, the bytes every live spool has reserved. Reservations
	// are taken before a write, so concurrent spools share one budget instead
	// of each measuring the directory and racing past the cap together.
	mu   sync.Mutex
	used int64
}

// SpoolHeader binds a spool to the cursor that references it. Open rejects a
// spool whose header disagrees with the presenting cursor. ExpiresAt is the
// expiry at creation and is informational: liveness is the lease's.
type SpoolHeader struct {
	Version      int                `json:"version"`
	SpoolID      string             `json:"spool_id"`
	LeaseID      string             `json:"lease_id"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	ExpiresAt    time.Time          `json:"expires_at"`
}

const (
	spoolVersion = 1
	spoolPrefix  = "spool-"
	// maxSpoolRecordBytes bounds one record so a reader never allocates from
	// an unbounded length prefix.
	maxSpoolRecordBytes = 1 << 20
	// headerlessGrace is how long a spool file may exist without a readable
	// header before a sweep treats it as a crashed Create. A live Create
	// syncs its header before returning, so this covers only the window
	// between the file's creation and that sync.
	headerlessGrace = time.Minute
)

// NewSpools opens dir (created 0700) with a total byte cap across live spools
// and the lease store that decides whether a spool is still live. Bytes
// already on disk from a previous process count against the cap until Sweep
// reconciles them.
func NewSpools(dir string, maxBytes int64, leases LeaseStore) (*Spools, error) {
	if maxBytes <= 0 {
		return nil, &model.Error{Code: model.CodeConfigInvalid, Message: "spool byte cap must be positive"}
	}
	if leases == nil {
		return nil, &model.Error{Code: model.CodeConfigInvalid, Message: "spools require a lease store"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internalErr("spool directory: " + err.Error())
	}
	s := &Spools{dir: dir, maxBytes: maxBytes, leases: leases}
	used, err := s.diskBytes()
	if err != nil {
		return nil, err
	}
	s.used = used
	return s, nil
}

// reserve claims n bytes of the shared budget or fails without claiming any.
func (s *Spools) reserve(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+n > s.maxBytes {
		return &model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message: "query spool exceeds its disk budget", Remediation: "narrow the query, retry after outstanding cursors expire, or raise resources.max_temp_bytes"}
	}
	s.used += n
	return nil
}

// release returns n bytes to the budget.
func (s *Spools) release(n int64) {
	s.mu.Lock()
	s.used -= n
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

// Spool is one open spool file. Records are appended while a query runs and
// read back in order by a continuation.
type Spool struct {
	owner   *Spools
	header  SpoolHeader
	path    string
	file    *os.File
	w       *bufio.Writer
	written int64
}

// Create allocates a spool for cursor c. The header is written, flushed and
// synced before Create returns, so a sweep or a crash never finds a live spool
// as an empty file, and a later Open can validate it even if no record was
// ever appended.
func (s *Spools) Create(c Cursor) (*Spool, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	id, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	h := SpoolHeader{Version: spoolVersion, SpoolID: id, LeaseID: c.LeaseID, GenerationID: c.GenerationID,
		AnalysisKey: c.AnalysisKey, QueryHash: c.QueryHash, ExpiresAt: c.ExpiresAt.UTC()}
	head, err := json.Marshal(h)
	if err != nil {
		return nil, internalErr("spool header: " + err.Error())
	}
	path := filepath.Join(s.dir, spoolPrefix+id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, internalErr("spool create: " + err.Error())
	}
	sp := &Spool{owner: s, header: h, path: path, file: f, w: bufio.NewWriter(f)}
	if err := sp.writeFrame(head); err != nil {
		sp.discard()
		return nil, err
	}
	if err := sp.w.Flush(); err != nil {
		sp.discard()
		return nil, internalErr("spool header flush: " + err.Error())
	}
	if err := f.Sync(); err != nil {
		sp.discard()
		return nil, internalErr("spool header sync: " + err.Error())
	}
	return sp, nil
}

// discard abandons a spool that failed during Create.
func (sp *Spool) discard() {
	sp.file.Close()
	os.Remove(sp.path)
	sp.owner.release(sp.written)
	sp.file, sp.w = nil, nil
}

// ID is the spool identifier a cursor carries.
func (sp *Spool) ID() string { return sp.header.SpoolID }

// Append writes one record. Exceeding the remaining budget is a typed limit,
// never a silent truncation.
func (sp *Spool) Append(record []byte) error {
	if sp.w == nil {
		return internalErr("spool is not open for writing")
	}
	if len(record) > maxSpoolRecordBytes {
		return &model.Error{Code: model.CodeResourceLimit, Message: "spool record exceeds the record size bound"}
	}
	return sp.writeFrame(record)
}

func (sp *Spool) writeFrame(p []byte) error {
	need := int64(4 + len(p))
	if err := sp.owner.reserve(need); err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(p)))
	if _, err := sp.w.Write(length[:]); err != nil {
		sp.owner.release(need)
		return internalErr("spool write: " + err.Error())
	}
	if _, err := sp.w.Write(p); err != nil {
		sp.owner.release(need)
		return internalErr("spool write: " + err.Error())
	}
	sp.written += need
	return nil
}

// Close flushes and closes a spool opened for writing. It is idempotent. The
// bytes stay reserved until Release or Sweep removes the file.
func (sp *Spool) Close() error {
	if sp.file == nil {
		return nil
	}
	var err error
	if sp.w != nil {
		err = sp.w.Flush()
	}
	if cerr := sp.file.Close(); err == nil {
		err = cerr
	}
	sp.file, sp.w = nil, nil
	if err != nil {
		return internalErr("spool close: " + err.Error())
	}
	return nil
}

// Open validates that the spool named by cursor c exists, was written for
// exactly this lease, generation and query, and that its lease is still live
// at now, then streams its records to fn in order. The header is checked
// before any record is read; liveness comes from the lease store, so a renewed
// lease keeps its spool and a released one ends it.
func (s *Spools) Open(ctx context.Context, c Cursor, now time.Time, fn func(record []byte) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.SpoolID == "" {
		return cursorInvalid("cursor names no spool")
	}
	f, err := os.Open(filepath.Join(s.dir, spoolPrefix+c.SpoolID))
	if errors.Is(err, os.ErrNotExist) {
		return cursorInvalid("continuation state has expired or was released")
	}
	if err != nil {
		return internalErr("spool open: " + err.Error())
	}
	defer f.Close()
	r := bufio.NewReader(f)
	h, err := readHeader(r)
	if err != nil {
		return err
	}
	if h.SpoolID != c.SpoolID || h.LeaseID != c.LeaseID || h.GenerationID != c.GenerationID ||
		h.AnalysisKey != c.AnalysisKey || h.QueryHash != c.QueryHash {
		return cursorInvalid("spool does not belong to this cursor")
	}
	expiry, err := s.leases.LeaseExpiry(ctx, h.LeaseID)
	if err != nil {
		return err
	}
	if !now.Before(expiry) {
		return cursorInvalid("continuation state has expired")
	}
	for {
		rec, err := readFrame(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// readHeader reads and validates the first frame. A file that ends before a
// header is a typed corruption, never a bare io.EOF leaking to the caller.
func readHeader(r *bufio.Reader) (SpoolHeader, error) {
	head, err := readFrame(r)
	if errors.Is(err, io.EOF) {
		return SpoolHeader{}, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool has no header"}
	}
	if err != nil {
		return SpoolHeader{}, err
	}
	var h SpoolHeader
	if err := json.Unmarshal(head, &h); err != nil || h.Version != spoolVersion {
		return SpoolHeader{}, cursorInvalid("spool header is not readable")
	}
	return h, nil
}

// readFrame returns io.EOF only at a clean frame boundary.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool is truncated"}
	}
	n := binary.BigEndian.Uint32(length[:])
	if n > maxSpoolRecordBytes {
		return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool record length exceeds its bound"}
	}
	rec := make([]byte, n)
	if _, err := io.ReadFull(r, rec); err != nil {
		return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool is truncated"}
	}
	return rec, nil
}

// Release removes one spool once its cursor chain ends or its lease is
// released, returning its bytes to the budget. A missing spool is not an error.
func (s *Spools) Release(spoolID string) error {
	if !model.ValidHexID(spoolID) {
		return cursorInvalid("spool id is malformed")
	}
	return s.remove(filepath.Join(s.dir, spoolPrefix+spoolID))
}

func (s *Spools) remove(path string) error {
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return internalErr("spool release: " + err.Error())
	}
	s.release(size)
	return nil
}

// Sweep removes spools whose lease is gone or expired at now, and files with
// no readable header that are older than the create grace window, then
// reconciles the budget with the bytes still on disk and reports them.
// Callers run it on a schedule and at startup recovery, not per query.
func (s *Spools) Sweep(ctx context.Context, now time.Time) (liveBytes int64, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, internalErr("spool sweep: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		h, ok := spoolHeader(path)
		if !ok {
			// No header yet: a Create in progress within the grace window is
			// live; anything older is a crashed Create.
			if now.Sub(info.ModTime()) > headerlessGrace {
				os.Remove(path)
				continue
			}
			liveBytes += info.Size()
			continue
		}
		expiry, err := s.leases.LeaseExpiry(ctx, h.LeaseID)
		if err != nil {
			var typed *model.Error
			if errors.As(err, &typed) && typed.Code == model.CodeCursorInvalid {
				os.Remove(path)
				continue
			}
			return 0, err
		}
		if !now.Before(expiry) {
			os.Remove(path)
			continue
		}
		liveBytes += info.Size()
	}
	s.mu.Lock()
	s.used = liveBytes
	s.mu.Unlock()
	return liveBytes, nil
}

func spoolHeader(path string) (SpoolHeader, bool) {
	f, err := os.Open(path)
	if err != nil {
		return SpoolHeader{}, false
	}
	defer f.Close()
	h, err := readHeader(bufio.NewReader(f))
	return h, err == nil
}

func (s *Spools) diskBytes() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, internalErr("spool directory: " + err.Error())
	}
	var used int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			used += info.Size()
		}
	}
	return used, nil
}
