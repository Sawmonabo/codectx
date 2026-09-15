package diagnostics

import (
	"context"
	"errors"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
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
	checkAnalyzerRestrict = "analyzer_restriction"
	checkTemporaryState   = "temporary_state"
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

// suppliedIndexReader reports the supplied indexes recorded against a
// generation. Like blobSampler it is optional: recording the path is the
// producer half of this obligation and lands outside this package, so a store
// that does not implement this yields an unavailable check with a reason
// rather than a silent pass.
type suppliedIndexReader interface {
	SuppliedIndexes(ctx context.Context, gen model.GenerationID) ([]SuppliedIndex, error)
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
	checks := make([]model.DoctorCheck, 0, 16)
	checks = append(checks,
		s.checkBuild(),
		s.checkDataDirectory(ctx),
		s.checkFreeDisk(ctx),
		s.checkStorage(ctx, req.Deep),
		s.checkAccounting(ctx),
	)
	active, generationCheck := s.checkActive(ctx)
	checks = append(checks,
		generationCheck,
		s.checkCAS(ctx, req.Deep),
		s.checkSuppliedIndex(ctx, active),
		s.checkTemporary(ctx),
		s.checkAnalyzerRestriction(),
	)
	checks = append(checks, s.checkToolchain(ctx)...)
	// The state is reduced BEFORE the list is bounded: a failing check that
	// fell off the end of an over-long list must still fail the report.
	state := aggregate(checks)
	if len(checks) > model.MaxCapabilityStates {
		checks = checks[:model.MaxCapabilityStates]
	}
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

// checkDataDirectory proves the data directory is writable. The directory is
// not named in the detail: it is a private absolute path, and Section 21 keeps
// those out of ordinary output.
func (s *Service) checkDataDirectory(ctx context.Context) model.DoctorCheck {
	if err := s.opts.Workspace.Writable(ctx, s.opts.Config.Storage.DataDir); err != nil {
		return failure(checkDataDirectory, err)
	}
	return model.DoctorCheck{Name: checkDataDirectory, State: model.CheckPass, Detail: "the data directory is readable and writable"}
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

// checkStorage runs the integrity checks. deep is passed straight through: the
// store's own contract is that an ordinary pass is quick_check plus the foreign
// key check, and the full-text index walk happens only when deep is set.
func (s *Service) checkStorage(ctx context.Context, deep bool) model.DoctorCheck {
	if err := s.opts.Store.Check(ctx, deep); err != nil {
		return failure(checkStorageIntegrity, err)
	}
	detail := "the index database passed its quick integrity and referential checks"
	if deep {
		detail = "the index database passed its full integrity checks, including the search index"
	}
	return model.DoctorCheck{Name: checkStorageIntegrity, State: model.CheckPass, Detail: detail}
}

// checkAccounting reports the bounded row counts and file sizes, and warns when
// the write-ahead log has grown past its high-water mark: a WAL that never
// checkpoints is how a workspace runs a disk out of space while every
// individual operation still succeeds. The lease and session counts are the
// retention figures Section 22 asks for -- they are counts, never identities.
func (s *Service) checkAccounting(ctx context.Context) model.DoctorCheck {
	stats, err := s.opts.Store.Stats(ctx)
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
func (s *Service) checkCAS(ctx context.Context, deep bool) model.DoctorCheck {
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
		if stats, err := s.opts.Store.Stats(ctx); err == nil && stats.Blobs > 0 {
			return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckWarn,
				Detail:      "this workspace records " + strconv.FormatInt(stats.Blobs, 10) + " retained objects, none of them in a readable state",
				Code:        model.CodeSourceIntegrity,
				Remediation: "rebuild the cache with codectx index --rebuild"}
		}
		return model.DoctorCheck{Name: checkSourceRetention, State: model.CheckPass, Detail: "no source is retained yet, so there was nothing to verify"}
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
	reader, ok := s.opts.Store.(suppliedIndexReader)
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
			Detail:      "this generation was built with a supplied index at " + joinPaths(unresolved) + ", which matched no file and imported nothing",
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

// joinPaths renders a bounded list of root-relative paths. The list is bounded
// by the number of supplied indexes a generation records, and the detail is
// bounded again by model.MaxDetailBytes in DoctorCheck.Validate.
func joinPaths(paths []string) string {
	out := paths[0]
	for _, p := range paths[1:] {
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
