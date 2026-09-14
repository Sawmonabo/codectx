package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// epoch is the single modification time every normalized payload entry carries.
// A payload must be a function of its contents alone: a wall-clock mtime would
// make two identical trees produce different digests and defeat the lock.
var epoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	fileMode = 0o644
	execMode = 0o755
	dirMode  = 0o755

	// maxPayloadBytes is GitHub's per-release-asset ceiling. A payload above it
	// cannot be published, so the generator fails with the number rather than
	// discovering it during a multi-minute upload.
	maxPayloadBytes = 2 << 30
)

// confinedRel normalizes an upstream archive member name and then holds it to
// the runtime's own rule, toolchain.ConfinedRelPath. Normalizing first is the
// one deliberate difference: this program reads archives as their publishers
// wrote them, where a leading "./" or a trailing "/" is ordinary, while the
// runtime reads only payloads this program has already vetted and rejects both
// outright. Everything the runtime refuses -- an absolute path, a rooted
// Windows path, a backslash, a NUL, a path climbing out with ".." -- is refused
// here too, from the same code, so a payload cannot be packed or measured under
// a name the product would later throw out.
func confinedRel(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty path")
	}
	clean := path.Clean(strings.TrimSuffix(strings.ReplaceAll(name, `\`, "/"), "/"))
	if err := toolchain.ConfinedRelPath(clean); err != nil {
		return "", fmt.Errorf("path %q %s", name, err)
	}
	return clean, nil
}

// confinedLink validates a symlink target read from dir-relative position
// linkDir. A target that resolves outside the payload root would let an
// extracted payload reach arbitrary files on the host.
func confinedLink(linkDir, target string) error {
	slashed := strings.ReplaceAll(target, `\`, "/")
	if path.IsAbs(slashed) {
		return fmt.Errorf("absolute symlink target %q", target)
	}
	resolved := path.Join(linkDir, slashed)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("symlink target escapes payload root: %q -> %q", linkDir, target)
	}
	return nil
}

// packDeterministic writes root as a tar.gz whose bytes are a pure function of
// the tree's contents: entries sorted by path, one fixed mtime, uid/gid 0, no
// owner names, modes collapsed to 0644/0755, and a gzip header with no name and
// no timestamp. Two identical trees produce byte-identical archives, which is
// what makes a published payload digest reproducible from source.
func packDeterministic(root, outPath string) (digest string, size int64, err error) {
	entries, err := collectEntries(root)
	if err != nil {
		return "", 0, err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	hasher := sha256.New()
	counter := &countingWriter{}
	gz, err := gzip.NewWriterLevel(io.MultiWriter(out, hasher, counter), gzip.DefaultCompression)
	if err != nil {
		return "", 0, err
	}
	gz.Header = gzip.Header{OS: 255} // no name, no comment, zero mtime, unknown OS
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		if err := writeEntry(tw, root, e); err != nil {
			return "", 0, fmt.Errorf("%s: %w", e.rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if counter.n > maxPayloadBytes {
		return "", 0, fmt.Errorf("payload %s is %d bytes, above the %d-byte release-asset ceiling", outPath, counter.n, int64(maxPayloadBytes))
	}
	return hex.EncodeToString(hasher.Sum(nil)), counter.n, nil
}

type entry struct {
	rel  string
	info fs.FileInfo
}

func collectEntries(root string) ([]entry, error) {
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, err := confinedRel(rel); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular(), info.IsDir(), info.Mode()&fs.ModeSymlink != 0:
		default:
			return fmt.Errorf("%s: unsupported file type %s", rel, info.Mode().Type())
		}
		entries = append(entries, entry{rel: rel, info: info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	return entries, nil
}

func writeEntry(tw *tar.Writer, root string, e entry) error {
	full := filepath.Join(root, filepath.FromSlash(e.rel))
	hdr := &tar.Header{
		Name:    e.rel,
		ModTime: epoch,
		Uid:     0,
		Gid:     0,
		Format:  tar.FormatPAX,
	}
	switch {
	case e.info.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = e.rel + "/"
		hdr.Mode = dirMode
		return tw.WriteHeader(hdr)
	case e.info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return err
		}
		if err := confinedLink(path.Dir(e.rel), target); err != nil {
			return err
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = filepath.ToSlash(target)
		hdr.Mode = execMode
		return tw.WriteHeader(hdr)
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.info.Size()
		hdr.Mode = fileMode
		if e.info.Mode().Perm()&0o111 != 0 {
			hdr.Mode = execMode
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		defer f.Close()
		n, err := io.Copy(tw, f)
		if err != nil {
			return err
		}
		if n != e.info.Size() {
			return fmt.Errorf("short read: %d of %d bytes", n, e.info.Size())
		}
		return nil
	}
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// hashFile streams a file through SHA-256 without holding it in memory.
func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
