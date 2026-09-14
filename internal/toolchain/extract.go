package toolchain

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Confined extraction (Section 21). Everything a payload carries is validated
// before it is written and is written through an os.Root over the staging
// payload directory, so a hostile archive is refused by the rules below and
// again by the operating system if a rule were ever wrong.
//
// The lock declares a payload's compressed size and digest but no file count or
// uncompressed size, so the bounds below are package constants: a payload that
// needs more than this is a lock mistake, not something to discover at run time.
const (
	// maxPayloadFiles bounds entries in one archive. The largest payloads the
	// lock carries are the JDK and the npm-installed Node servers; the local
	// Joern 4.0.627 distribution is 1,349 files, and this leaves a wide margin
	// over anything the lock is expected to name.
	maxPayloadFiles = 50_000
	// maxUncompressedBytes is the absolute ceiling on what one payload may
	// expand to.
	maxUncompressedBytes = 4 << 30
	// maxExpansionRatio and expansionFloor bound expansion relative to the
	// bytes actually fetched, which is what stops a decompression bomb: the
	// pinned payloads sit between 1.5x and 4x, a bomb is a thousandfold.
	maxExpansionRatio = 50
	expansionFloor    = 16 << 20
	// tarBlockSize, tarMagicOffset and tarMagic identify a tar stream from its
	// first block, which is how a gzip-compressed tar is told apart from a lone
	// gzip-compressed file.
	tarBlockSize   = 512
	tarMagicOffset = 257
	tarMagic       = "ustar"
)

// MaxPayloadFiles and MaxUncompressedBytes are the absolute extraction bounds
// this package enforces. They are exported so the release-time generator in
// internal/tools/toollock refuses to pin a payload the runtime would refuse to
// install, rather than leaving that to be discovered on a user's machine.
const (
	MaxPayloadFiles      = maxPayloadFiles
	MaxUncompressedBytes = int64(maxUncompressedBytes)
)

// MaxExpandedBytes is the byte budget one payload of the given compressed size
// is allowed to expand to. It is exported alongside the two constants because
// for every payload under about 85 MiB this ratio bound, not the absolute one,
// is what an extraction actually hits -- and a generator carrying its own copy
// of the formula is the drift the export exists to prevent.
func MaxExpandedBytes(compressedSize int64) int64 {
	return min(MaxUncompressedBytes, compressedSize*maxExpansionRatio+expansionFloor)
}

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zipMagic  = []byte("PK\x03\x04")
)

// extract unpacks the verified payload in src into dir, which must already
// exist and be empty. The format is decided by the file's magic bytes rather
// than by the URL, so a renamed asset cannot steer the reader.
//
// Three shapes exist, because three of the pinned upstreams publish a bare
// executable rather than an archive (Section 11.7 "Payloads"): a gzip-compressed
// tar, a zip, and a single file -- either a lone gzip member or an uncompressed
// binary. A single file has no member name of its own, so it is written at
// entry, the payload-relative path the lock already pins for it, and the entry
// digest check that follows verifies it exactly as it verifies an archive
// member.
func extract(ctx context.Context, name string, src *os.File, dir, entry string, compressedSize int64) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return ioError("tool payload rewind", err)
	}
	var magic [4]byte
	if _, err := io.ReadFull(src, magic[:]); err != nil {
		return corrupt(name, "the payload is too small to be an archive")
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return ioError("tool payload rewind", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return ioError("tool payload staging root", err)
	}
	defer root.Close()
	w := &payloadWriter{
		root:     root,
		name:     name,
		maxBytes: MaxExpandedBytes(compressedSize),
	}

	switch {
	case string(magic[:2]) == string(gzipMagic):
		err = untarOrSingleGz(ctx, src, w, entry)
	case string(magic[:]) == string(zipMagic):
		err = unzip(ctx, src, compressedSizeOf(src), w)
	default:
		// One uncompressed file. src is back at offset 0 and the fetcher
		// already proved the file is exactly compressedSize bytes, so the
		// declared size here is the true size and the sized writer applies.
		err = w.file(entry, true, compressedSize, src)
	}
	if err != nil {
		return err
	}
	if w.files == 0 {
		return corrupt(name, "the payload archive is empty")
	}
	// A payload may not publish itself. The name check on each entry rejects a
	// literal ".complete", and this rejects the same file arriving through an
	// in-payload directory symlink, which os.Root follows because it does not
	// leave the payload.
	if _, err := root.Lstat(completeName); err == nil {
		return corrupt(name, "the payload carries the publication marker")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ioError("tool payload marker check", err)
	}
	return nil
}

