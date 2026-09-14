package retention

import "context"

// sweep gives five landed, caller-less helpers their first caller, in
// dependency order -- ExpireSessions, then PruneSessions (the first reader of
// the dead `storage.closed_session_retention` key), then Spools.Sweep,
// snapshot.Sweep and Resolver.GC -- and reclaims the two trees nothing
// reclaims: <data_dir>/lsp/<name>/<digest> for a digest no lock entry names,
// the abandoned <workdir>/config-* staging directories, and orphan dependence
// run directories. Owned by L3b.
//
// It never deletes retained source: that is the grace protocol's, not this
// pass's.
func (c *Collector) sweep(_ context.Context) (Report, error) {
	return Report{}, notImplemented("sweeps", "L3b")
}
