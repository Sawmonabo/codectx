package diagnostics

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// Check names. They are stable identifiers an operator and a script both key
// on, so they are declared once here rather than spelled at the call site.
// None of them names an analyzer backend by its product name: Section 21 keeps
// the dependence engine anonymous everywhere, and a diagnostic report is not an
// exception.
const (
	checkBuild            = "build"
	checkDataDirectory    = "data_directory"
	checkFreeDisk         = "free_disk_space"
	checkStorageIntegrity = "storage_integrity"
	checkStorageAccount   = "storage_accounting"
	checkActiveGeneration = "active_generation"
	checkSourceRetention  = "source_retention"
	checkSuppliedIndex    = "supplied_index"
	checkWatchHeartbeat   = "watch_heartbeat"
	checkAnalyzerRestrict = "analyzer_restriction"
	checkTemporaryState   = "temporary_state"
	checkBundledGrammars  = "bundled_grammars"
	checkCaptureFreshness = "capture_freshness"
	// checkToolchainPrefix prefixes one check per lock entry; the suffix is
	// the entry name from tools.lock.json, which is what a managed tool is
	// called everywhere else in this product.
	checkToolchainPrefix = "toolchain:"
)

// Sample sizes for the content-addressed-storage probe. An ordinary call spot
// checks a handful of objects; --deep widens the sample. Neither is a scan:
// the store bounds the sample again on its side.
const (
	casSampleOrdinary = 4
	casSampleDeep     = 64
)

// blobSampler is the hash source the content-addressed-storage check needs.
//
// It is an optional interface rather than a StoreReader method because
// StoreReader is frozen: a store that cannot list hashes makes the check
// unavailable with a reason, which is the same nil-versus-zero discipline one
// level up. The composition root's adapter must forward
// (*sqlite.Store).SampleBlobs, or this check reports unavailable forever.
type blobSampler interface {
	SampleBlobs(ctx context.Context, limit int) ([]string, error)
}

// SuppliedIndex is one `--scip-index` path recorded against a generation, and
// whether it resolved to an imported unit. It exists so a typo'd path is
// distinguishable from no path at all: today both leave the repository indexed
// without imported symbols and nothing tells the two apart.
type SuppliedIndex struct {
	// Path is the root-relative path the user supplied. It is root-relative
	// by construction, so reporting it discloses no private absolute root.
	Path string
	// Resolved is true when the generation holds an imported unit for Path.
	Resolved bool
}

// SuppliedIndexReader reports the supplied indexes recorded against a
// generation. Like blobSampler it is optional: a store that does not implement
// it yields an unavailable check with a reason rather than a silent pass.
//
// It is the one optional probe interface exported from this package, because it
// is the one whose adapter must convert rather than forward: a composition root
// method with the right name and the wrong signature satisfies nothing, fails
// no build and leaves the check permanently unavailable. Exported, the
// composition root asserts it at compile time instead.
type SuppliedIndexReader interface {
	SuppliedIndexes(ctx context.Context, gen model.GenerationID) ([]SuppliedIndex, error)
}

// WatchHeartbeat is what a running watch published about itself, read from the
// store by a process that is not the one watching.
//
// ExpiresAt is the writer's own deadline and the only liveness signal this
// package consults. A watching process refreshes it while it lives, so an
// expired row means the writer stopped refreshing -- the one signal of process
// death that needs no pid probe and works on every platform. LastPassAt and
// PendingEvents are absent when nothing measured them: a watch driven only by
// periodic reconciliation counts no notification queue, and a watch that has
// completed no pass has no pass time.
type WatchHeartbeat struct {
	WriterPID     int
	LastPassAt    *time.Time
	PendingEvents *int64
	ExpiresAt     time.Time
}

// WatchHeartbeatReader reports the repository's watch heartbeat, and whether
// one exists at all -- absent and expired are different answers. Like
// SuppliedIndexReader it is optional, exported, and converts rather than
// forwards, so the composition root asserts it at compile time instead of
// leaving a signature drift to show up as a check that is unavailable forever.
type WatchHeartbeatReader interface {
	WatchHeartbeat(ctx context.Context, repo model.RepositoryID) (WatchHeartbeat, bool, error)
}

// liveWatch reports the heartbeat of a watch that is running now: the row
// exists and the writer's own deadline has not passed. An expired row is a
// watch that stopped, and is reported as no watch at all rather than as
// coverage -- a figure from a dead process would read as "the watch is caught
// up" for as long as nobody rebooted it.
//
// The second result separates "no heartbeat is live" from "this build cannot
// tell", which the two callers render differently.
func (s *Service) liveWatch(ctx context.Context) (hb WatchHeartbeat, live bool, ok bool) {
	reader, isReader := s.opts.Store.(WatchHeartbeatReader)
	if !isReader {
		return WatchHeartbeat{}, false, false
	}
	hb, found, err := reader.WatchHeartbeat(ctx, s.opts.Repo)
	if err != nil {
		return WatchHeartbeat{}, false, false
	}
	if !found {
		return WatchHeartbeat{}, false, true
	}
	return hb, hb.ExpiresAt.After(s.opts.Now()), true
}

