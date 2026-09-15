package pagination

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A retained STATE DIRECTORY is continuation state whose producer builds it as
// files of its own rather than as a record stream -- the shortest-path search's
// external-memory scratch is the one such producer. It is named, bound,
// validated, released and swept exactly like a spool file: same `spool-`
// naming, same header, same lease, same shared byte budget. Only the shape of
// the bytes inside differs, which is why the lifecycle below adds two entry
// points and changes nothing about how a spool lives.
//
// The alternative -- serializing the scratch into a record spool on every page
// stop and writing it back on the next one -- was rejected: it costs a full
// copy of the search state per page and charges the whole state against
// resources.max_temp_bytes, so a large search would stop being able to mint a
// continuation at all. Adoption is a rename within the store's own directory,
// which is atomic and O(1).
//
// The header lives in a file INSIDE the directory, in the same length-prefixed
// frame a spool file's first frame uses, so readHeader serves both shapes and
// a sweep reads a directory's lease the same way it reads a file's.
const spoolDirHeader = "header"

// AdoptDir places the state directory dir under this store's retention, bound
// to cursor c, and returns the spool id that names it. dir must be a directory
// the caller owns; on success the store owns it and the caller must not touch
// it again except through OpenDir.
//
// The bytes are reserved against the shared budget before the rename, so a
// directory the budget cannot hold is refused with the budget-exhausted detail
// and the caller ends its answer without a continuation instead of quietly
// exceeding resources.max_temp_bytes.
func (s *Spools) AdoptDir(c Cursor, dir string) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	id, err := model.NewRandomID()
	if err != nil {
		return "", err
	}
	if err := writeDirHeader(dir, id, c); err != nil {
		return "", err
	}
	size, err := dirBytes(dir)
	if err != nil {
		return "", err
	}
	if err := s.reserve(id, size); err != nil {
		return "", err
	}
	if err := os.Rename(dir, filepath.Join(s.dir, spoolPrefix+id)); err != nil {
		s.unreserve(id, size)
		return "", internalErr("spool adopt: " + err.Error())
	}
	return id, nil
}

// ReadoptDir rebinds the retained directory prevID already names to cursor c,
// under a fresh id, charging the shared budget only the bytes it has GROWN by.
//
// It exists because a paged walk extends one directory page after page.
// AdoptDir measures the whole directory and reserves it again while the
// previous reservation is released only afterwards, so the budget held roughly
// TWICE the cumulative retained bytes at every page boundary -- and a walk
// whose state is append-only would still stop being able to mint a
// continuation at half the budget it actually needs. Here the previous
// reservation is transferred to the new id inside one critical section, so the
// entry is charged its real size exactly once and only the delta has to fit.
//
// The caller must NOT hold the directory open for write across this call for
// anything it still needs named: the rename moves the entry, exactly as
// AdoptDir does.
func (s *Spools) ReadoptDir(c Cursor, prevID string) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if !model.ValidHexID(prevID) {
		return "", cursorInvalid("spool id is malformed")
	}
	from := filepath.Join(s.dir, spoolPrefix+prevID)
	if info, err := os.Stat(from); err != nil || !info.IsDir() {
		return "", cursorInvalid("continuation state has expired or was released")
	}
	id, err := model.NewRandomID()
	if err != nil {
		return "", err
	}
	// The RESERVATION is taken before anything on disk changes, and the header
	// is stamped only once the directory is this cursor's to keep.
	//
	// Stamping first was a way to lose a walk. A re-adoption can be refused --
	// a grown directory that no longer fits the shared byte budget answers the
	// retryable CTX_RESOURCE_LIMIT -- and the refusal left the directory still
	// named spool-<prevID>, still reserved under it, but carrying the NEW id
	// and lease in its header. OpenDir compares the two, so the cursor the
	// caller was told to present again could never open it: a retryable
	// failure had silently become a permanent one.
	frame, err := dirHeaderFrame(id, c)
	if err != nil {
		return "", err
	}
	// The header file is part of what dirBytes measured, so the reservation is
	// corrected by the difference between the header being replaced and the one
	// replacing it rather than by re-measuring after the write.
	prev, prevErr := os.ReadFile(filepath.Join(from, spoolDirHeader))
	if prevErr != nil {
		prev = nil
	}
	size, err := dirBytes(from)
	if err != nil {
		return "", err
	}
	size += int64(len(frame)) - int64(len(prev))
	delta, err := s.transfer(prevID, id, size)
	if err != nil {
		return "", err
	}
	if err := replaceDirHeader(from, frame); err != nil {
		s.transferBack(id, prevID, delta)
		return "", err
	}
	if err := os.Rename(from, filepath.Join(s.dir, spoolPrefix+id)); err != nil {
		// Put the directory back exactly as it was found: the accounting AND
		// the binding prevID's cursor opens it by.
		if prev != nil {
			_ = replaceDirHeader(from, prev)
		}
		s.transferBack(id, prevID, delta)
		return "", internalErr("spool adopt: " + err.Error())
	}
	return id, nil
}

