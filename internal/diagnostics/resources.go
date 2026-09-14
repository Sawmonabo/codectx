package diagnostics

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
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
// A failure to read the store is not a failure to report resources: the host
// figures the sampler produced are still true, so the database and write-ahead
// log sizes are left absent and the call succeeds. The alternative would let an
// unreadable database suppress the memory reading an operator is diagnosing a
// memory problem with.
func (s *Service) Resources(ctx context.Context) (model.ResourceReport, error) {
	report, err := s.opts.Sampler.Sample(ctx)
	if err != nil {
		return model.ResourceReport{}, err
	}
	if stats, statsErr := s.opts.Store.Stats(ctx); statsErr == nil {
		report.DatabaseBytes = nonNegativeBytes(stats.DatabaseBytes)
		report.WALBytes = nonNegativeBytes(stats.WALBytes)
	}
	s.reservations(&report)
	if err := report.Validate(); err != nil {
		return model.ResourceReport{}, err
	}
	return report, nil
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