// captureReader reports when the capture behind a generation was taken -- the
// snapshot's own creation time, not the generation's. Like blobSampler it is an
// optional interface on the frozen StoreReader, so a reader that cannot answer
// makes the freshness check `unavailable` with its reason instead of leaving
// Section 22's "recent capture/freshness" silently unreported.
type captureReader interface {
	GenerationCapturedAt(ctx context.Context, gen model.GenerationID) (time.Time, error)
}

// Doctor runs the Section 22 check list.
//
// Two rules shape the whole function. First, a probe failure is a *check*, not
// an error: the one command meant to diagnose a broken workspace must produce a
// report on a broken workspace, so Doctor returns an error only for an invalid
// request or a report it could not build. Second, every detail and remediation
// is generated from the typed code, never from a probe's own message: those
// messages carry SQL text, absolute private paths and analyzer output, and
// Section 21 closes that disclosure channel everywhere else.
//
// The expensive work belongs to req.Deep alone: an ordinary call runs no
// full-database integrity pass and samples a handful of objects, because every
// `version` and `search` would otherwise pay for a scan.
func (s *Service) Doctor(ctx context.Context, req model.DoctorRequest) (model.DoctorReport, error) {
	if err := req.Validate(); err != nil {
		return model.DoctorReport{}, err
	}
	// One Stats read serves both the checks that need it. Two reads in one
	// report can disagree with each other, and the accounting figures an
	// operator is shown must be the same ones the retention sample reasoned
	// about.
	//
	// It is read only under --deep. Stats is eleven `count(*)` scans, one per
	// table, and node_facts, relation_facts and evidence are the largest tables
	// this product writes: on a large repository that is a second whole-database
	// walk beside the integrity one, on a command Section 22 forbids scanning.
	// Shallow reports the sizes it can stat and marks the counts unverified.
	var stats StoreStats
	var statsErr error
	if req.Deep {
		stats, statsErr = s.opts.Store.Stats(ctx)
	}
	checks := make([]model.DoctorCheck, 0, 24)
	checks = append(checks,
		s.checkBuild(),
		s.checkDataDirectory(ctx),
		s.checkFreeDisk(ctx),
		s.checkStorage(ctx, req.Deep),
		s.checkAccounting(ctx, req.Deep, stats, statsErr),
	)
	active, generationCheck := s.checkActive(ctx)
	checks = append(checks,
		generationCheck,
		s.checkCaptureFreshness(ctx, active),
		s.checkCAS(ctx, req.Deep, stats, statsErr),
		s.checkSuppliedIndex(ctx, active),
		s.checkWatchHeartbeat(ctx),
		s.checkGrammars(),
		s.checkTemporary(ctx),
		s.checkAnalyzerRestriction(),
	)
	checks = append(checks, s.checkToolchain(ctx)...)
	// The state is reduced BEFORE the list is bounded: a failing check that
	// fell off the end of an over-long list must still fail the report.
	state := aggregate(checks)
	// No clamp. The check list is enumerated above -- it is a function of the
	// code, not of the repository -- and a silent `checks[:256]` dropped the
	// toolchain checks appended last with no omitted count and no warning,
	// which is exactly the shape a diagnostic report may not have.
	report := model.DoctorReport{
		Build: s.opts.Build,
		Deep:  req.Deep,
		// Offline is echoed, not acted on. Doctor opens no socket: every
		// probe it runs is local, and the toolchain report is the cheap
		// local one that never fetches. Whether a real OS-level restriction
		// confines the analyzers is answered by checkAnalyzerRestriction as
		// its own check -- and answered `unavailable`, because nothing in this
		// build measures it.
		Offline:   req.Offline,
		State:     state,
		Checks:    checks,
		CheckedAt: s.opts.Now(),
	}
	if err := report.Validate(); err != nil {
		return model.DoctorReport{}, err
	}
	return report, nil
}

// aggregate reduces the check list to the report's state. A failure dominates a
// warning, which dominates a pass; an unavailable check does NOT degrade the
// report, because Section 22 treats an honestly unavailable measurement as an
// answer and not as a defect. Collapsing unavailable into warn would make every
// non-Linux host report a permanently unhealthy workspace.
func aggregate(checks []model.DoctorCheck) model.CheckState {
	state := model.CheckPass
	for _, c := range checks {
		switch c.State {
		case model.CheckFail:
			return model.CheckFail
		case model.CheckWarn:
			state = model.CheckWarn
		}
	}
	return state
}

