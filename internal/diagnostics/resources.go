package diagnostics

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/scratch"
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
// freeing happens, byte by byte as each truncation and each unlink releases
// them, because a run that reuses its space instead of freeing it is the
// design and a reader outside the process cannot see the difference any other
// way. It is one counter -- the process's own removals, the reclaimer's, and
// the file-system shim's all add to it -- disclosed here twice: whole as
// freed_bytes, and split by what the removal was for as freed_by_purpose.
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
	report.FreedBytes = nonNegativeBytes(paced.FreedBytes())
	// What this process handed its heavy analyzers, and what they used. It is
	// read from the provider's own record rather than from the store because
	// it is process accounting: a unit's reservation and the peak its tree
	// reached belong to the run that started it, not to a stored generation.
	report.AnalyzerUnits = dependence.ObservedUnits()
	// A unit that peaked above what it was admitted against took memory the
	// machine had accounted for elsewhere, which is the direction that freezes
	// a host. The rows above carry both figures; this is the count, so an
	// overrun reaches an operator reading the block rather than only one
	// comparing every row by hand. It is the provider's own total, which also
	// counts the units past the rows' bound. It is absent where this process
	// ran no heavy unit -- nothing was measured -- and zero where it ran them
	// and none overran.
	overran, omitted := dependence.ObservedTotals()
	if len(report.AnalyzerUnits) > 0 || omitted > 0 {
		report.AnalyzerOverrunUnits = &overran
	}
	if omitted > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%d heavy analyzer unit runs are past the %d rows this block carries; the overrun count includes them",
			omitted, dependence.MaxObservedUnits))
	}
	scratchBytes(&report)
	s.pendingWatchEvents(ctx, &report)
	s.reservations(&report)
	s.runLedger(ctx, &report)
	if err := report.Validate(); err != nil {
		return model.ResourceReport{}, err
	}
	return report, nil
}

// warningText is the operator-facing text of a failure a resource figure was
// left absent for: a typed error's code, message and remediation, or the
// error's own text.
func warningText(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		if typed.Remediation != "" {
			return typed.Code + ": " + typed.Message + " -- " + typed.Remediation
		}
		return typed.Code + ": " + typed.Message
	}
	return err.Error()
}

