package pagination

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	// maxSpoolChunkBytes bounds one FRAME so a reader never allocates from an
	// unbounded length prefix. A record larger than this is split across
	// continuation frames (see writeFrame) rather than refused: a search hit
	// is JSON, so truncating it corrupts it, and refusing it would fail the
	// whole query over one large record.
	maxSpoolChunkBytes = 1 << 20
	// frameContinues is the high bit of the 4-byte length prefix, set on every
	// chunk of a record that has another chunk after it. The remaining 31 bits
	// carry the chunk length, which maxSpoolChunkBytes bounds far below that.
	frameContinues  = uint32(1) << 31
	frameLengthMask = frameContinues - 1
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
//
// maxBytes of zero or less is UNLIMITED: nothing this store holds is refused
// for want of budget. That is the scale posture's spelling of an unset bound --
// only a bound an operator SET may refuse work -- and it is what lets
// resources.max_temp_bytes default to unlimited without this constructor
// rejecting the configuration it derives from. Accounting is unchanged either
// way: used still tracks what is live, because that figure is reported by the
// resource envelope whether or not anything is capped.
func NewSpools(dir string, maxBytes int64, leases LeaseStore) (*Spools, error) {
	if maxBytes < 0 {
		maxBytes = 0
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

// ByteBudget is the shared continuation byte budget, or zero when this store
// is UNLIMITED. It is read by callers that size a piece of continuation state
// they can make smaller rather than be refused for -- an accelerator, such as
// a membership summary, whose size is a choice and never a correctness
// property. The budget is fixed at construction, so this needs no lock.
func (s *Spools) ByteBudget() int64 { return s.maxBytes }

// reserve claims n bytes of the shared budget for spool id or fails without
// claiming any.
func (s *Spools) reserve(id string, n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// An unlimited store (maxBytes zero) refuses nothing: the reservation is
	// still recorded, so Release, Sweep and the resource envelope keep seeing
	// the same numbers, but no page ends because of them.
	if s.maxBytes > 0 && s.used+n > s.maxBytes {
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

// recordCeiling bounds ONE assembled record on the way back in. It is the
// shared budget while there is one -- no record can exceed what the budget
// admitted when it was written -- and, on an unlimited store, no ceiling at
// all: a reader that refused every record because nothing was capped would
// turn "unlimited" into "reads nothing". The reassembly loop still terminates
// on a corrupt length chain without it, because every continued chunk must be
// a full maxSpoolChunkBytes and the file is finite, and each individual
// allocation stays bounded by that chunk size.
func (s *Spools) recordCeiling() int64 {
	if s.maxBytes > 0 {
		return s.maxBytes
	}
	return math.MaxInt64
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
	// pending is what the record currently being written has put in the
	// buffer, so a fault part way through a chunked record returns only the
	// reservation the record did not use.
	pending int64
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

// Written is the byte offset one past everything appended so far, counting the
// header frame Create wrote. It is the position a later OpenAt seeks to, which
// is how a writer that lays two sections into one spool records where the
// second begins without reading the file back.
func (sp *Spool) Written() int64 { return sp.written }

// Append writes one record, split across continuation frames when it exceeds
// one frame. No record size is refused: the only limit is the shared byte
// budget, and exceeding that is a typed limit, never a silent truncation.
//
// A record is admitted against the shared budget whole or not at all: the whole
// chain is reserved before any of it is written, so a record the budget cannot
// hold leaves nothing behind. A reader that meets a half-written chain -- which
// only a disk fault can produce -- reports corruption rather than serving a
// truncated record.
func (sp *Spool) Append(record []byte) error {
	if sp.w == nil {
		return internalErr("spool is not open for writing")
	}
	return sp.writeFrame(record)
}

// writeFrame emits p as one or more frames. Every chunk but the last is
// exactly maxSpoolChunkBytes and carries frameContinues, which is what lets a
// reader bound each allocation and detect a chain that ends early.
//
// The WHOLE chain is reserved against the shared budget before any of it is
// written, which is what makes the record atomic against the budget: a record
// the budget cannot hold is refused before a byte of it exists, rather than
// leaving a continued chunk with no successor behind. A write fault mid-chain
// still leaves a partial chain, but its bytes are unreserved and the spool is
// discarded by its caller, exactly as before chunking.
func (sp *Spool) writeFrame(p []byte) error {
	need := frameBytes(int64(len(p)))
	if err := sp.owner.reserve(sp.header.SpoolID, need); err != nil {
		return err
	}
	// An empty record is one empty frame, so a reader sees it rather than
	// nothing; the loop below would emit no frame at all for it.
	for first := true; first || len(p) > 0; first = false {
		chunk := p
		more := false
		if len(chunk) > maxSpoolChunkBytes {
			chunk, more = chunk[:maxSpoolChunkBytes], true
		}
		if err := sp.writeChunk(chunk, more); err != nil {
			sp.owner.unreserve(sp.header.SpoolID, need-sp.pending)
			sp.pending = 0
			return err
		}
		p = p[len(chunk):]
	}
	sp.written += need
	sp.pending = 0
	return nil
}

// frameBytes is how many bytes on disk one record of n payload bytes occupies:
// its chunks plus their four-byte length prefixes. Writer and reader share it
// -- writeFrame reserves it, OpenAt accounts a record read with it -- so a
// byte offset a cursor carries is the same arithmetic on both sides and cannot
// drift as the framing changes.
func frameBytes(n int64) int64 {
	chunks := n/maxSpoolChunkBytes + 1
	if n > 0 && n%maxSpoolChunkBytes == 0 {
		// An exact multiple needs no extra short chunk.
		chunks--
	}
	return n + 4*chunks
}

// writeChunk emits one frame against the reservation writeFrame already took.
// pending tracks what this record has actually written, so a fault mid-chain
// returns only the reservation for the bytes that never reached the buffer.
func (sp *Spool) writeChunk(p []byte, more bool) error {
	header := uint32(len(p))
	if more {
		header |= frameContinues
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], header)
	if _, err := sp.w.Write(length[:]); err != nil {
		return internalErr("spool write: " + err.Error())
	}
	sp.pending += 4
	if _, err := sp.w.Write(p); err != nil {
		return internalErr("spool write: " + err.Error())
	}
	sp.pending += int64(len(p))
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

// ErrStopSpool ends a spool read early from inside the record callback. It is
// not a failure: OpenAt returns the offset it stopped at and a nil error, which
// is what lets a reader take one PAGE off a spool without streaming the rest of
// it -- the difference between O(page) and O(remaining) work per page.
var ErrStopSpool = errors.New("pagination: stop reading this spool")

// Open validates that the spool named by cursor c exists, was written for
// exactly this lease, generation and query, and that its lease is still live
// at now, then streams its records to fn in order. The header is checked
// before any record is read; liveness comes from the lease store, so a renewed
// lease keeps its spool and a released one ends it.
func (s *Spools) Open(ctx context.Context, c Cursor, now time.Time, fn func(record []byte) error) error {
	_, err := s.OpenAt(ctx, c, now, 0, fn)
	return err
}

// OpenAt is Open starting at a byte offset rather than at the first record, and
// it reports the offset one past the last record it handed to fn. An offset of
// zero (or less) starts at the first record after the header.
//
// A spool is written once and read by many pages now: each page seeks to where
// the previous one stopped, takes its own records and returns ErrStopSpool, so
// a page's cost is its own page and never the remainder behind it. The offset
// is server-minted state carried in a SIGNED cursor, never a caller's choice,
// and it is only ever a position this store reported; a tampered one lands off
// a frame boundary and readFrame reports corruption rather than serving
// invented records.
//
// The header frame is always read and validated first -- binding, version and
// lease liveness -- before the seek, so an offset can never be used to skip the
// checks that decide whether this cursor may read this spool at all. The seek
// then installs a FRESH bufio.Reader: the one that read the header has already
// buffered past it, so reusing it would place the offset wrong.
func (s *Spools) OpenAt(ctx context.Context, c Cursor, now time.Time, offset int64,
	fn func(record []byte) error) (int64, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if c.SpoolID == "" {
		return 0, cursorInvalid("cursor names no spool")
	}
	f, err := os.Open(filepath.Join(s.dir, spoolPrefix+c.SpoolID))
	if errors.Is(err, os.ErrNotExist) {
		return 0, cursorInvalid("continuation state has expired or was released")
	}
	if err != nil {
		return 0, internalErr("spool open: " + err.Error())
	}
	defer f.Close()
	r := bufio.NewReader(f)
	h, at, err := readHeader(r, s.recordCeiling())
	if err != nil {
		return 0, err
	}
	if h.SpoolID != c.SpoolID || h.LeaseID != c.LeaseID || h.GenerationID != c.GenerationID ||
		h.AnalysisKey != c.AnalysisKey || h.QueryHash != c.QueryHash {
		return 0, cursorInvalid("spool does not belong to this cursor")
	}
	expiry, err := s.leases.LeaseExpiry(ctx, h.LeaseID)
	if err != nil {
		return 0, err
	}
	if !now.Before(expiry) {
		return 0, cursorInvalid("continuation state has expired")
	}
	if offset > at {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return 0, internalErr("spool seek: " + err.Error())
		}
		r, at = bufio.NewReader(f), offset
	}
	for {
		rec, err := readFrame(r, s.recordCeiling())
		if errors.Is(err, io.EOF) {
			return at, nil
		}
		if err != nil {
			return 0, err
		}
		at += frameBytes(int64(len(rec)))
		if err := fn(rec); err != nil {
			if errors.Is(err, ErrStopSpool) {
				return at, nil
			}
			return 0, err
		}
	}
}

// readHeader reads and validates the first frame. A file that ends before a
// header is a typed corruption, never a bare io.EOF leaking to the caller.
// It also reports the byte offset one past the header frame, which is where
// the first record of the spool begins.
func readHeader(r *bufio.Reader, max int64) (SpoolHeader, int64, error) {
	head, err := readFrame(r, max)
	if errors.Is(err, io.EOF) {
		return SpoolHeader{}, 0, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool has no header"}
	}
	if err != nil {
		return SpoolHeader{}, 0, err
	}
	var h SpoolHeader
	if err := json.Unmarshal(head, &h); err != nil || h.Version != spoolVersion {
		return SpoolHeader{}, 0, cursorInvalid("spool header is not readable")
	}
	return h, frameBytes(int64(len(head))), nil
}

// readFrame reassembles one record from its chain of frames. It returns io.EOF
// only at a clean RECORD boundary: a chain that ends mid-record is corruption,
// not the end of the file, because treating it as the end would silently drop
// the tail of an answer.
//
// max bounds the assembled record. Chunking removes the per-frame ceiling as a
// record ceiling, so the assembler needs one of its own or a corrupt length
// chain allocates without limit; the honest ceiling is the spool's whole byte
// budget, since no record can exceed what the budget admitted when it was
// written. Each individual allocation is still bounded by maxSpoolChunkBytes.
func readFrame(r *bufio.Reader, max int64) ([]byte, error) {
	var rec []byte
	for {
		var length [4]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			if errors.Is(err, io.EOF) && rec == nil {
				return nil, io.EOF
			}
			return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool is truncated"}
		}
		header := binary.BigEndian.Uint32(length[:])
		more := header&frameContinues != 0
		n := header & frameLengthMask
		if n > maxSpoolChunkBytes {
			return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool frame length exceeds its bound"}
		}
		// Only the LAST chunk of a record may be short. Requiring every
		// continued chunk to be full is what makes the assembled length grow
		// by a fixed step, so a corrupt chain cannot loop forever on empty
		// continued frames.
		if more && n != maxSpoolChunkBytes {
			return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool record chunk is short but claims a continuation"}
		}
		if int64(len(rec))+int64(n) > max {
			return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool record exceeds the spool byte budget"}
		}
		chunk := make([]byte, n)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, &model.Error{Code: model.CodeStorageCorrupt, Message: "spool is truncated"}
		}
		rec = append(rec, chunk...)
		if !more {
			return rec, nil
		}
	}
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

// remove deletes spool id's file -- or, for state adopted with AdoptDir, its
// whole directory -- and returns its reservation. A removal failure leaves the
// accounting untouched: the bytes are still on disk.
func (s *Spools) remove(id string) error {
	path := filepath.Join(s.dir, spoolPrefix+id)
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = entryBytes(path, info)
	}
	// RemoveAll rather than Remove: a retained state directory is one entry of
	// this store like any spool file, and leaving it behind would leak the
	// whole search state of every page that ended early.
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
		// A directory is NOT skipped: AdoptDir retains externally built state
		// under the same naming, and skipping it would leave a search's whole
		// scratch on disk for every page that ended early -- its lease would
		// expire and nothing would ever remove it.
		if !strings.HasPrefix(e.Name(), spoolPrefix) {
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
		h, ok := s.spoolHeader(path)
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
			if rerr := os.RemoveAll(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				errs = append(errs, internalErr("spool sweep: "+rerr.Error()))
				live[id] = entryBytes(path, info)
			}
			continue
		}
		live[id] = entryBytes(path, info)
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

// spoolHeader reads one store entry's header. A retained state directory keeps
// it in a file inside itself, in the same frame shape a spool file's first
// frame has, so one reader answers for both and a sweep learns a directory's
// lease exactly as it learns a file's.
func (s *Spools) spoolHeader(path string) (SpoolHeader, bool) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, spoolDirHeader)
	}
	f, err := os.Open(path)
	if err != nil {
		return SpoolHeader{}, false
	}
	defer f.Close()
	h, _, err := readHeader(bufio.NewReader(f), s.recordCeiling())
	return h, err == nil
}

func (s *Spools) diskBytes() (int64, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, internalErr("spool directory: " + err.Error())
	}
	var used int64
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			used += entryBytes(filepath.Join(s.dir, e.Name()), info)
		}
	}
	return used, nil
}