// checkBuild reports the build identity and pins the machine-readable output
// contract. A binary whose compiled-in schema version disagrees with the model
// package it was built against cannot be trusted to describe its own output.
func (s *Service) checkBuild() model.DoctorCheck {
	if s.opts.Build.SchemaVersion != model.SchemaVersion {
		return model.DoctorCheck{Name: checkBuild, State: model.CheckFail,
			Detail:      "this binary reports output schema version " + s.opts.Build.SchemaVersion + ", but the contract it was built against is " + model.SchemaVersion,
			Code:        model.CodeSchemaMismatch,
			Remediation: "reinstall codectx; this build is inconsistent with itself"}
	}
	return model.DoctorCheck{Name: checkBuild, State: model.CheckPass,
		Detail: "version " + s.opts.Build.Version + ", commit " + s.opts.Build.Commit + ", output schema " + s.opts.Build.SchemaVersion}
}

// checkDataDirectory proves the two directories this command needs: the data
// directory, which must be writable, and the workspace root, which must be
// readable. Neither is named in the detail: both are private absolute paths,
// and Section 21 keeps those out of ordinary output.
//
// The workspace is probed for reading only. Section 6 forbids this product
// writing into the repository at all, so the create-and-remove proof the data
// directory gets would itself be the defect if it were aimed at the workspace;
// reading one entry is the whole of what a capture asks of the root, and a root
// this process cannot read produces an empty capture that reads as a repository
// with nothing in it.
//
// The readable probe is not the first gate the root passes. `workspace_open`
// (cli/doctor.go) reports the os.OpenRoot at internal/workspace/workspace.go:92,
// which already refuses a root this process cannot open at all -- a missing
// directory or one whose permissions deny entry never reaches here. What is
// left for this branch is the narrower case that open admits and listing does
// not: a ReadDir that fails for a reason other than permission, such as an I/O
// error or a filesystem that refuses to enumerate. The branch is live; it is
// simply not the one that catches an unreadable repository root.
func (s *Service) checkDataDirectory(ctx context.Context) model.DoctorCheck {
	if err := s.opts.Workspace.Writable(ctx, s.opts.Config.Storage.DataDir); err != nil {
		return failure(checkDataDirectory, err)
	}
	if err := s.opts.Workspace.Readable(ctx, s.opts.Root); err != nil {
		return failure(checkDataDirectory, err)
	}
	return model.DoctorCheck{Name: checkDataDirectory, State: model.CheckPass,
		Detail: "the data directory is readable and writable, and the workspace root is readable"}
}

// checkFreeDisk is the first and only reader of resources.min_free_disk_bytes:
// the key exists so an operator learns about disk pressure from `doctor` before
// a capture fails half way through with CTX_DISK_FULL.
//
// A host that cannot report free space yields unavailable with the reason, not
// a pass and not a zero: a filesystem whose free space is unknown is not a
// filesystem known to have room.
func (s *Service) checkFreeDisk(ctx context.Context) model.DoctorCheck {
	want := s.opts.Config.Resources.MinFreeDiskBytes
	free, err := s.opts.Workspace.FreeDiskBytes(ctx, s.opts.Config.Storage.DataDir)
	if err != nil {
		return failure(checkFreeDisk, err)
	}
	if free == nil {
		return model.DoctorCheck{Name: checkFreeDisk, State: model.CheckUnavailable,
			Detail: "this host reports no free-space figure for the data directory, so the " + bytesPhrase(want) + " minimum could not be checked"}
	}
	if want > 0 && *free < uint64(want) {
		return model.DoctorCheck{Name: checkFreeDisk, State: model.CheckFail,
			Detail:      bytesPhrase(int64(*free)) + " free under the data directory, below the configured minimum of " + bytesPhrase(want),
			Code:        model.CodeDiskFull,
			Remediation: "free space under the data directory, or lower resources.min_free_disk_bytes"}
	}
	return model.DoctorCheck{Name: checkFreeDisk, State: model.CheckPass, Detail: bytesPhrase(int64(*free)) + " free under the data directory"}
}

