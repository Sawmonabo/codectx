//go:build !linux && !darwin

package paced

// unlinkRefused has no permission check to ask on this platform, so the
// unlink itself is the only answer and the bytes are charged before it.
func unlinkRefused(string) error { return nil }
