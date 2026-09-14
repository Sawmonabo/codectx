// Package fslock is the product's one operating-system file-lock and
// directory-durability implementation. Two packages publish a directory tree by
// renaming it into place and serialize concurrent writers of that tree with an
// advisory whole-file lock: internal/snapshot's workspace lock and
// internal/toolchain's per-tool install lock. Keeping one implementation is
// what stops the two from drifting on a platform difference -- Windows has no
// whole-file flock and no directory fsync -- which is exactly the kind of
// divergence that is invisible until a crash on one platform loses a
// publication the other would have kept.
package fslock