// checkStorage runs the integrity checks. deep is passed straight through, and
// it decides which check this row reports: shallow reads the database header,
// the schema fingerprint and the journal mode -- all constant cost -- while
// quick_check, the foreign key check and the full-text index walk are O(database
// bytes) and belong to --deep alone.
//
// A shallow pass is therefore reported `unverified`, never `pass`. The row is
// still emitted with the flag that verifies it: nothing is dropped from the
// report, and an operator is never told the database passed a check this run did
// not run. A shallow FAILURE is a real failure -- a fingerprint mismatch or a
// truncated header is decided without reading a page of content.
func (s *Service) checkStorage(ctx context.Context, deep bool) model.DoctorCheck {
	if err := s.opts.Store.Check(ctx, deep); err != nil {
		return failure(checkStorageIntegrity, err)
	}
	if !deep {
		return model.DoctorCheck{Name: checkStorageIntegrity, State: model.CheckUnverified,
			Detail: "the index database header, page count, schema fingerprint and write-ahead-log mode read back; " +
				"the integrity and referential checks walk the whole database and were " + shallowUnverified,
			Remediation: shallowRemediation}
	}
	return model.DoctorCheck{Name: checkStorageIntegrity, State: model.CheckPass,
		Detail: "the index database passed its full integrity checks, including the search index"}
}

// shallowUnverified is the one phrase every check skipped by shallow mode ends
// with, and shallowRemediation the one remediation beside it. They are declared
// once so an operator and a script see the same wording whichever check skipped.
const (
	shallowUnverified  = "not verified in shallow mode; run doctor --deep"
	shallowRemediation = "run codectx doctor --deep to verify this check"
)

// checkAccounting reports the bounded row counts and file sizes, and warns when
// the write-ahead log has grown past its high-water mark: a WAL that never
// checkpoints is how a workspace runs a disk out of space while every
// individual operation still succeeds. The lease and session counts are the
// retention figures Section 22 asks for -- they are counts, never identities.
func (s *Service) checkAccounting(ctx context.Context, deep bool, stats StoreStats, err error) model.DoctorCheck {
	if !deep {
		return s.checkAccountingShallow(ctx)
	}
	if err != nil {
		return failure(checkStorageAccount, err)
	}
	detail := strconv.FormatInt(stats.Generations, 10) + " generations, " +
		strconv.FormatInt(stats.Units, 10) + " units, " +
		strconv.FormatInt(stats.Blobs, 10) + " retained blobs, " +
		strconv.FormatInt(stats.Leases, 10) + " live leases, " +
		strconv.FormatInt(stats.Sessions, 10) + " read sessions; database " +
		bytesPhrase(stats.DatabaseBytes) + ", write-ahead log " + bytesPhrase(stats.WALBytes)
	if high := s.opts.Config.Storage.WALHighWaterBytes; high > 0 && stats.WALBytes > high {
		return model.DoctorCheck{Name: checkStorageAccount, State: model.CheckWarn,
			Detail:      detail,
			Code:        model.CodeResourceLimit,
			Remediation: "the write-ahead log is past its high-water mark; run an index or refresh to checkpoint it"}
	}
	return model.DoctorCheck{Name: checkStorageAccount, State: model.CheckPass, Detail: detail}
}

// checkAccountingShallow reports what accounting costs nothing: the two file
// sizes, and the write-ahead-log high-water warning that is the one actionable
// fact in this check. The row counts are O(rows) and are reported unverified.
//
// The WAL warning is deliberately NOT suppressed by shallow mode. A WAL that
// never checkpoints fills a disk while every operation still succeeds, and it is
// decided by a file stat; withholding it until --deep would hide the cheapest
// real finding this command has behind the most expensive flag.
func (s *Service) checkAccountingShallow(ctx context.Context) model.DoctorCheck {
	sizer, ok := s.opts.Store.(StoreSizer)
	if !ok {
		return model.DoctorCheck{Name: checkStorageAccount, State: model.CheckUnavailable,
			Detail: "this build's store reader reports no on-disk sizes without a full row count, so no accounting figure could be read"}
	}
	dbBytes, walBytes, err := sizer.StoreSizes(ctx)
	if err != nil {
		return failure(checkStorageAccount, err)
	}
	detail := "database " + bytesPhrase(dbBytes) + ", write-ahead log " + bytesPhrase(walBytes) +
		"; the generation, unit, blob, lease and session counts are " + shallowUnverified
	if high := s.opts.Config.Storage.WALHighWaterBytes; high > 0 && walBytes > high {
		return model.DoctorCheck{Name: checkStorageAccount, State: model.CheckWarn,
			Detail:      detail,
			Code:        model.CodeResourceLimit,
			Remediation: "the write-ahead log is past its high-water mark; run an index or refresh to checkpoint it"}
	}
	return model.DoctorCheck{Name: checkStorageAccount, State: model.CheckUnverified,
		Detail: detail, Remediation: shallowRemediation}
}

