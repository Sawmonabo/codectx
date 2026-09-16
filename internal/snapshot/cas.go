package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/source"
)

// CAS is the local content-addressed store of Section 10.3: raw immutable blob
// files named by their validated SHA-256, published atomically and never
// replaced. Integrity metadata (block digests, line checkpoints) is not stored
// here; it is persisted through storage and handed back to Open and ReadRange,
// which verify the bytes they expose against it.
type CAS struct {
	dir string
	tmp string
}

// OpenCAS opens or creates the store at dir (0700).
func OpenCAS(dir string) (*CAS, error) {
	if !filepath.IsAbs(dir) {
		return nil, invalid("the CAS directory must be an absolute path")
	}
	tmp := filepath.Join(dir, casTmpDirName)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, ioError("CAS directory", err)
	}
	return &CAS{dir: dir, tmp: tmp}, nil
}

// path derives the blob location from a validated digest and nothing else.
func (c *CAS) path(hash string) (string, error) {
	if !model.ValidHexID(hash) {
		return "", integrity("content hash is not a lowercase SHA-256 hex digest")
	}
	return filepath.Join(c.dir, hash[:2], hash), nil
}

// Has reports whether the blob file for hash is present. Presence is not
// verification; Open and ReadRange are.
func (c *CAS) Has(hash string) (bool, error) {
	p, err := c.path(hash)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, ioError("CAS stat", err)
}

// Put streams r into the store and makes it durable before returning. It is
// the single-blob form of a batch: callers that publish many blobs under one
// generation use NewBatch, which pays the durability cost once for the group.
// Repair is the caller that needs this form, because it publishes into a
// manifest that is already committed and so has no later barrier to order
// against.
func (c *CAS) Put(ctx context.Context, r io.Reader) (model.BlobRecord, error) {
	return c.put(ctx, r, "")
}

// put is Put with an optional expected digest: when want is set, content whose
// digest differs is discarded unpublished and reported as an integrity
// failure. Repair uses this so a wrong reconstruction never enters the store.
func (c *CAS) put(ctx context.Context, r io.Reader, want string) (model.BlobRecord, error) {
	b := c.NewBatch()
	defer b.Discard()
	rec, err := b.put(ctx, r, want)
	if err != nil {
		return model.BlobRecord{}, err
	}
	if err := b.Barrier(ctx); err != nil {
		return model.BlobRecord{}, err
	}
	return rec, nil
}

// Batch stages blobs for one publication and makes them durable as a group.
//
// Section 10.3 requires that everything a published generation names is on
// disk before that generation is visible; it does not require each blob to be
// durable the instant it is written. A batch keeps that invariant and pays for
// it once: bytes are written to private temporaries under <cas>/tmp, the
// temporaries are fsynced in parallel groups, and only a synced temporary is
// linked into its bucket. Barrier flushes whatever is still staged and then
// fsyncs each bucket directory the batch touched -- at most 256, one per
// two-hex-digit prefix, instead of one per blob. The caller commits the
// manifest or generation that names the blobs only after Barrier returns.
//
// Publishing after the fsync rather than ahead of it is what keeps a crash
// recoverable: an interrupted batch leaves nothing in the buckets, only
// temporaries that Sweep already removes. A bucket entry whose bytes had not
// reached the disk would survive as a short file and fail every later capture
// of the same content as an integrity error.
//
// A batch is used by one goroutine at a time, like the capture that owns it.
type Batch struct {
	c *CAS
	// window is how many staged temporaries are held open before a group is
	// flushed, and parallel how many fsyncs run at once. Both are derived from
	// the hardware. Neither bounds how much work a batch accepts -- every blob
	// staged is synced before Barrier returns -- so this is a batch size in
	// the sense a read page is, not a limit on a repository's size. Peak
	// descriptors and peak memory are a function of the window, not of the
	// number of files captured.
	window   int
	parallel int
	open     []staged
	// dirty is the set of bucket directories this batch has published into
	// since the last barrier. It doubles as the record of which buckets have
	// been created, so MkdirAll runs once per bucket rather than once per
	// blob. Barrier clears it, so a bucket written to again afterwards is
	// created (a no-op) and synced again, which is what a second barrier owes.
	dirty map[string]struct{}
	// pending is the digests staged since the last flush. Content the capture
	// meets twice inside one window -- a vendored copy, a repeated licence --
	// is written once and synced once. It is bounded by the window like the
	// open descriptors are, and cleared with them.
	pending map[string]struct{}
	// onSync reports each blob as it becomes durable and published. It exists
	// for the durability-barrier test and is nil in production.
	onSync func(hash string)
}

