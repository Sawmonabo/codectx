//go:build unix

package paced

import (
	"io/fs"
	"syscall"
)

// shrinkable reports whether emptying this file frees only this file's space.
// A file with more than one link is one NAME of an object that other names
// still reach: truncating it empties the object itself, so the space it holds
// is not this name's to give back. Unlinking the name is, and it frees nothing
// while another link remains -- which is exactly right, because nothing has
// been given up yet.
//
// A content-addressed store publishes by linking its staging file to the
// object's final name, so between the link and the staging file's removal the
// two names are one object. Emptying the staging file there would empty the
// published object: a blob served as its hash, with none of its bytes.
func shrinkable(st fs.FileInfo) bool {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return !ok || sys.Nlink <= 1
}