// checkActive resolves the active generation pointer and returns it, so the
// checks that need a generation read the pointer once rather than each taking
// their own and disagreeing with this report's own answer.
func (s *Service) checkActive(ctx context.Context) (model.GenerationID, model.DoctorCheck) {
	gen, err := s.opts.Store.ActiveGeneration(ctx, s.opts.Repo)
	if err != nil {
		return 0, failure(checkActiveGeneration, err)
	}
	return gen, model.DoctorCheck{Name: checkActiveGeneration, State: model.CheckPass,
		Detail: "generation " + strconv.FormatInt(int64(gen), 10) + " is published and serving queries"}
}

// checkCAS reads back the integrity metadata of a bounded sample of retained
// objects: the size, the block digests in block order and the line checkpoints
// the store recorded for each. It catches a retained object whose recorded
// metadata has gone missing or stopped describing itself -- the state in which
// no reader can verify the bytes it is handed.
//
// What it deliberately does NOT claim: the object's bytes on disk are not read
// here, because the frozen store reader exposes no way to open one. A truncated
// or replaced content-addressed object is therefore still reported as sound by
// this check, and saying otherwise would grant exactly the false readiness
// Section 22 exists to prevent.
func (s *Service) checkCAS(ctx context.Context, deep bool, stats StoreStats, statsErr error) model.DoctorCheck {
	sampler, ok := s.opts.Store.(blobSampler)
	if !ok {
		return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckUnavailable,
			Detail: "this build's store reader lists no retained object hashes, so no sample could be verified"}
	}
	limit := casSampleOrdinary
	if deep {
		limit = casSampleDeep
	}
	hashes, err := sampler.SampleBlobs(ctx, limit)
	if err != nil {
		return failure(checkSourceRetention, err)
	}
	if len(hashes) == 0 {
		// The sample lists ready objects only. A store whose blob rows are
		// all quarantined or in trash returns nothing here, and reporting
		// that as "nothing to verify" would read as a healthy empty
		// workspace, so the accounting count decides which it is.
		if deep {
			if statsErr == nil && stats.Blobs > 0 {
				return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckWarn,
					Detail:      "this workspace records " + strconv.FormatInt(stats.Blobs, 10) + " retained objects, none of them in a readable state",
					Code:        model.CodeSourceIntegrity,
					Remediation: "rebuild the cache with codectx index --rebuild"}
			}
			return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckPass, Detail: "no source is retained yet, so there was nothing to verify"}
		}
		// Telling the two apart needs the retained-object count, which is one
		// of the O(rows) figures shallow mode does not read. "Nothing to
		// verify" and "every retained object is unreadable" look identical
		// from here, and reporting the reassuring one would be a guess.
		return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckUnverified,
			Detail:      "no retained object was listed in a readable state; whether that is an empty workspace or an unreadable one is " + shallowUnverified,
			Remediation: shallowRemediation}
	}
	for _, h := range hashes {
		if _, err := s.opts.Store.Blob(ctx, h); err != nil {
			return failure(checkSourceRetention, err)
		}
	}
	return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckPass,
		Detail: "the recorded integrity metadata of " + strconv.Itoa(len(hashes)) + " retained objects is present and self-consistent"}
}

// checkSuppliedIndex distinguishes three states a user cannot tell apart today:
// this generation was built with no supplied index, with one that was imported,
// or with one whose path resolved to nothing. The third is the dangerous one --
// a typo in `--scip-index` plans no unit, the command exits 0, and the
// repository reads as fully indexed while its cross-file symbols are missing.
func (s *Service) checkSuppliedIndex(ctx context.Context, active model.GenerationID) model.DoctorCheck {
	reader, ok := s.opts.Store.(SuppliedIndexReader)
	if !ok {
		return model.DoctorCheck{Name: checkSuppliedIndex, State: model.CheckUnavailable,
			Detail: "this build records no supplied-index path against a generation, so a path that resolved to nothing cannot be told from no path at all"}
	}
	if active == 0 {
		return model.DoctorCheck{Name: checkSuppliedIndex, State: model.CheckUnavailable,
			Detail: "no generation is published, so no supplied index could be checked"}
	}
	supplied, err := reader.SuppliedIndexes(ctx, active)
	if err != nil {
		return failure(checkSuppliedIndex, err)
	}
	var unresolved []string
	for _, si := range supplied {
		if !si.Resolved {
			unresolved = append(unresolved, si.Path)
		}
	}
	switch {
	case len(unresolved) > 0:
		return model.DoctorCheck{Name: checkSuppliedIndex, State: model.CheckFail,
			Detail:      "this generation was built with a supplied index at " + joinNames(unresolved) + ", which matched no file and imported nothing",
			Code:        model.CodeScopeIncomplete,
			Remediation: "check the --scip-index path against the repository root and index again; the path is root-relative"}
	case len(supplied) > 0:
		return model.DoctorCheck{Name: checkSuppliedIndex, State: model.CheckPass,
			Detail: strconv.Itoa(len(supplied)) + " supplied index/indexes were imported into this generation"}
	default:
		return model.DoctorCheck{Name: checkSuppliedIndex, State: model.CheckPass,
			Detail: "this generation was built with no supplied index"}
	}
}