// staged is one temporary awaiting its group flush. The descriptor stays open
// so the flush can fsync it without reopening: on Windows a read-only handle
// cannot be flushed at all.
type staged struct {
	f     *os.File
	final string
	hash  string
	size  int64
}

// NewBatch opens a batch against the store, sized from the machine.
func (c *CAS) NewBatch() *Batch {
	window, parallel := syncWindow()
	return &Batch{c: c, window: window, parallel: parallel, dirty: map[string]struct{}{}, pending: map[string]struct{}{}}
}

// maxSyncWindow caps the window on a machine with very many cores: beyond a
// few hundred concurrent fsyncs the journal commits they share stop getting
// cheaper and the open descriptors stop being free.
const maxSyncWindow = 1024

// syncWindow derives the group flush size and its parallelism from the CPU
// count and the process descriptor limit. A quarter of the limit leaves the
// rest for the worktree reads, the store and the provider processes.
func syncWindow() (window, parallel int) {
	parallel = max(runtime.NumCPU(), 1)
	window = min(parallel*32, maxSyncWindow)
	if lim := openFileLimit(); lim > 0 {
		window = min(window, lim/4)
	}
	window = max(window, 1)
	parallel = min(parallel, window)
	return window, parallel
}

// Put streams r into the store as Put does, but leaves it staged: the bytes
// are durable and the blob published only once Barrier returns.
func (b *Batch) Put(ctx context.Context, r io.Reader) (model.BlobRecord, error) {
	return b.put(ctx, r, "")
}

// put stages one blob, flushing the group first when the window is full.
func (b *Batch) put(ctx context.Context, r io.Reader, want string) (model.BlobRecord, error) {
	rec, tmp, final, err := b.c.stage(ctx, r, want)
	if err != nil {
		return model.BlobRecord{}, err
	}
	if tmp == nil {
		// Already published by an earlier generation, so the BYTES cost no
		// sync. The bucket DIRENT still does: the batch that published it may
		// have died before its own Barrier, leaving a file whose name is not
		// durable, and this batch is about to commit a manifest that names it.
		// Recording the bucket costs one fsync per two-hex prefix and is what
		// the pre-batch put did unconditionally. The directory exists (the
		// blob is in it), so the MkdirAll this skips is not owed.
		b.dirty[filepath.Dir(final)] = struct{}{}
		return rec, nil
	}
	if _, ok := b.pending[rec.Hash]; ok {
		// The identical blob is already staged in this group and will be
		// published and synced by the same flush, before the same barrier.
		tmp.Close()
		paced.Remove(tmp.Name())
		return rec, nil
	}
	dir := filepath.Dir(final)
	if _, ok := b.dirty[dir]; !ok {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			tmp.Close()
			paced.Remove(tmp.Name())
			return model.BlobRecord{}, ioError("CAS bucket", err)
		}
		b.dirty[dir] = struct{}{}
	}
	b.open = append(b.open, staged{f: tmp, final: final, hash: rec.Hash, size: rec.Size})
	b.pending[rec.Hash] = struct{}{}
	if len(b.open) >= b.window {
		if err := b.flush(ctx); err != nil {
			return model.BlobRecord{}, err
		}
	}
	return rec, nil
}

// Barrier is the batch's single durability point: when it returns without
// error every blob staged through it is on disk and linked into its bucket,
// and every bucket entry naming one is on disk too. Nothing that names these
// blobs may be committed before it returns.
func (b *Batch) Barrier(ctx context.Context) error {
	if err := b.flush(ctx); err != nil {
		return err
	}
	// Sorted so the syncs are ordered the same way on every run, which makes a
	// failure reproducible rather than map-order dependent.
	for _, dir := range slices.Sorted(maps.Keys(b.dirty)) {
		if err := fslock.SyncDir(dir); err != nil {
			return ioError("CAS directory sync", err)
		}
	}
	clear(b.dirty)
	return nil
}

// Discard drops the temporaries of blobs staged but never made durable. It is
// the failure-path counterpart of Barrier, so an abandoned capture does not
// leave its temporaries for the next startup sweep to find; Sweep remains the
// backstop for a process that dies before either runs.
func (b *Batch) Discard() {
	for i := range b.open {
		b.open[i].f.Close()
		paced.Remove(b.open[i].f.Name())
	}
	b.open = b.open[:0]
	clear(b.pending)
}

