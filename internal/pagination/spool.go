package pagination

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Spools manages bounded, disk-backed continuation state for traversals whose
// frontier cannot live in a token (Section 14.3). Each spool is a private
// 0600 file under dir bound to one lease, generation and query; it expires
// with its lease and counts against maxBytes in total.
type Spools struct {
	dir      string
	maxBytes int64
	now      func() time.Time
}

// SpoolHeader binds a spool to the cursor that references it. Open rejects a
// spool whose header disagrees with the presenting cursor.
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
)

// NewSpools opens dir (created 0700) with a total byte cap across live spools.
func NewSpools(dir string, maxBytes int64) (*Spools, error) {
	if maxBytes <= 0 {
		return nil, &model.Error{Code: model.CodeConfigInvalid, Message: "spool byte cap must be positive"}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internalErr("spool directory: " + err.Error())
	}
	return &Spools{dir: dir, maxBytes: maxBytes, now: time.Now}, nil
}

// Spool is one open spool file. Records are appended while a query runs and
// read back in order by a continuation.
type Spool struct {
	header  SpoolHeader
	path    string
	file    *os.File
	w       *bufio.Writer
	written int64
	budget  int64
}

// Create allocates a spool for cursor c. The header is written immediately so
// a crash leaves a file that a later Open can still validate or a sweep can
// expire.
func (s *Spools) Create(c Cursor) (*Spool, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	used, err := s.usedBytes()
	if err != nil {
		return nil, err
	}
	if used >= s.maxBytes {
		return nil, &model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message: "query spool disk budget is exhausted", Remediation: "retry after outstanding cursors expire or are released"}
	}
	id, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	h := SpoolHeader{Version: spoolVersion, SpoolID: id, LeaseID: c.LeaseID, GenerationID: c.GenerationID,
		AnalysisKey: c.AnalysisKey, QueryHash: c.QueryHash, ExpiresAt: c.ExpiresAt.UTC()}
	path := filepath.Join(s.dir, spoolPrefix+id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, internalErr("spool create: " + err.Error())
	}
	sp := &Spool{header: h, path: path, file: f, w: bufio.NewWriter(f), budget: s.maxBytes - used}
	head, err := json.Marshal(h)
	if err != nil {
		f.Close()
		return nil, internalErr("spool header: " + err.Error())
	}
	if err := sp.writeFrame(head); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return sp, nil
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
	if sp.written+need > sp.budget {
		return &model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message: "query spool exceeds its disk budget", Remediation: "narrow the query or raise resources.max_temp_bytes"}
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(p)))
	if _, err := sp.w.Write(length[:]); err != nil {
		return internalErr("spool write: " + err.Error())
	}
	if _, err := sp.w.Write(p); err != nil {
		return internalErr("spool write: " + err.Error())
	}
	sp.written += need
	return nil
}

// Close flushes and closes a spool opened for writing. It is idempotent.
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

// Open validates that the spool named by cursor c exists, is unexpired and was
// written for exactly this lease, generation and query, then streams its
// records to fn in order. The header is checked before any record is read.
func (s *Spools) Open(c Cursor, fn func(record []byte) error) error {
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
	head, err := readFrame(r)
	if err != nil {
		return err
	}
	var h SpoolHeader
	if err := json.Unmarshal(head, &h); err != nil || h.Version != spoolVersion {
		return cursorInvalid("spool header is not readable")
	}
	if h.SpoolID != c.SpoolID || h.LeaseID != c.LeaseID || h.GenerationID != c.GenerationID ||
		h.AnalysisKey != c.AnalysisKey || h.QueryHash != c.QueryHash {
		return cursorInvalid("spool does not belong to this cursor")
	}
	if !s.now().Before(h.ExpiresAt) {
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

func readFrame(r *bufio.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, internalErr("spool read: " + err.Error())
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
// released. A missing spool is not an error.
func (s *Spools) Release(spoolID string) error {
	if !model.ValidHexID(spoolID) {
		return cursorInvalid("spool id is malformed")
	}
	err := os.Remove(filepath.Join(s.dir, spoolPrefix+spoolID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return internalErr("spool release: " + err.Error())
	}
	return nil
}

// Sweep removes spools whose header expiry has passed or whose header cannot
// be read, and reports the bytes still in use. Callers run it on a schedule
// and at startup recovery, not per query.
func (s *Spools) Sweep() (liveBytes int64, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, internalErr("spool sweep: " + err.Error())
	}
	now := s.now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		if expiry, ok := spoolExpiry(path); !ok || !now.Before(expiry) {
			os.Remove(path)
			continue
		}
		if info, err := e.Info(); err == nil {
			liveBytes += info.Size()
		}
	}
	return liveBytes, nil
}

func spoolExpiry(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	head, err := readFrame(bufio.NewReader(f))
	if err != nil {
		return time.Time{}, false
	}
	var h SpoolHeader
	if json.Unmarshal(head, &h) != nil || h.Version != spoolVersion {
		return time.Time{}, false
	}
	return h.ExpiresAt, true
}

func (s *Spools) usedBytes() (int64, error) {
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
