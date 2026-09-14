package retention

import "context"

// grace is the Section 10.4 blob protocol, owned by L3a: an unreferenced blob
// is quarantined, then trashed under a recheck of leases and reachability, and
// deleted -- row, blocks, line checkpoints and the CAS object on disk -- only
// after the grace window and a second reachability check. A blob that becomes
// referenced again before the final deletion is restored, not deleted.
//
// Binding invariant from storage/sqlite/source.go:57-64: blob_blocks and
// line_checkpoints must not be dropped before the blobs row, because PutBlob's
// restore path flips state and keeps them.
//
// The store methods this needs do not exist at this commit: L3a owns
// storage/sqlite/gc.go and source.go and declares its narrow store interface
// here beside its implementation, so L0 does not freeze a signature for
// behaviour that has not been designed yet.
func (c *Collector) grace(_ context.Context, report Report) (Report, error) {
	return report, notImplemented("blob grace protocol", "L3a")
}