// flush makes every staged temporary durable and publishes it. The fsyncs run
// concurrently on already-open descriptors: a journalling filesystem folds
// concurrent fsyncs into shared commits, which is the entire saving over
// syncing each blob where it was written. Publication is sequential because it
// is metadata only.
func (b *Batch) flush(ctx context.Context) error {
	if len(b.open) == 0 {
		return nil
	}
	group := b.open
	b.open = b.open[:0]
	clear(b.pending)
	defer func() {
		// Whatever happened, no descriptor and no temporary outlives the
		// group. A Close after a successful Close and a Remove after a rename
		// both fail harmlessly.
		for i := range group {
			group[i].f.Close()
			paced.Remove(group[i].f.Name())
		}
	}()
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	errs := make([]error, len(group))
	sem := make(chan struct{}, b.parallel)
	var wg sync.WaitGroup
	for i := range group {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			errs[i] = group[i].f.Sync()
		}(i)
	}
	wg.Wait()
	for i := range group {
		s := &group[i]
		if errs[i] != nil {
			return ioError("CAS sync", errs[i])
		}
		if err := s.f.Chmod(0o400); err != nil {
			return ioError("CAS chmod", err)
		}
		if err := s.f.Close(); err != nil {
			return ioError("CAS close", err)
		}
		if err := b.c.publish(s.f.Name(), s.final, s.size); err != nil {
			return err
		}
		if b.onSync != nil {
			b.onSync(s.hash)
		}
	}
	return nil
}

// stage streams r into a private temporary while computing the whole-file
// digest, the 64-KiB block digests and the sparse line checkpoints in one pass
// (source.BuildIndex). When want is set, content hashing to anything else is
// discarded unpublished and reported as an integrity failure. Content already
// published under the same digest is durable by construction, so the temporary
// is dropped and a nil file returned: the caller has nothing to sync. The
// returned record is what the caller persists with storage.PutBlob before any
// manifest names it.
func (c *CAS) stage(ctx context.Context, r io.Reader, want string) (model.BlobRecord, *os.File, string, error) {
	if err := ctx.Err(); err != nil {
		return model.BlobRecord{}, nil, "", model.Canceled(err)
	}
	tmp, err := os.CreateTemp(c.tmp, casTmpPrefix+"*")
	if err != nil {
		return model.BlobRecord{}, nil, "", ioError("CAS temporary file", err)
	}
	keep := false
	defer func() {
		if !keep {
			tmp.Close()
			paced.Remove(tmp.Name())
		}
	}()

	idx, err := source.BuildIndex(io.TeeReader(r, tmp))
	if err != nil {
		return model.BlobRecord{}, nil, "", ioError("CAS write", err)
	}
	if want != "" && idx.ContentHash != want {
		return model.BlobRecord{}, nil, "", integrity("reconstructed content hashes to a different digest than the manifest records")
	}
	rec := recordOf(idx)
	final, err := c.path(rec.Hash)
	if err != nil {
		return model.BlobRecord{}, nil, "", err
	}
	if _, err := os.Lstat(final); err == nil {
		if err := c.checkExisting(final, rec.Size); err != nil {
			return model.BlobRecord{}, nil, "", err
		}
		return rec, nil, final, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return model.BlobRecord{}, nil, "", ioError("CAS stat", err)
	}
	keep = true
	return rec, tmp, final, nil
}

// publish links tmp to final atomically. An existing final is the same
// content by construction of the name, so it is kept and only its size is
// checked. A filesystem without hard links falls back to a rename, attempted
// only while final is absent; a publisher racing into that window replaces
// the object with byte-identical content, which changes nothing a reader can
// observe. A temporary that vanished is the startup sweep running without the
// workspace lock held here (Repair): retryable, not corruption.
func (c *CAS) publish(tmp, final string, size int64) error {
	err := os.Link(tmp, final)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return c.checkExisting(final, size)
	}
	if _, statErr := os.Lstat(final); statErr == nil {
		return c.checkExisting(final, size)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return sweptTemporary()
	}
	if err := os.Rename(tmp, final); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return sweptTemporary()
		}
		return ioError("CAS publish", err)
	}
	return nil
}

func sweptTemporary() error {
	return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
		Message:     "the object's temporary file was removed by a concurrent recovery sweep before publication",
		Remediation: "retry; run repair under the workspace lock to exclude the sweep"}
}

func (c *CAS) checkExisting(final string, size int64) error {
	info, err := os.Lstat(final)
	if err != nil {
		return ioError("CAS stat", err)
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return integrity("a different object is already published under this content hash")
	}
	return nil
}

