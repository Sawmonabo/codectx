// Package workspace discovers the repository root, confines every file access
// to it and streams its files deterministically.
//
// It owns path safety for the whole application: a caller never turns a
// root-relative path into an absolute one and opens it itself, because string
// validation cannot see the symlink that appears between the check and the
// open. Every read goes through Root, which is backed by os.Root and therefore
// confined by the operating system rather than by inspection.
//
// It does not execute Git. Discovery reports whether a Git directory is
// present; enumerating tracked paths and evaluating ignore rules belongs to the
// Git owner, which supplies them to Walk through Policy.
package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// gitDirName is the entry that marks a repository root. It is a directory in an
// ordinary clone and a file in a worktree or submodule.
const gitDirName = ".git"

// maxDiscoveryDepth bounds the walk toward the filesystem root so a pathological
// mount cannot turn discovery into an unbounded loop.
const maxDiscoveryDepth = 256

// Root is an opened workspace: an absolute path plus the OS-confined handle
// every read goes through. It is safe for concurrent use.
type Root struct {
	// Path is the absolute, cleaned repository root.
	Path string
	// HasGit reports whether the root carries a .git entry. It is a statement
	// about the filesystem only: no Git command has been run.
	HasGit bool
	// GitPath is the absolute path of that .git entry when HasGit is true.
	GitPath string

	root *os.Root
}

// File is one entry emitted by Walk. It carries no content and no buffer, so a
// caller can stream a repository without retaining a file list.
type File struct {
	// Path is the root-relative path with "/" separators, in the exact case the
	// filesystem reported. It is never case-folded: two paths differing only in
	// case are distinct paths on a case-sensitive filesystem, and aliasing them
	// would merge two files into one identity.
	Path    string
	Size    int64
	Mode    fs.FileMode
	ModTime int64
}

// Discover finds the workspace containing start: the nearest ancestor holding a
// .git entry, or the start directory itself when there is none. A directory
// without Git is a legitimate workspace; it simply has no tracked-path source.
func Discover(start string) (Root, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return Root{}, pathError("workspace root %q cannot be resolved: %v", start, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Root{}, notFound("workspace %q cannot be opened: %v", abs, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	rootPath, gitPath := abs, ""
	dir := abs
	for range maxDiscoveryDepth {
		candidate := filepath.Join(dir, gitDirName)
		if _, err := os.Lstat(candidate); err == nil {
			rootPath, gitPath = dir, candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	opened, err := os.OpenRoot(rootPath)
	if err != nil {
		return Root{}, notFound("workspace %q cannot be opened: %v", rootPath, err)
	}
	return Root{Path: rootPath, HasGit: gitPath != "", GitPath: gitPath, root: opened}, nil
}

// Close releases the confined directory handle.
func (r Root) Close() error {
	if r.root == nil {
		return nil
	}
	return r.root.Close()
}

// Open opens one regular file inside the workspace for reading.
//
// The path is validated, then opened through the confined handle, and the
// result is checked both before and after the open: the entry must be a regular
// file when its link status is inspected, and the opened descriptor must still
// refer to that same file. A symlink swapped in between the two steps therefore
// fails instead of silently redirecting the read.
func (r Root) Open(rel string) (*os.File, error) {
	if r.root == nil {
		return nil, notFound("the workspace is not open")
	}
	clean, err := r.checkPath(rel)
	if err != nil {
		return nil, err
	}
	before, err := r.root.Lstat(clean)
	if err != nil {
		return nil, pathError("%q cannot be opened: %v", rel, err)
	}
	if !before.Mode().IsRegular() {
		return nil, pathError("%q is %s, not a regular file", rel, describeMode(before.Mode()))
	}
	f, err := r.root.Open(clean)
	if err != nil {
		return nil, pathError("%q cannot be opened: %v", rel, err)
	}
	after, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, pathError("%q cannot be inspected after opening: %v", rel, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, pathError("%q changed between the check and the open", rel)
	}
	return f, nil
}

// Lstat reports metadata for one path inside the workspace without following a
// final-component symlink, so the caller learns what the entry is rather than
// what it points at. The name says so: a caller reaching for Stat would
// reasonably expect the opposite, and a symlink silently resolved is how a
// capture attributes foreign bytes to the repository.
func (r Root) Lstat(rel string) (fs.FileInfo, error) {
	if r.root == nil {
		return nil, notFound("the workspace is not open")
	}
	clean, err := r.checkPath(rel)
	if err != nil {
		return nil, err
	}
	info, err := r.root.Lstat(clean)
	if err != nil {
		return nil, pathError("%q cannot be inspected: %v", rel, err)
	}
	return info, nil
}

// checkPath validates one root-relative path and returns its native spelling.
//
// Only separators are normalized. Case is preserved exactly: folding it would
// alias two distinct valid paths on a case-sensitive filesystem, and Unicode
// normalization would do the same for two distinct filenames that are visually
// identical.
func (r Root) checkPath(rel string) (string, error) {
	if rel == "" {
		return "", pathError("an empty path does not name a file in the workspace")
	}
	if strings.ContainsRune(rel, 0) {
		return "", pathError("a path containing a NUL byte is not a valid path")
	}
	if len(rel) > model.MaxPathBytes {
		return "", pathError("path is %d bytes, limit %d", len(rel), model.MaxPathBytes)
	}
	slashed := filepath.ToSlash(rel)
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", pathError("%q is absolute; workspace paths are root-relative", rel)
	}
	for _, part := range strings.Split(slashed, "/") {
		switch part {
		case "":
			return "", pathError("%q has an empty path component", rel)
		case ".":
			return "", pathError("%q has a %q component; workspace paths are already normalized", rel, ".")
		case "..":
			return "", pathError("%q traverses out of the workspace", rel)
		}
	}
	return filepath.FromSlash(slashed), nil
}

func describeMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeDevice != 0:
		return "a device"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	default:
		return "an unsupported file type"
	}
}

func pathError(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodePathEscape, Message: fmt.Sprintf(format, args...)}
}

func notFound(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeWorkspaceNotFound, Message: fmt.Sprintf(format, args...)}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}
