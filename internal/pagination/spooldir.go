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
	h := SpoolHeader{Version: spoolVersion, SpoolID: id, LeaseID: c.LeaseID, GenerationID: c.GenerationID,
		AnalysisKey: c.AnalysisKey, QueryHash: c.QueryHash, ExpiresAt: c.ExpiresAt.UTC()}
	head, err := json.Marshal(h)
	if err != nil {
		return "", internalErr("spool header: " + err.Error())
	}
	frame := make([]byte, 4+len(head))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(head)))
	copy(frame[4:], head)
	// The header is written BEFORE the rename, so a directory that carries the
	// spool naming always carries a readable header: a sweep never meets one
	// mid-adoption and never has to fall back to the headerless grace window.
	if err := os.WriteFile(filepath.Join(dir, spoolDirHeader), frame, 0o600); err != nil {
		return "", internalErr("spool adopt: " + err.Error())
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