// transfer moves prevID's reservation onto id and adjusts it to size, claiming
// only the difference against the budget. It returns that difference, which is
// what transferBack needs to undo it. A shrunk directory gives bytes back; a
// grown one that does not fit is refused with the budget-exhausted detail, and
// nothing is moved.
func (s *Spools) transfer(prevID, id string, size int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delta := size - s.reserved[prevID].bytes
	if s.maxBytes > 0 && delta > 0 && s.used+delta > s.maxBytes {
		return 0, (&model.Error{Code: model.CodeResourceLimit, Retryable: true,
			Message:     "query spool exceeds its disk budget",
			Remediation: "narrow the query, retry after outstanding cursors expire, or raise resources.max_temp_bytes"}).
			WithDetail(budgetDetailKey, budgetDetailValue)
	}
	s.used += delta
	if s.used < 0 {
		s.used = 0
	}
	delete(s.reserved, prevID)
	s.seq++
	s.reserved[id] = reservation{seq: s.seq, bytes: size}
	return delta, nil
}

// transferBack undoes transfer when the rename that was to follow it failed:
// the directory is still where it was, still under its old id -- ReadoptDir
// puts its header back to match -- so the entry must go back to that id AND the
// budget must give the delta back. Restoring
// the map alone would leave s.used inflated by the delta until the next Sweep,
// which is the accounting drift ReadoptDir exists to remove.
func (s *Spools) transferBack(id, prevID string, delta int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bytes := s.reserved[id].bytes - delta
	delete(s.reserved, id)
	s.used -= delta
	if s.used < 0 {
		s.used = 0
	}
	s.seq++
	s.reserved[prevID] = reservation{seq: s.seq, bytes: bytes}
}

// writeDirHeader stamps a retained directory with the binding it is being
// adopted under. The header is written BEFORE the rename, so a directory that
// carries the spool naming always carries a readable header: a sweep never
// meets one mid-adoption and never has to fall back to the headerless grace
// window.
func writeDirHeader(dir, id string, c Cursor) error {
	frame, err := dirHeaderFrame(id, c)
	if err != nil {
		return err
	}
	return replaceDirHeader(dir, frame)
}

// dirHeaderFrame is that stamp as bytes, without writing it. ReadoptDir needs
// it separately: it must know the frame's size before it reserves, and it must
// not touch the directory until the reservation has been granted.
func dirHeaderFrame(id string, c Cursor) ([]byte, error) {
	h := SpoolHeader{Version: spoolVersion, SpoolID: id, LeaseID: c.LeaseID, GenerationID: c.GenerationID,
		AnalysisKey: c.AnalysisKey, QueryHash: c.QueryHash, ExpiresAt: c.ExpiresAt.UTC()}
	head, err := json.Marshal(h)
	if err != nil {
		return nil, internalErr("spool header: " + err.Error())
	}
	frame := make([]byte, 4+len(head))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(head)))
	copy(frame[4:], head)
	return frame, nil
}

// replaceDirHeader puts frame in place ATOMICALLY: a directory always carries
// either the whole header it had or the whole one replacing it, never the
// prefix of a write that failed part-way. That is what lets ReadoptDir put the
// previous binding back when the rename it was preparing for does not happen.
func replaceDirHeader(dir string, frame []byte) error {
	tmp := filepath.Join(dir, spoolDirHeader+".new")
	if err := os.WriteFile(tmp, frame, 0o600); err != nil {
		return internalErr("spool adopt: " + err.Error())
	}
	if err := os.Rename(tmp, filepath.Join(dir, spoolDirHeader)); err != nil {
		_ = os.Remove(tmp)
		return internalErr("spool adopt: " + err.Error())
	}
	return nil
}

// OpenDir validates that the retained directory named by cursor c exists, was
// adopted for exactly this lease, generation and query, and that its lease is
// still live at now, then returns its path. It is Open's check, without the
// record stream: the producer reads the directory's own files itself.
func (s *Spools) OpenDir(ctx context.Context, c Cursor, now time.Time) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if c.SpoolID == "" {
		return "", cursorInvalid("cursor names no spool")
	}
	path := filepath.Join(s.dir, spoolPrefix+c.SpoolID)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", cursorInvalid("continuation state has expired or was released")
	}
	h, ok := s.spoolHeader(path)
	if !ok {
		return "", cursorInvalid("continuation state has expired or was released")
	}
	if h.SpoolID != c.SpoolID || h.LeaseID != c.LeaseID || h.GenerationID != c.GenerationID ||
		h.AnalysisKey != c.AnalysisKey || h.QueryHash != c.QueryHash {
		return "", cursorInvalid("spool does not belong to this cursor")
	}
	expiry, err := s.leases.LeaseExpiry(ctx, h.LeaseID)
	if err != nil {
		return "", err
	}
	if !now.Before(expiry) {
		return "", cursorInvalid("continuation state has expired")
	}
	return path, nil
}

// dirBytes is a retained directory's size: the sum of the regular files under
// it, which is what it costs the shared budget.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, internalErr("spool directory size: " + err.Error())
	}
	return total, nil
}

// entryBytes charges one live store entry, file or directory. A directory's own
// FileInfo size is a filesystem detail rather than the bytes it holds, so the
// contents are measured; a directory that disappears under the walk charges
// nothing, which is what Release already did to it.
func entryBytes(path string, info fs.FileInfo) int64 {
	if !info.IsDir() {
		return info.Size()
	}
	n, err := dirBytes(path)
	if err != nil {
		return 0
	}
	return n
}