func compressedSizeOf(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

// payloadWriter applies the entry rules and owns the byte and file budgets.
type payloadWriter struct {
	root     *os.Root
	name     string
	files    int
	bytes    int64
	maxBytes int64
}

// check validates one archive entry name. It is the same confinement rule the
// lock's entry paths pass, plus the reserved publication marker.
func (w *payloadWriter) check(name string) (string, error) {
	if len(name) > model.MaxPathBytes {
		return "", corrupt(w.name, "the payload carries a path longer than %d bytes", model.MaxPathBytes)
	}
	clean := strings.TrimSuffix(name, "/")
	if err := ConfinedRelPath(clean); err != nil {
		return "", corrupt(w.name, "the payload carries a path that %s", err)
	}
	if clean == completeName {
		return "", corrupt(w.name, "the payload carries the reserved %q name", completeName)
	}
	if w.files++; w.files > maxPayloadFiles {
		return "", resourceLimit("tool %q payload holds more than %d entries", w.name, maxPayloadFiles)
	}
	return clean, nil
}

func (w *payloadWriter) mkdir(name string) error {
	clean, err := w.check(name)
	if err != nil {
		return err
	}
	if err := w.root.MkdirAll(clean, storeDirPerm); err != nil {
		return ioError("tool payload directory", err)
	}
	return nil
}

// file writes one regular entry. The executable bit is the only permission a
// payload may express: everything else is the store's own 0600/0700, because a
// payload does not get to decide who may read the store.
func (w *payloadWriter) file(name string, executable bool, size int64, r io.Reader) error {
	clean, err := w.check(name)
	if err != nil {
		return err
	}
	if w.bytes += size; w.bytes > w.maxBytes {
		return resourceLimit("tool %q payload expands past its %d-byte bound", w.name, w.maxBytes)
	}
	if dir := path.Dir(clean); dir != "." {
		if err := w.root.MkdirAll(dir, storeDirPerm); err != nil {
			return ioError("tool payload directory", err)
		}
	}
	perm := fs.FileMode(storeFilePerm)
	if executable {
		perm = storeExecPerm
	}
	// O_EXCL makes a duplicate entry a failure rather than a silent overwrite,
	// which is how an archive smuggles a second version of a verified file past
	// a reader that only inspected the first.
	f, err := w.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return ioError("tool payload file", err)
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if err != nil {
		return ioError("tool payload write", err)
	}
	if n != size {
		return corrupt(w.name, "a payload entry is %d bytes where its header declares %d", n, size)
	}
	// Durability before publication: the rename that publishes this tree must
	// not expose file names whose contents are still only in the page cache.
	if err := f.Sync(); err != nil {
		return ioError("tool payload sync", err)
	}
	return nil
}

// stream writes one regular entry whose decompressed length is not known in
// advance. A lone gzip member carries no trustworthy decompressed length --
// the footer's ISIZE is attacker-supplied and only 32 bits wide -- so the byte
// budget cannot be charged up front the way file does it. It is charged while
// the bytes move instead, against the same payload-wide w.bytes and w.maxBytes,
// so a bomb in this shape fails at the bound rather than at the end of the copy.
func (w *payloadWriter) stream(name string, executable bool, r io.Reader) error {
	clean, err := w.check(name)
	if err != nil {
		return err
	}
	if dir := path.Dir(clean); dir != "." {
		if err := w.root.MkdirAll(dir, storeDirPerm); err != nil {
			return ioError("tool payload directory", err)
		}
	}
	perm := fs.FileMode(storeFilePerm)
	if executable {
		perm = storeExecPerm
	}
	f, err := w.root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return ioError("tool payload file", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, &budgetedReader{r: r, w: w}); err != nil {
		var typed *model.Error
		if errors.As(err, &typed) {
			return err
		}
		return ioError("tool payload write", err)
	}
	if err := f.Sync(); err != nil {
		return ioError("tool payload sync", err)
	}
	return nil
}

// budgetedReader charges every byte it hands on to the payload's byte budget
// and fails the moment the running total passes the bound.
type budgetedReader struct {
	r io.Reader
	w *payloadWriter
}

func (b *budgetedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		if b.w.bytes += int64(n); b.w.bytes > b.w.maxBytes {
			return 0, resourceLimit("tool %q payload expands past its %d-byte bound", b.w.name, b.w.maxBytes)
		}
	}
	return n, err
}