// recordOf converts the streaming index to the storage record. A source
// checkpoint marks where a line begins, so the line containing that byte
// starts at the byte itself.
func recordOf(idx source.Index) model.BlobRecord {
	rec := model.BlobRecord{Hash: idx.ContentHash, Size: int64(idx.Size), BlockDigests: idx.Blocks}
	for _, cp := range idx.Checkpoints {
		rec.LineCheckpoints = append(rec.LineCheckpoints, model.LineCheckpoint{ByteOffset: cp.Byte, LineNumber: cp.Line, LineStartByte: cp.Byte})
	}
	return rec
}

// indexOf is the inverse: the stored record as the source package's index,
// so block coverage and checkpoint lookup reuse one implementation.
func indexOf(rec model.BlobRecord) source.Index {
	idx := source.Index{Size: uint64(rec.Size), ContentHash: rec.Hash, Blocks: rec.BlockDigests}
	for _, cp := range rec.LineCheckpoints {
		idx.Checkpoints = append(idx.Checkpoints, source.Checkpoint{Byte: cp.LineStartByte, Line: cp.LineNumber})
	}
	return idx
}

// Remove deletes the published object for hash. It is the Section 10.4 grace
// protocol's last step and its only caller: internal/retention removes the
// object only after the store has already deleted the blobs row inside a
// transaction that rechecked reachability, so an object reaching here is
// unreachable through the index.
//
// An object that is already gone is not an error. The rows are the authority
// on what exists, the deletion is idempotent by design, and a crash between
// the row delete and this call must not make every later collection pass fail
// on the same leftover.
//
// It exists here rather than in internal/retention because c.path is the one
// derivation of an object's location; rebuilding <data>/cas/<hh>/<hash> in the
// collector would be a second copy of the layout that can drift from this one.
func (c *CAS) Remove(hash string) error {
	p, err := c.path(hash)
	if err != nil {
		return err
	}
	if err := paced.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ioError("CAS remove", err)
	}
	return nil
}

// SweepOrphans removes published objects that no blobs row names: content that
// reached the disk and whose naming commit never landed, which is the exact
// state a rolled-back generation tail (ADR-0004 Decision 1) and a crashed
// capture leave behind. It is the Section 10.4 collector's second reclaim, the
// counterpart of Remove: Remove deletes an object whose row the grace protocol
// just deleted, this deletes an object that never had a row at all.
//
// known reports which of a batch of hashes the index still holds a blobs row
// for, in ANY state -- quarantined and trashed rows are mid-grace, not orphans.
// It is a bare func rather than a named interface so internal/retention can
// declare this method structurally without importing this package. Its result
// is a SET: a hash present in the map is named, and absence means no row. An
// error from it aborts the chunk without removing anything, because an empty
// answer read as "nothing is named" would delete the whole store.
//
// grace is the safety margin for the publish-before-commit window: Put and
// Batch.Barrier make an object durable BEFORE the commit that names it, so an
// object younger than the grace may be a publication whose commit has not
// landed yet. The lock order in internal/retention (the workspace lock and the
// indexing mutex) is what excludes an indexing run from overlapping a pass;
// the mtime grace is the second line for the publishers that lock order does
// not cover -- Repair publishes outside the workspace lock -- and for a
// writer some later caller adds. Repair's own objects are additionally
// row-backed, so known already keeps them. The window is measured from first
// publication: the dedup path (checkExisting) deliberately leaves an existing
// object's mtime alone, so a re-published object does not restart its grace.
//
// batch bounds MEMORY, never work: the walk visits every bucket and every
// entry in it, reading one directory chunk and asking one known() batch at a
// time, so peak is a chunk plus its answer rather than the size of the store.
// Nothing is truncated and no pass leaves a known orphan behind for a later
// one -- except an entry a concurrent removal moved past the readdir cursor,
// which the next pass sees, which is why this is written as an idempotent
// re-walk rather than relying on any readdir-during-unlink guarantee.
//
// It returns how many objects it removed. An object that cannot be removed is
// joined into the error with the rest rather than aborting the walk: it is a
// leftover file, not lost source.
func (c *CAS) SweepOrphans(ctx context.Context, known func(context.Context, []string) (map[string]struct{}, error),
	now time.Time, grace time.Duration, batch int) (int64, error) {
	if batch <= 0 {
		batch = defaultOrphanBatch
	}
	buckets, err := os.ReadDir(c.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, ioError("CAS sweep", err)
	}
	var removed int64
	var errs []error
	for _, b := range buckets {
		// Only the two-hex-digit buckets path derives are objects. <cas>/tmp is
		// snapshot.Sweep's to reclaim and holds the temporaries a live batch is
		// still filling; widening this filter would delete them mid-publication.
		if !b.IsDir() || !isBucketName(b.Name()) {
			continue
		}
		n, err, stop := c.sweepBucket(ctx, filepath.Join(c.dir, b.Name()), known, now, grace, batch)
		removed += n
		if err != nil {
			errs = append(errs, err)
		}
		// A cancelled context or an index that cannot answer is not this
		// bucket's problem: walking the remaining 255 would re-ask a store
		// that is still broken and report the same failure up to 256 times.
		if stop {
			break
		}
	}
	return removed, errors.Join(errs...)
}

