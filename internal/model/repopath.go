package model

import (
	"path"
	"strings"
)

// IsRepoPath reports whether p is a repository path in the spelling a capture
// stores a file under: slash-separated, relative to the repository root,
// lexically clean (path.Clean leaves it unchanged, so no empty, "." or ".."
// segment and no trailing slash), not the root itself, not climbing above it,
// and within MaxPathBytes. It is the one rule every surface that takes a path
// from outside holds that path to; each surface first reads its input its own
// way (a search filter trims a trailing slash, a free task word drops a leading
// one) and asks this of the result.
func IsRepoPath(p string) bool {
	return p != "" && p != "." && p != ".." && len(p) <= MaxPathBytes &&
		!strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "../") && path.Clean(p) == p
}

// CleanRepoPath lexically cleans a slash-separated path (path.Clean) and
// reports whether the result is a repository path (IsRepoPath). A path that
// cleans to the root, climbs above it or is absolute is not one.
func CleanRepoPath(p string) (string, bool) {
	clean := path.Clean(p)
	return clean, IsRepoPath(clean)
}
