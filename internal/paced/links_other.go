//go:build !unix

package paced

import "io/fs"

// shrinkable reports whether emptying this file frees only this file's space.
// Where the link count is not readable, the file is emptied: the platforms
// this covers have no hard links for a removal to empty across.
func shrinkable(st fs.FileInfo) bool { return true }