// defaultOrphanBatch is the chunk SweepOrphans holds in memory when the caller
// states none. Like retention's own batch limit it is a working-set size, not a
// bound on how much the pass reclaims.
const defaultOrphanBatch = 200

// isBucketName reports whether name is one of the 256 two-lowercase-hex-digit
// bucket directories CAS.path derives.
func isBucketName(name string) bool {
	if len(name) != 2 {
		return false
	}
	for i := 0; i < 2; i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sweepBucket walks one bucket in ReadDir chunks, returning what it removed,
// what went wrong and whether the whole sweep must stop. Emptied buckets are
// left in place: removing one races a concurrent publication that has just
// created it and is about to link into it.
//
// Order within a chunk is deliberate: the index is asked about every name the
// readdir returned -- names cost no syscall -- and only the hashes it reports
// as UNNAMED are then stat'ed for their age. A healthy store has none, so the
// pass costs one query per chunk and no lstat at all, instead of one lstat per
// object in the store on every invocation. The grace still gates every
// removal; it is only consulted for the objects that could actually go.
func (c *CAS) sweepBucket(ctx context.Context, dir string, known func(context.Context, []string) (map[string]struct{}, error),
	now time.Time, grace time.Duration, batch int) (int64, error, bool) {
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil, false
		}
		return 0, ioError("CAS sweep", err), false
	}
	defer d.Close()
	var removed int64
	var errs []error
	for {
		if err := ctx.Err(); err != nil {
			return removed, errors.Join(append(errs, model.Canceled(err))...), true
		}
		ents, err := d.ReadDir(batch)
		if err != nil && !errors.Is(err, io.EOF) {
			return removed, errors.Join(append(errs, ioError("CAS sweep", err))...), false
		}
		// A name path never derives is not this sweep's to judge, and a
		// directory inside a bucket is not an object.
		candidates := make([]fs.DirEntry, 0, len(ents))
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			if e.IsDir() || !model.ValidHexID(e.Name()) {
				continue
			}
			candidates = append(candidates, e)
			names = append(names, e.Name())
		}
		if len(names) > 0 {
			named, kerr := known(ctx, names)
			if kerr != nil {
				// No removal on an unreadable answer: an empty set read as
				// "nothing is named" would delete the whole store.
				return removed, errors.Join(append(errs, kerr)...), true
			}
			for _, e := range candidates {
				if _, ok := named[e.Name()]; ok {
					continue
				}
				info, statErr := e.Info()
				if statErr != nil {
					if !errors.Is(statErr, fs.ErrNotExist) {
						errs = append(errs, ioError("CAS sweep", statErr))
					}
					continue
				}
				// Younger than the grace: a publication whose naming commit
				// may still be in flight. Dropping this check is what makes
				// the sweep delete content a generation is about to
				// reference.
				if info.ModTime().Add(grace).After(now) {
					continue
				}
				if err := c.Remove(e.Name()); err != nil {
					errs = append(errs, err)
					continue
				}
				removed++
			}
		}
		if errors.Is(err, io.EOF) || len(ents) == 0 {
			break
		}
	}
	return removed, errors.Join(errs...), false
}

// Open streams the blob described by rec, verifying each 64-KiB block digest
// as it passes and the whole-file digest and size at end of file. A mismatch
// is CTX_SOURCE_INTEGRITY at the offending block, before any later byte is
// exposed. Memory is one block.
func (c *CAS) Open(ctx context.Context, rec model.BlobRecord) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	f, err := c.open(rec.Hash)
	if err != nil {
		return nil, err
	}
	return &verifyingReader{f: f, rec: rec, whole: sha256.New()}, nil
}

