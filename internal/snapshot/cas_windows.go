//go:build windows

package snapshot

// openFileLimit reports no discoverable descriptor limit: Windows handles are
// bounded by memory rather than by a per-process soft limit, so the sync
// window is sized from the CPU count alone.
func openFileLimit() int { return 0 }
