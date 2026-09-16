package diagnostics

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/scratch"
	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
)

// Resources is the Section 23 accounting block `status --resources` and
// `codectx_index_status` report.
//
// Three sources meet here and nowhere else: the Sampler reads the host, the
// store reader reports the database and write-ahead log on disk, and the
// resources configuration block states the reservations this process has
// promised itself. Every one of them keeps the Section 23 rule that an
// unavailable metric is absent and never zero -- a nil field says "not measured
// on this host", a zero says "measured, and it is zero".
//
// The space this process has freed is the one figure here that is neither
// measured on the host nor read from the store: it is counted where the
// freeing happens, in the window-at-a-time truncations the process's own
// removals and its file-system shim make, because a run that reuses its space
// instead of freeing it is the design and a reader outside the process cannot
// see the difference any other way.
//
// The pending-event count is the one figure here that a second process could
// not report at all until a watch published a heartbeat: the sampler measures
// the host, and a notification queue belongs to whichever process holds the
// watcher. It is read from the store for that reason and reported only while
// the writer's own deadline holds.
//
// A failure to read the store is not a failure to report resources: the host
// figures the sampler produced are still true, so the database and write-ahead
// log sizes are left absent and the call succeeds. The alternative would let an
// unreadable database suppress the memory reading an operator is diagnosing a
// memory problem with. The failure is not swallowed either -- an unreadable
// store is what the Section 22 store-integrity check reports, and this block's
// job is only to avoid reporting its size as zero.
func (s *Service) Resources(ctx context.Context) (model.ResourceReport, error) {
	report, err := s.opts.Sampler.Sample(ctx)
	if err != nil {
		return model.ResourceReport{}, err
	}
	if stats, statsErr := s.opts.Store.Stats(ctx); statsErr == nil {
		report.DatabaseBytes = nonNegativeBytes(stats.DatabaseBytes)
		report.WALBytes = nonNegativeBytes(stats.WALBytes)
	}
	report.FreedBytes = nonNegativeBytes((paced.Steps() + pacedvfs.Truncations()) * paced.Window)
	scratchBytes(&report)
	s.pendingWatchEvents(ctx, &report)
	s.reservations(&report)
	if err := report.Validate(); err != nil {
		return model.ResourceReport{}, err
	}
	return report, nil
}

// scratchBytes discloses the disk the store's scratch pools hold, which is
// the space this run kept instead of freeing. It is summed over every pool
// this process has opened, because a store's surfaces are pooled under more
// than one directory -- the data directory, the continuation store's, a
// provider's work directory -- and reporting one of them would understate it.
//
// Beside it goes the space this run has removed and not yet given back: a
// removal renames its file aside and returns, so between the removal and the
// release the space belongs to neither figure, and reporting only the pools
// would leave it invisible. Summing the two would be worse -- the pool would
// appear to grow every time the run removed something.
//
// A pool that cannot be read leaves the figure absent rather than short: a
// partial sum here would read as "the run is holding less than it is", which
// is the one thing this figure exists to rule out. The purposes the freeing
// WAS for are reported beside it, and an empty map is left absent because a
// process that has freed nothing for any named purpose has nothing to say
// rather than a measured zero per purpose.
func scratchBytes(report *model.ResourceReport) {
	var total int64
	for _, a := range scratch.All() {
		n, err := a.Bytes()
		if err != nil {
			return
		}
		total += n
	}
	report.ScratchBytes = nonNegativeBytes(total)
	if pending, err := paced.PendingFreeBytes(); err == nil {
		report.PendingFreeBytes = nonNegativeBytes(pending)
	}
	if byPurpose := paced.FreedByPurpose(); len(byPurpose) > 0 {
		report.FreedByPurpose = make(map[string]uint64, len(byPurpose))
		for p, n := range byPurpose {
			if n > 0 {
				report.FreedByPurpose[string(p)] = uint64(n)
			}
		}
	}
}

// pendingWatchEvents fills the pending-event count from the live watch
// heartbeat, and leaves it absent otherwise.
//
// Absent covers three different situations on purpose -- no watch is running,
// the watch that was running stopped refreshing its row, or the heartbeat could
// not be read -- because all three mean the same thing to this block: nobody is
// measuring the queue right now. Reporting the last figure a dead watcher wrote
// would read as "the watch is caught up", which is precisely the claim a stale
// row must never make; which of the three it is, and whether it needs acting
// on, is what the doctor's `watch_heartbeat` check reports.
//
// A watch running without a notification source leaves the figure absent too:
// periodic reconciliation has no queue to count, so it writes none.
func (s *Service) pendingWatchEvents(ctx context.Context, report *model.ResourceReport) {
	hb, live, _ := s.liveWatch(ctx)
	if live && hb.PendingEvents != nil && *hb.PendingEvents >= 0 {
		pending := *hb.PendingEvents
		report.PendingEvents = &pending
	}
}

// reservations fills the three figures that are not measured but promised: what
// this process has set aside for concurrent queries, for the parsed-graph cache
// and for the indexing queue. They are configuration, so they are always
// available and a zero here is a real zero.
//
// This is the first runtime reader of resources.cache_bytes: until now that key
// was read only by configuration validation. Validation already proves the three
// reservations fit resources.base_memory_budget_bytes
// (internal/config/validate.go:204-211), so the report states the three rather
// than re-deriving the check -- a second implementation of that arithmetic would
// drift from the one that refuses a bad configuration.
func (s *Service) reservations(report *model.ResourceReport) {
	res := s.opts.Config.Resources
	// Validation refuses a configuration whose product overflows
	// (mulNoOverflow, internal/config/validate.go:199), so the multiplication
	// here cannot wrap on a loaded configuration; a negative from any other
	// path is absent rather than published as a huge unsigned.
	report.QueryReservationBytes = nonNegativeBytes(int64(res.MaxConcurrentQueries) * res.QueryMemoryBytes)
	report.CacheReservationBytes = nonNegativeBytes(res.CacheBytes)
	report.QueueReservationBytes = nonNegativeBytes(s.opts.Config.Index.QueueBytes)
}

// nonNegativeBytes converts a signed byte count to the report's unsigned
// pointer form. A negative count is not a measurement, so it is absent: model
// validation refuses a byte count it cannot round-trip through int64, and
// publishing a negative as a huge unsigned would be the worse of the two wrong
// answers.
func nonNegativeBytes(n int64) *uint64 {
	if n < 0 {
		return nil
	}
	u := uint64(n)
	return &u
}