func (c *CAS) open(hash string) (*os.File, error) {
	p, err := c.path(hash)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, integrity("blob is missing from the content-addressed store").WithDetail("content_hash", hash)
		}
		return nil, ioError("CAS open", err)
	}
	return f, nil
}

// verifyingReader serves a blob one verified block at a time.
type verifyingReader struct {
	f      *os.File
	rec    model.BlobRecord
	whole  hash.Hash
	block  int
	buf    []byte
	pos    int
	buffer [source.BlockBytes]byte
	done   bool
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	for r.pos == len(r.buf) {
		if r.done {
			return 0, io.EOF
		}
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	return n, nil
}

// fill reads and verifies the next block, or finishes the whole-file check
// when every block has been served.
func (r *verifyingReader) fill() error {
	if r.block == len(r.rec.BlockDigests) {
		// Every recorded block is verified. The file must end exactly here and
		// the aggregate digest must match; extra bytes or a different whole
		// digest mean the blocks and the object disagree.
		var probe [1]byte
		if n, err := r.f.Read(probe[:]); n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return integrity("blob is longer than its recorded size").WithDetail("content_hash", r.rec.Hash)
		}
		if hex.EncodeToString(r.whole.Sum(nil)) != r.rec.Hash {
			return integrity("blob content does not match its content hash").WithDetail("content_hash", r.rec.Hash)
		}
		r.done = true
		r.buf, r.pos = nil, 0
		return nil
	}
	want := blockLen(r.rec.Size, r.block)
	n, err := io.ReadFull(r.f, r.buffer[:want])
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return integrity("blob is shorter than its recorded size").WithDetail("content_hash", r.rec.Hash)
		}
		return ioError("CAS read", err)
	}
	if err := verifyBlock(r.buffer[:n], r.rec, r.block); err != nil {
		return err
	}
	r.whole.Write(r.buffer[:n])
	r.buf, r.pos = r.buffer[:n], 0
	r.block++
	return nil
}

func (r *verifyingReader) Close() error { return r.f.Close() }

// blockLen is the size of block i of a blob of size bytes.
func blockLen(size int64, i int) int {
	remaining := size - int64(i)*source.BlockBytes
	if remaining > source.BlockBytes {
		return source.BlockBytes
	}
	return int(remaining)
}

func verifyBlock(data []byte, rec model.BlobRecord, i int) error {
	sum := sha256.Sum256(data)
	want, err := hex.DecodeString(rec.BlockDigests[i])
	if err != nil || !bytes.Equal(sum[:], want) {
		return integrity("blob block %d does not match its recorded digest", i).WithDetail("content_hash", rec.Hash)
	}
	return nil
}

// ReadRange returns the bytes [r.Start, r.End) of the blob, verifying exactly
// the blocks that cover the interval against rec and nothing else: it never
// rehashes the file and makes no claim about blocks it did not read. The
// interval is bounded by model.MaxRawChunkBytes, so one call reads at most
// that plus two partial blocks.
func (c *CAS) ReadRange(ctx context.Context, rec model.BlobRecord, r model.ByteRange) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, model.Canceled(err)
	}
	if err := rec.Validate(); err != nil {
		return nil, err
	}
	if err := r.Validate("byte_range"); err != nil {
		return nil, err
	}
	if r.End-r.Start > model.MaxRawChunkBytes {
		return nil, resourceLimit("byte range spans %d bytes, over the %d-byte read ceiling", r.End-r.Start, model.MaxRawChunkBytes)
	}
	idx := indexOf(rec)
	first, last, err := idx.BlockRange(r.Start, r.End)
	if err != nil {
		return nil, err
	}
	if first == last {
		// The empty interval at any valid offset, including EOF of an empty
		// file: nothing to verify and nothing to return but the offset itself.
		return []byte{}, nil
	}
	f, err := c.open(rec.Hash)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	start := int64(first) * source.BlockBytes
	end := min(int64(last)*source.BlockBytes, rec.Size)
	buf := make([]byte, end-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, integrity("blob is shorter than its recorded size").WithDetail("content_hash", rec.Hash)
		}
		return nil, ioError("CAS read", err)
	}
	for i := first; i < last; i++ {
		off := int64(i-first) * source.BlockBytes
		if err := verifyBlock(buf[off:off+int64(blockLen(rec.Size, i))], rec, i); err != nil {
			return nil, err
		}
	}
	return buf[r.Start-uint64(start) : r.End-uint64(start)], nil
}