// checkTemporary reports orphan temporary state against resources.max_temp_bytes.
// The figure comes from the sampler because it is a host measurement, and a
// host that cannot measure it reports unavailable -- a temp total that failed to
// read is not a temp total of zero.
func (s *Service) checkTemporary(ctx context.Context) model.DoctorCheck {
	report, err := s.opts.Sampler.Sample(ctx)
	if err != nil {
		return failure(checkTemporaryState, err)
	}
	if report.TempBytes == nil {
		return model.DoctorCheck{Name: checkTemporaryState, State: model.CheckUnavailable,
			Detail: "this host reports no temporary-state total, so it could not be checked against the configured ceiling"}
	}
	want := s.opts.Config.Resources.MaxTempBytes
	if want > 0 && *report.TempBytes > uint64(want) {
		return model.DoctorCheck{Name: checkTemporaryState, State: model.CheckWarn,
			Detail:      bytesPhrase(int64(*report.TempBytes)) + " of temporary state, above the configured ceiling of " + bytesPhrase(want),
			Code:        model.CodeResourceLimit,
			Remediation: "run codectx index or refresh to reclaim abandoned staging state, or raise resources.max_temp_bytes"}
	}
	return model.DoctorCheck{Name: checkTemporaryState, State: model.CheckPass,
		Detail: bytesPhrase(int64(*report.TempBytes)) + " of temporary state"}
}

// checkToolchain reports one check per lock entry, named by the entry. A tool
// the lock names for another platform is unavailable, not a failure: that is
// the honest answer on a host the payload was never published for.
func (s *Service) checkToolchain(ctx context.Context) []model.DoctorCheck {
	statuses, err := s.opts.Toolchain.Statuses(ctx)
	if err != nil {
		return []model.DoctorCheck{failure(checkToolchainPrefix+"status", err)}
	}
	out := make([]model.DoctorCheck, 0, len(statuses))
	for _, st := range statuses {
		c := model.DoctorCheck{Name: checkToolchainPrefix + st.Name, State: model.CheckPass,
			Detail: st.Name + " " + st.Version + " is " + string(st.State)}
		switch st.State {
		case toolchain.StateAvailable:
			c.State = model.CheckWarn
			// StateAvailable means the lock names a payload for this platform
			// that is not on disk. Rendering the state word into the detail
			// read as "gopls 0.23.0 is available", the opposite of what the
			// remediation beside it tells the operator to do.
			c.Detail = st.Name + " " + st.Version + " is pinned by the lock but not installed"
			c.Code = model.CodeProviderUnavailable
			c.Remediation = "run codectx tools install to fetch the payloads this lock names"
		case toolchain.StateUnsupportedPlatform:
			c.State = model.CheckUnavailable
			c.Detail = st.Name + " publishes no payload for this platform"
		case toolchain.StateCorrupt:
			c.State = model.CheckFail
			c.Code = model.CodeToolCorrupt
			c.Remediation = "run codectx tools gc and then codectx tools install to reinstall this payload"
		}
		out = append(out, c)
	}
	return out
}