// symlink writes one link after proving its target stays inside the payload.
// os.Root refuses to traverse an escaping link, but the store is also read by
// ordinary path operations once published -- the analyzer process itself, for
// one -- so the link must be safe as data, not only as something os.Root walks.
func (w *payloadWriter) symlink(name, target string) error {
	clean, err := w.check(name)
	if err != nil {
		return err
	}
	if target == "" || strings.ContainsRune(target, 0) || strings.ContainsRune(target, '\\') {
		return corrupt(w.name, "the payload carries a symlink with an unusable target")
	}
	if strings.HasPrefix(target, "/") || (len(target) > 1 && target[1] == ':') {
		return corrupt(w.name, "the payload carries a symlink to an absolute path")
	}
	resolved := path.Clean(path.Join(path.Dir(clean), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return corrupt(w.name, "the payload carries a symlink whose target leaves the payload")
	}
	if dir := path.Dir(clean); dir != "." {
		if err := w.root.MkdirAll(dir, storeDirPerm); err != nil {
			return ioError("tool payload directory", err)
		}
	}
	if err := w.root.Symlink(target, clean); err != nil {
		return ioError("tool payload symlink", err)
	}
	return nil
}

// untarOrSingleGz decompresses one gzip member and decides what it holds. A
// gzip member is a tar only when "ustar" sits at offset 257 of the first
// 512-byte block; anything else is one compressed file, which is how
// rust-analyzer publishes its unix builds. The decision is made on the
// decompressed bytes rather than on the URL's suffix, for the same reason the
// format switch reads magic rather than a name.
func untarOrSingleGz(ctx context.Context, src io.Reader, w *payloadWriter, entry string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return corrupt(w.name, "the payload is not a readable gzip stream")
	}
	defer gz.Close()
	head := make([]byte, tarBlockSize)
	n, err := io.ReadFull(gz, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return corrupt(w.name, "the payload gzip stream is malformed")
	}
	head = head[:n]
	stream := io.MultiReader(bytes.NewReader(head), gz)
	if n < tarBlockSize || string(head[tarMagicOffset:tarMagicOffset+len(tarMagic)]) != tarMagic {
		return w.stream(entry, true, stream)
	}
	return untar(ctx, stream, w)
}

// untar walks an already-decompressed tar stream.
func untar(ctx context.Context, src io.Reader, w *payloadWriter) error {
	tr := tar.NewReader(src)
	for {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return corrupt(w.name, "the payload tar stream is malformed")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = w.mkdir(hdr.Name)
		case tar.TypeReg:
			err = w.file(hdr.Name, hdr.Mode&0o111 != 0, hdr.Size, tr)
		case tar.TypeSymlink:
			err = w.symlink(hdr.Name, hdr.Linkname)
		case tar.TypeLink:
			// A hard link shares an inode with a file the extractor may have
			// already verified, or with one outside the payload on a filesystem
			// that allows it. Neither is something a pinned payload needs.
			err = corrupt(w.name, "the payload carries a hard link")
		default:
			err = corrupt(w.name, "the payload carries an entry that is not a file, directory or symlink")
		}
		if err != nil {
			return err
		}
	}
}

func unzip(ctx context.Context, src io.ReaderAt, size int64, w *payloadWriter) error {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return corrupt(w.name, "the payload is not a readable zip archive")
	}
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		mode := entry.Mode()
		switch {
		case entry.FileInfo().IsDir():
			err = w.mkdir(entry.Name)
		case mode&fs.ModeSymlink != 0:
			err = w.zipSymlink(entry)
		case mode.IsRegular():
			err = w.zipFile(entry, mode)
		default:
			err = corrupt(w.name, "the payload carries an entry that is not a file, directory or symlink")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *payloadWriter) zipFile(entry *zip.File, mode fs.FileMode) error {
	// The declared uncompressed size is checked against the budget before the
	// stream is opened, so a lying header cannot get a bomb past the bound: the
	// copy below is still limited to the declared size and a longer body fails.
	if entry.UncompressedSize64 > uint64(maxUncompressedBytes) {
		return resourceLimit("tool %q payload declares an entry over the %d-byte bound", w.name, int64(maxUncompressedBytes))
	}
	rc, err := entry.Open()
	if err != nil {
		return corrupt(w.name, "a payload entry cannot be decompressed")
	}
	defer rc.Close()
	return w.file(entry.Name, mode&0o111 != 0, int64(entry.UncompressedSize64), rc)
}

func (w *payloadWriter) zipSymlink(entry *zip.File) error {
	if entry.UncompressedSize64 > model.MaxPathBytes {
		return corrupt(w.name, "the payload carries a symlink target longer than %d bytes", model.MaxPathBytes)
	}
	rc, err := entry.Open()
	if err != nil {
		return corrupt(w.name, "a payload entry cannot be decompressed")
	}
	defer rc.Close()
	target, err := io.ReadAll(io.LimitReader(rc, model.MaxPathBytes))
	if err != nil {
		return corrupt(w.name, "a payload symlink target cannot be read")
	}
	return w.symlink(entry.Name, string(target))
}

// corrupt types a payload whose bytes matched the lock but whose contents are
// malformed or hostile. A fresh install is the repair; the digest is not in
// question, so this is never confused with CTX_TOOL_DIGEST_MISMATCH.
func corrupt(name, format string, args ...any) *model.Error {
	return toolError(model.CodeToolCorrupt, format, args...).
		WithDetail("tool", name).
		WithRemediation("run `codectx tools verify`; if it persists the pinned payload is unusable on this platform and is a codectx defect")
}
