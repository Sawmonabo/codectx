package toolchain

import (
	"archive/tar"
	"archive/zip"
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
)

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zipMagic  = []byte("PK\x03\x04")
)

// extract unpacks the verified archive in src into dir, which must already
// exist and be empty. The format is decided by the file's magic bytes rather
// than by the URL, so a renamed asset cannot steer the reader.
func extract(ctx context.Context, name string, src *os.File, dir string, compressedSize int64) error {
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
		maxBytes: min(int64(maxUncompressedBytes), compressedSize*maxExpansionRatio+expansionFloor),
	}

	switch {
	case string(magic[:2]) == string(gzipMagic):
		err = untar(ctx, src, w)
	case string(magic[:]) == string(zipMagic):
		err = unzip(ctx, src, compressedSizeOf(src), w)
	default:
		err = corrupt(name, "the payload is neither a gzip nor a zip archive")
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
	if err := confinedRelPath(clean); err != nil {
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

func untar(ctx context.Context, src io.Reader, w *payloadWriter) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return corrupt(w.name, "the payload is not a readable gzip stream")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
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
	return toolError(CodeToolCorrupt, format, args...).
		WithDetail("tool", name).
		WithRemediation("run `codectx tools verify`; if it persists the pinned payload is unusable on this platform and is a codectx defect")
}