// failure turns a probe error into a check whose detail and remediation are
// generated from the typed code alone.
//
// The probe's own message is deliberately discarded. Those messages carry SQL
// fragments (the store wraps the failing statement), absolute private paths
// (a filesystem probe names the directory it could not open) and analyzer
// output; passing any of it through would make the one command an operator
// runs on a sick workspace the disclosure channel Section 21 closes everywhere
// else. An unrecognised code still produces a check, never a panic and never a
// raw string.
func failure(name string, err error) model.DoctorCheck {
	code := model.CodeInternal
	var typed *model.Error
	if errors.As(err, &typed) {
		code = typed.Code
	}
	c := model.DoctorCheck{Name: name, State: model.CheckFail, Code: code}
	switch code {
	case model.CodeStorageCorrupt:
		c.Detail = "the index database failed its integrity checks"
		c.Remediation = "rebuild the cache with codectx index --rebuild"
	case model.CodeSchemaMismatch:
		c.Detail = "the cache was written by a different schema version than this build understands"
		c.Remediation = "rebuild the cache with codectx index --rebuild"
	case model.CodeSourceIntegrity:
		c.Detail = "retained source did not verify against the digests recorded for it"
		c.Remediation = "rebuild the cache with codectx index --rebuild"
	case model.CodeDiskFull:
		c.Detail = "the data directory has no room left"
		c.Remediation = "free space under the data directory, then retry"
	case model.CodeNoActiveGeneration:
		c.State = model.CheckWarn
		c.Detail = "this repository has no published generation"
		c.Remediation = "run codectx index to build one"
	case model.CodeWorkspaceBusy:
		c.State = model.CheckWarn
		c.Detail = "another process holds this workspace"
		c.Remediation = "retry once the other codectx process finishes"
	case model.CodeResourceLimit:
		c.State = model.CheckWarn
		c.Detail = "the check stopped at a configured resource bound"
		c.Remediation = "raise the resources bound this check names, or reduce the workspace"
	case model.CodeScopeIncomplete:
		c.State = model.CheckWarn
		c.Detail = "part of the configured analysis scope produced nothing"
		c.Remediation = "run codectx index and check the completeness rows it reports"
	case model.CodeCanceled:
		c.State = model.CheckUnavailable
		c.Code = ""
		c.Detail = "the check was canceled before it completed"
	case model.CodePathEscape, model.CodeTrustRequired, model.CodeConfigInvalid, model.CodeWorkspaceNotFound:
		c.Detail = "the workspace or its configuration refused this check"
		c.Remediation = "run codectx init and check the configuration this workspace loads"
	default:
		c.Detail = "the check did not complete"
		c.Remediation = "run codectx doctor --deep, and report this with the command you ran"
	}
	return c
}

// joinNames renders a bounded list of names -- root-relative supplied-index
// paths, configured language tags -- into one detail. Both lists are bounded by
// the configuration or the generation that produced them, and the detail is
// bounded again by model.MaxDetailBytes in DoctorCheck.Validate.
func joinNames(names []string) string {
	out := names[0]
	for _, p := range names[1:] {
		out += ", " + p
	}
	return out
}

// bytesPhrase renders a byte count for a human detail. It stays a plain decimal
// count with a unit: a doctor detail is read by an operator and parsed by
// nobody, and a rounded "1.2 GiB" hides the difference between just over and
// just under a configured bound, which is exactly what these checks report.
func bytesPhrase(n int64) string { return strconv.FormatInt(n, 10) + " bytes" }

// checkAnalyzerRestriction answers Section 21's question -- whether an
// OS-level restriction confines the analyzer subprocesses this installation
// launches -- and answers it `unavailable`.
//
// That is the whole point of the check, and it is deliberately not a silent
// omission. Nothing in this build measures a sandbox, a seccomp filter, a job
// object restriction or a container policy: the runners set process-group and
// job-object lifetime control, which bounds what a child outlives, not what a
// child may reach. `codectx doctor --offline` therefore reports the question as
// asked and unmeasured rather than letting the flag read as an assertion that
// the restriction is in force -- a false readiness claim of exactly the kind
// Section 22 exists to prevent.
//
// It takes no context because it performs no probe. When a real measurement
// lands it replaces this body and nothing else changes: the name, and the rule
// that the report never claims enforcement it did not observe, are the contract.
func (s *Service) checkAnalyzerRestriction() model.DoctorCheck {
	return model.DoctorCheck{Name: checkAnalyzerRestrict, State: model.CheckUnavailable,
		Detail: "whether an OS-level restriction confines the analyzer subprocesses is not measured on this platform, so this installation neither claims nor denies that one is active"}
}

// checkWatchHeartbeat reports whether a watch is running for this workspace,
// from the row a watching process publishes and refreshes (Section 13.2).
//
// The three states are three different facts and never collapse. `pass` is a
// live row: some process is watching, and it says which one, so an operator who
// wants it stopped knows what to stop. `warn` is an expired row: a watch ran and
// is no longer refreshing, so every change since is unseen while the workspace
// still looks watched -- the one state an operator must act on. `unavailable` is
// no row at all, which is the ordinary state of a workspace nobody is watching
// and not a defect; it is also what a build with no heartbeat reader reports.
func (s *Service) checkWatchHeartbeat(ctx context.Context) model.DoctorCheck {
	hb, live, ok := s.liveWatch(ctx)
	switch {
	case !ok:
		return model.DoctorCheck{Name: checkWatchHeartbeat, State: model.CheckUnavailable,
			Detail: "this build publishes no watch heartbeat, so whether a watch is running for this workspace cannot be told from another process"}
	case live:
		detail := "a watch is running in process " + strconv.Itoa(hb.WriterPID)
		if hb.PendingEvents != nil {
			detail += ", with " + strconv.FormatInt(*hb.PendingEvents, 10) + " pending event/events"
		}
		if hb.LastPassAt != nil {
			detail += "; its last pass completed " + hb.LastPassAt.UTC().Format(time.RFC3339)
		}
		return model.DoctorCheck{Name: checkWatchHeartbeat, State: model.CheckPass, Detail: detail}
	case hb.ExpiresAt.IsZero():
		return model.DoctorCheck{Name: checkWatchHeartbeat, State: model.CheckUnavailable,
			Detail: "no watch has published a heartbeat for this workspace, so there is no watch coverage to report"}
	default:
		return model.DoctorCheck{Name: checkWatchHeartbeat, State: model.CheckWarn,
			Detail:      "the watch that was running here stopped refreshing its heartbeat at " + hb.ExpiresAt.UTC().Format(time.RFC3339) + ", so changes since then have not been reconciled",
			Code:        model.CodeSnapshotChanged,
			Remediation: "start `codectx watch` again, or run `codectx index` once to reconcile what changed while it was down"}
	}
}

