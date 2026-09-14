package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/toolchain"
)

type archiveKind string

const (
	archiveTarGz archiveKind = "tar.gz"
	archiveZip   archiveKind = "zip"
	archiveGz    archiveKind = "gz"  // a single gzip-compressed file
	archiveRaw   archiveKind = "raw" // a single uncompressed file
)

// extract writes src into dst under the confinement rules the runtime store
// applies: every member name must stay inside the payload root, symlink targets
// must stay inside it, and anything that is not a regular file, directory or
// symlink is refused. strip drops that many leading path components so the
// payload root is the tool itself rather than an upstream-versioned directory.
// dest is used for the single-file kinds as the payload-relative file name.
func extract(src, dst string, kind archiveKind, strip int, dest string) error {
	if err := os.MkdirAll(dst, dirMode); err != nil {
		return err
	}
	switch kind {
	case archiveTarGz:
		return extractTarGz(src, dst, strip)
	case archiveZip:
		return extractZip(src, dst, strip)
	case archiveGz:
		return extractSingleGz(src, dst, dest)
	case archiveRaw:
		return copyInto(src, filepath.Join(dst, filepath.FromSlash(dest)), execMode)
	}
	return fmt.Errorf("unknown archive kind %q", kind)
}

func stripComponents(name string, strip int) (string, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) <= strip {
		return "", false
	}
	return strings.Join(parts[strip:], "/"), true
}

func extractTarGz(src, dst string, strip int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok := stripComponents(hdr.Name, strip)
		if !ok {
			continue
		}
		clean, err := confinedRel(rel)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		target := filepath.Join(dst, filepath.FromSlash(clean))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, dirMode); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
				return err
			}
			mode := fs.FileMode(fileMode)
			if hdr.FileInfo().Mode().Perm()&0o111 != 0 {
				mode = execMode
			}
			if err := writeStream(target, tr, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := confinedLink(path.Dir(clean), hdr.Linkname); err != nil {
				return fmt.Errorf("%s: %w", src, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			// A hard link inside a redistributed payload would alias a file the
			// store cannot account for; materialize it as its own copy instead.
			linkRel, ok := stripComponents(hdr.Linkname, strip)
			if !ok {
				return fmt.Errorf("%s: hard link %q outside payload", src, hdr.Linkname)
			}
			cleanLink, err := confinedRel(linkRel)
			if err != nil {
				return fmt.Errorf("%s: %w", src, err)
			}
			if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
				return err
			}
			if err := copyInto(filepath.Join(dst, filepath.FromSlash(cleanLink)), target, execMode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported tar entry %q type %v", src, hdr.Name, hdr.Typeflag)
		}
	}
}

func extractZip(src, dst string, strip int) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rel, ok := stripComponents(zf.Name, strip)
		if !ok {
			continue
		}
		if strings.HasSuffix(zf.Name, "/") {
			clean, err := confinedRel(rel)
			if err != nil {
				return fmt.Errorf("%s: %w", src, err)
			}
			if err := os.MkdirAll(filepath.Join(dst, filepath.FromSlash(clean)), dirMode); err != nil {
				return err
			}
			continue
		}
		clean, err := confinedRel(rel)
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		target := filepath.Join(dst, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		mode := fs.FileMode(fileMode)
		if zf.Mode()&fs.ModeSymlink != 0 {
			linkBytes, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			link := string(linkBytes)
			if err := confinedLink(path.Dir(clean), link); err != nil {
				return fmt.Errorf("%s: %w", src, err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			continue
		}
		if zf.Mode().Perm()&0o111 != 0 {
			mode = execMode
		}
		err = writeStream(target, rc, mode)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractSingleGz(src, dst, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	target := filepath.Join(dst, filepath.FromSlash(dest))
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return err
	}
	return writeStream(target, gz, execMode)
}

func writeStream(target string, r io.Reader, mode fs.FileMode) error {
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(r, maxDownloadBytes)); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func copyInto(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), dirMode); err != nil {
		return err
	}
	return writeStream(dst, in, mode)
}

// checkTreeBounds holds one realized payload to the bounds the runtime
// extractor will apply to it. Measuring and only logging would leave a payload
// that outgrew a bound to be discovered at install time on a user's machine,
// as a CTX_RESOURCE_LIMIT nobody can act on; here it is a release-time failure
// with the two numbers in the message. The bounds come from internal/toolchain
// itself, so raising one there raises it here in the same change.
func checkTreeBounds(tool, plat, root string, compressedSize int64) error {
	files, bytes := treeStats(root)
	logf("%s/%s: extracts to %d files, %d bytes", tool, plat, files, bytes)
	if files > toolchain.MaxPayloadFiles {
		return fmt.Errorf("%s/%s: the payload holds %d files, over the runtime extractor's %d-entry bound",
			tool, plat, files, toolchain.MaxPayloadFiles)
	}
	if bound := toolchain.MaxExpandedBytes(compressedSize); bytes > bound {
		return fmt.Errorf("%s/%s: the payload expands to %d bytes, over the runtime extractor's %d-byte bound for a %d-byte payload",
			tool, plat, bytes, bound, compressedSize)
	}
	return nil
}

// treeStats reports what an extracted payload costs, so a release can be
// checked against the runtime extractor's file-count and expansion bounds
// before a lock that exceeds them is ever shipped.
func treeStats(root string) (files int, bytes int64) {
	_ = filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		files++
		bytes += fi.Size()
		return nil
	})
	return files, bytes
}
