package model

// WorkspaceReset is the payload of `codectx reset`: the data directory that was
// emptied, every entry removed from it with the bytes its removal gave back to
// the filesystem, and, when the reset was asked to index again, the result of
// that index.
//
// The bytes are what the filesystem was actually given back, counted as each
// truncation and unlink released them, not what the entries appeared to hold:
// a file another name still reaches gives nothing back when this name goes,
// and a report that counted it would claim space the disk still holds.
type WorkspaceReset struct {
	// DataDir is the resolved data directory: the path the removal was made
	// in, after every symbolic link above it was followed.
	DataDir string `json:"data_dir"`
	// Removed is every entry the data directory held, in name order. The
	// workspace and serving lock files are not among them: they are the locks
	// this reset held while it removed the rest, and they stay as the empty
	// files the next holders take.
	Removed    []RemovedEntry `json:"removed"`
	FreedBytes uint64         `json:"freed_bytes"`
	// Index is the full index run after the removal, present only when the
	// reset was asked to index again.
	Index *IndexResult `json:"index,omitempty"`
}

// RemovedEntry is one entry of the data directory a reset removed: a file, a
// directory with everything under it, or a symbolic link removed as a link.
type RemovedEntry struct {
	Path  string `json:"path"`
	Bytes uint64 `json:"bytes"`
}
