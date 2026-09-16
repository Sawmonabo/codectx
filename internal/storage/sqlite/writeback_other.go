//go:build !linux

package sqlite

// writebackPacer exists only where the kernel offers range writeback; on
// other systems the commit's fsync is the only writeback the store requests.
type writebackPacer struct{}

func (s *Store) startPacer() {}

func (s *Store) stopPacer() {}