// checkGrammars answers Section 22's bundled-grammar question: whether this
// build carries a structural grammar for every language the workspace has
// configured. It reads the pinned registry compiled into this binary, because
// the question is about this binary -- a figure handed in by a caller could
// describe a build that is not the one running.
//
// A configured language this build bundles no grammar for is a failure, not a
// warning: every file of that language is walked, planned and then parsed by
// nothing, and the repository reads as fully indexed with its structure
// missing. The provider refuses the same configuration when an indexing run
// composes it; this check is where an operator learns it before the run.
func (s *Service) checkGrammars() model.DoctorCheck {
	ts := s.opts.Config.Providers.TreeSitter
	if !ts.Enabled {
		return model.DoctorCheck{Name: checkBundledGrammars, State: model.CheckUnavailable,
			Detail: "structural analysis is disabled in this workspace, so no bundled grammar is required"}
	}
	var missing []string
	for _, name := range ts.Languages {
		if _, ok := lang.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return model.DoctorCheck{Name: checkBundledGrammars, State: model.CheckFail,
			Detail:      "this build bundles no structural grammar for " + joinNames(missing),
			Code:        model.CodeConfigInvalid,
			Remediation: "remove the language from providers.tree_sitter.languages, or install a build that bundles it"}
	}
	return model.DoctorCheck{Name: checkBundledGrammars, State: model.CheckPass,
		Detail: "this build bundles a structural grammar for each of the " + strconv.Itoa(len(ts.Languages)) +
			" configured languages, out of " + strconv.Itoa(len(lang.All)) + " it carries"}
}

// checkCaptureFreshness reports how old the capture behind the published
// generation is. Section 22 asks for recent capture/freshness and nothing else
// reports it: the active-pointer check says a generation is serving queries,
// which is equally true of one captured months ago, and an operator reading a
// stale answer out of this workspace has no other place to learn that.
//
// It states the age and does not judge it. There is no configured staleness
// bound in this product, and inventing one here would turn a measurement into a
// policy no operator set -- the same reason checkAnalyzerRestriction reports
// its question unmeasured rather than answering it.
func (s *Service) checkCaptureFreshness(ctx context.Context, active model.GenerationID) model.DoctorCheck {
	reader, ok := s.opts.Store.(captureReader)
	if !ok {
		return model.DoctorCheck{Name: checkCaptureFreshness, State: model.CheckUnavailable,
			Detail: "this build's store reader reports no capture time for a generation, so the age of the served capture is unknown"}
	}
	if active == 0 {
		return model.DoctorCheck{Name: checkCaptureFreshness, State: model.CheckUnavailable,
			Detail: "no generation is published, so there is no capture to age"}
	}
	captured, err := reader.GenerationCapturedAt(ctx, active)
	if err != nil {
		return failure(checkCaptureFreshness, err)
	}
	return model.DoctorCheck{Name: checkCaptureFreshness, State: model.CheckPass,
		Detail: "the capture this generation serves was taken " + agePhrase(s.opts.Now().Sub(captured)) + " ago"}
}

// agePhrase renders an elapsed duration for an operator. Whole minutes are the
// finest unit a capture age is ever read at, and a negative age -- a capture
// stamped ahead of this host's clock -- is reported as what it is rather than
// as a large positive number.
func agePhrase(d time.Duration) string {
	if d < 0 {
		return "-" + agePhrase(-d)
	}
	switch {
	case d < time.Hour:
		return strconv.FormatInt(int64(d/time.Minute), 10) + " minutes"
	case d < 48*time.Hour:
		return strconv.FormatInt(int64(d/time.Hour), 10) + " hours"
	default:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + " days"
	}
}
