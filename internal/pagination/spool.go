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

	// mu guards used, the bytes every live spool has reserved, and reserved,
	// the per-spool reservations taken by this process. Reservations are
	// taken before a write, so concurrent spools share one budget instead of
	// each measuring the directory and racing past the cap together, and a
	// spool's bytes are returned as the amount it reserved, not as whatever
	// had reached disk when it was removed.
	mu       sync.Mutex
	used     int64
	reserved map[string]reservation
	// seq numbers reservations in creation order so a sweep can tell a spool
	// created after its directory snapshot from one whose file is gone.
	seq int64
}

// reservation is what one spool has claimed from the budget and when its
// first claim was made.
type reservation struct {
	bytes int64
	seq   int64
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

// budgetDetailKey / budgetDetailValue mark the one error a caller must be able
// to tell apart from a disk fault: the shared spool budget is full. A caller
// that was spooling the TAIL of an answer ends the page there instead of
// failing the whole query, which is why this is a detail on the typed error
// rather than a distinct code -- the code and remediation an operator sees are
// unchanged.
const (
	budgetDetailKey   = "spool_budget"
	budgetDetailValue = "exhausted"
)

// IsBudgetExhausted reports whether err is the shared spool byte budget running
// out, as opposed to a disk fault, a corrupt spool or a programming error. Only
// this one is safe to degrade into "the page ends here"; everything else is a
// real failure and must surface.
func IsBudgetExhausted(err error) bool {
	var e *model.Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Details[budgetDetailKey] == budgetDetailValue
}

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
	s := &Spools{dir: dir, maxBytes: maxBytes, leases: leases, reserved: map[string]reservation{}}
	used, err := s.diskBytes()
	if err != nil {
		return nil, err
	}
	s.used = used
	return s, nil
}

// reserve claims n bytes of the shared budget for spool id or fails without
// claiming any.
func (s *Spools) reserve(id string, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used+n > s.maxBytes {
		return (&model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message:     "query spool exceeds its disk budget",
			Remediation: "narrow the query, retry after outstanding cursors expire, or raise resources.max_temp_bytes"}).
			WithDetail(budgetDetailKey, budgetDetailValue)
	}
	s.used += n
	r, ok := s.reserved[id]
	if !ok {
		s.seq++
		r.seq = s.seq
	}
	r.bytes += n
	s.reserved[id] = r
	return nil
}

// unreserve gives n bytes of spool id's reservation back after a failed write.
func (s *Spools) unreserve(id string, n int64) {
	s.mu.Lock()
	s.used -= n
	r := s.reserved[id]
	r.bytes -= n
	if r.bytes <= 0 {
		delete(s.reserved, id)
	} else {
		s.reserved[id] = r
	}
	if s.used < 0 {
		s.used = 0
	}
	s.mu.Unlock()
}

// forget returns everything spool id reserved once its file is gone. A spool
// created by an earlier process has no reservation here; its on-disk size,
// which was counted at NewSpools or the last Sweep, is returned instead.
func (s *Spools) forget(id string, onDisk int64) {
	s.mu.Lock()
	n := onDisk
	if r, ok := s.reserved[id]; ok {
		n = r.bytes
	}
	delete(s.reserved, id)
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
	sp.owner.forget(sp.header.SpoolID, sp.written)
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
	if err := sp.owner.reserve(sp.header.SpoolID, need); err != nil {
		return err
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(p)))
	if _, err := sp.w.Write(length[:]); err != nil {
		sp.owner.unreserve(sp.header.SpoolID, need)
		return internalErr("spool write: " + err.Error())
	}
	if _, err := sp.w.Write(p); err != nil {
		sp.owner.unreserve(sp.header.SpoolID, need)
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
// released, returning the bytes it reserved to the budget. A missing spool is
// not an error.
func (s *Spools) Release(spoolID string) error {
	if !model.ValidHexID(spoolID) {
		return cursorInvalid("spool id is malformed")
	}
	return s.remove(spoolID)
}

// remove deletes spool id's file and returns its reservation. A removal
// failure leaves the accounting untouched: the bytes are still on disk.
func (s *Spools) remove(id string) error {
	path := filepath.Join(s.dir, spoolPrefix+id)
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return internalErr("spool release: " + err.Error())
	}
	s.forget(id, size)
	return nil
}

// Sweep removes spools whose lease is gone or expired at now, and files with
// no readable header that are older than the create grace window, then
// reconciles the budget with what is live: each remaining spool counts as the
// larger of its on-disk size and the bytes this process has reserved for it,
// so an open spool's buffered frames are never given away. A file that cannot
// be removed stays counted and its error is returned with the live total.
// Callers run it on a schedule and at startup recovery, not per query.
func (s *Spools) Sweep(ctx context.Context, now time.Time) (liveBytes int64, err error) {
	// The directory listing is a snapshot; a spool created after it is not in
	// the listing but is live, so the reconciliation below keeps every
	// reservation made from this point on.
	s.mu.Lock()
	start := s.seq
	s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, internalErr("spool sweep: " + err.Error())
	}
	var errs []error
	live := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		id := strings.TrimPrefix(e.Name(), spoolPrefix)
		path := filepath.Join(s.dir, e.Name())
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, internalErr("spool sweep: " + err.Error())
		}
		h, ok := spoolHeader(path)
		dead := false
		if !ok {
			// No header yet: a Create in progress within the grace window is
			// live; anything older is a crashed Create.
			dead = now.Sub(info.ModTime()) > headerlessGrace
		} else {
			expiry, err := s.leases.LeaseExpiry(ctx, h.LeaseID)
			if err != nil {
				var typed *model.Error
				if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
					return 0, err
				}
				dead = true
			} else {
				dead = !now.Before(expiry)
			}
		}
		if dead {
			if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				errs = append(errs, internalErr("spool sweep: "+rerr.Error()))
				live[id] = info.Size()
			}
			continue
		}
		live[id] = info.Size()
	}
	s.mu.Lock()
	for id, r := range s.reserved {
		if _, ok := live[id]; ok {
			continue
		}
		if r.seq > start {
			// Created after the listing: live, nothing on disk observed yet.
			live[id] = 0
			continue
		}
		delete(s.reserved, id)
	}
	for id, size := range live {
		liveBytes += max(size, s.reserved[id].bytes)
	}
	s.used = liveBytes
	s.mu.Unlock()
	return liveBytes, errors.Join(errs...)
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