// runLedger fills the run this repository last recorded and the stages it
// spent its time in. The run is the live one where a run is going and
// otherwise the one that produced the active generation, which is the ledger's
// own choice and not a judgement made here: a status surface must never decide
// for itself whether another process is still alive.
//
// The stages are ordered once, here, by wall descending with the run's own
// ordinal breaking a tie. Ordering at assembly and not in each renderer is what
// makes the table and the JSON the same rows in the same order; two surfaces of
// one model that sorted separately could disagree about which stage cost the
// most, and only one of them would be read.
//
// A ledger that cannot be read leaves the two rows absent and the report
// succeeds, for the reason the store's sizes do: the host figures the sampler
// produced are still true, and an unreadable accounting file must not suppress
// the memory reading an operator is diagnosing a memory problem with. What it
// does NOT do is stay silent: the reason goes into the block's warnings, so
// "this workspace never recorded a run" and "your ledger is unreadable" are
// not the same answer.
func (s *Service) runLedger(ctx context.Context, report *model.ResourceReport) {
	if s.opts.Ledger == nil {
		return
	}
	// A repository with no active pointer yet is asking about its most recent
	// run, which is what a generation of zero means to the ledger; a store
	// that cannot answer is the same question.
	generation, err := s.opts.Store.ActiveGeneration(ctx, s.opts.Repo)
	if err != nil {
		generation = 0
	}
	run, stages, omitted, err := s.opts.Ledger.LatestRun(ctx, s.opts.Repo, generation)
	if err != nil {
		// A ledger that cannot be read is not a workspace that has never
		// recorded a run, and the two are the same picture with the rows
		// simply absent: a schema-mismatched or corrupt ledger carries a typed
		// error with its own remediation, and dropping it left an operator
		// looking at a block that says nothing is wrong.
		report.Warnings = append(report.Warnings, "the run accounting could not be read: "+warningText(err))
		return
	}
	if run == nil {
		return
	}
	slices.SortStableFunc(stages, func(a, b model.StageRecord) int {
		if order := cmp.Compare(b.WallMS, a.WallMS); order != 0 {
			return order
		}
		return cmp.Compare(a.Seq, b.Seq)
	})
	report.Run, report.Stages, report.StagesOmitted = run, stages, omitted
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
// A pool that cannot be read leaves the held figure absent rather than short:
// a partial sum there would read as "the run is holding less than it is",
// which is the one thing that figure exists to rule out. It leaves the others
// alone: what the run has freed is counted in the process, not read off the
// disk, so an unreadable pool says nothing about it. A pending figure that
// cannot be read is absent for the same reason. Neither absence is silent: the
// reason goes into the block's warnings, so an unreadable figure and a figure
// this host does not measure are not the same answer. The purposes the
// freeing WAS for are reported beside it, and an empty map is left absent
// because a process that has freed nothing for any named purpose has nothing
// to say rather than a measured zero per purpose.
func scratchBytes(report *model.ResourceReport) {
	var total int64
	readable := true
	for _, a := range scratch.All() {
		n, err := a.Bytes()
		if err != nil {
			readable = false
			report.Warnings = append(report.Warnings, "the scratch pools could not be read: "+warningText(err))
			break
		}
		total += n
	}
	if readable {
		report.ScratchBytes = nonNegativeBytes(total)
	}
	if pending, err := paced.PendingFreeBytes(); err == nil {
		report.PendingFreeBytes = nonNegativeBytes(pending)
	} else {
		report.Warnings = append(report.Warnings, "the space waiting to be freed could not be read: "+warningText(err))
	}
	// What is waiting, and what is waiting on something that will not
	// resolve itself. A removal the filesystem refuses keeps its space in the
	// pending figure for the life of the process, so the figure alone would
	// read as a backlog the pace is working through.
	// The list is paged at the response bound, and the rest is counted.
	var stuckFrees []model.StuckFree
	for _, stuck := range paced.StuckFrees() {
		stuckFrees = append(stuckFrees, model.StuckFree{Entry: stuck.Entry, Reason: stuck.Reason})
	}
	report.StuckFrees, report.StuckFreesOmitted = model.PageStuckFrees(stuckFrees)
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

// reservations fills the figures that are not measured but promised: what this
// process has set aside for concurrent queries, for the parsed-graph cache and
// for the indexing queue, and beside them the one allocation every heavy child
// of this process -- engine runs, external indexers, language servers -- is
// admitted against, with the sum currently reserved against it. Two of the
// first three are configuration and the third is how many queries this
// machine's cores run at once, so all three are always available and a zero
// there is a real zero.
//
// Each admission pair is read together from the ledger so the two figures are
// one moment rather than two, and is absent altogether when this composition
// has no ledger: an unavailable figure is never published as zero, and a zero
// allocation would read as a process that may run nothing. An allocation the
// composition marked unobserved -- the platform published no memory or no
// free-space figure, and the ledger admits against a stand-in -- is absent
// too, because it is not a measurement; what is reserved against it is still
// a real sum and is reported.
//
// The three are the reservations config.BaseFootprint adds to this build's idle
// overhead from the resources and index blocks; it adds the store's page
// caches too, which the storage block states. The report states them and
// derives nothing -- a second implementation of that arithmetic would drift
// from the one the allocation is computed against. None of the three is
// checked against a ceiling: the footprint follows the reservations, so there
// is no figure here for an operator to exceed.
func (s *Service) reservations(report *model.ResourceReport) {
	res := s.opts.Config.Resources
	// Validation refuses a configuration whose product overflows (mulNoOverflow),
	// so the multiplication here cannot wrap on a loaded configuration; a
	// negative from any other path is absent rather than published as a huge
	// unsigned.
	report.QueryReservationBytes = nonNegativeBytes(int64(config.QuerySlots()) * res.QueryMemoryBytes)
	report.CacheReservationBytes = nonNegativeBytes(res.CacheBytes)
	report.QueueReservationBytes = nonNegativeBytes(s.opts.Config.Index.QueueBytes)
	if s.opts.Admission != nil {
		allocation, reserved := s.opts.Admission.Snapshot()
		if s.opts.AdmissionMemoryObserved {
			report.AdmissionAllocationBytes = nonNegativeBytes(allocation)
		}
		report.AdmissionReservedBytes = nonNegativeBytes(reserved)
		// The ledger gates on two dimensions and a child can wait on either,
		// so both are disclosed or the report explains only half of a wait.
		diskAllocation, diskReserved := s.opts.Admission.DiskSnapshot()
		if s.opts.AdmissionDiskObserved {
			report.AdmissionDiskAllocationBytes = nonNegativeBytes(diskAllocation)
		}
		report.AdmissionDiskReservedBytes = nonNegativeBytes(diskReserved)
	}
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
