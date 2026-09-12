package joern

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

const (
	providerID = "joern"
	// adapterVersion changes whenever this package's mapping, projection or
	// argument arrays change; with the profile name it is the static
	// descriptor version. The observed tool versions travel separately, in
	// Detection.ObservedVersion.
	adapterVersion = "1"

	// Capabilities the pinned profile publishes. reads/writes are not among
	// them: Joern has no READ or WRITE edge, and deriving them from operator
	// call shapes is frontend-specific (documented gap).
	CapabilityCalls              = "calls"
	CapabilityControlDependence  = "control_dependence"
	CapabilityDataDependence     = "data_dependence"
	capabilityUnsupportedLabel   = "unsupported_label:"
	capabilityUntrackedUnsupport = "unsupported_labels:untracked"

	// Child output bounds. Tool output is never retained beyond the probe's
	// version line; the byte counts are the recorded metric.
	probeOutputBytes = 64 << 10
	toolOutputBytes  = 64 << 20
	toolGrace        = 10 * time.Second

	// probeMemoryReservation is what the version probe reserves with the
	// runner. A probe starts a JVM that prints a line and exits; charging it
	// the profile's whole analysis budget would serialise detection against
	// real work for no reason. It writes nothing, so it reserves no disk.
	probeMemoryReservation = 256 << 20
	probeDiskReservation   = 0
)

var dependsOn = []string{"filesystem", "treesitter"}

// Provider is the joern provider.Provider. One instance serves a process; it
// is safe for concurrent use.
type Provider struct {
	profile Profile
	runner  *process.Runner
}

// New binds a validated profile to the shared process runner.
func New(profile Profile, runner *process.Runner) (*Provider, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid, Message: "the joern provider needs the shared process runner"}
	}
	return &Provider{profile: profile, runner: runner}, nil
}

// Descriptor is the static contract; it never changes with detection. The
// installed Joern release is still part of this provider's semantics, so
// Detect reports it in Detection.ObservedVersion and the coordinator folds
// that into UnitSpec.ProviderVersion (Section 9.1): a unit built by one Joern
// release is never reused for another.
func (p *Provider) Descriptor() model.ProviderDescriptor {
	return model.ProviderDescriptor{ID: providerID, Version: adapterVersion + "/" + p.profile.Name,
		Capabilities:      []string{CapabilityCalls, CapabilityControlDependence, CapabilityDataDependence},
		DependsOn:         dependsOn,
		InvalidationScope: model.InvalidationWorkspace}
}

// Detect verifies the two approved executables (present, regular,
// executable, checksum when pinned) and runs the version probe of each
// through the runner, matching the constraint. The probe executes code, which
// is why it needs the approved absolute path from configuration and never a
// PATH lookup. A missing tool is unavailable; a checksum or version that is
// not the approved one is CTX_TRUST_REQUIRED; a probe that fails carries the
// runner's code. Detection reads nothing from the repository. The observed
// versions are returned as ObservedVersion, never written back into the
// descriptor.
func (p *Provider) Detect(ctx context.Context, _ workspace.Root, _ workspace.Policy) (provider.Detection, error) {
	if err := os.MkdirAll(p.profile.WorkDir, 0o700); err != nil {
		return provider.Detection{}, internalErr("joern work directory: %v", err)
	}
	var versions []string
	for _, t := range []struct {
		name string
		tool Tool
	}{{"parse", p.profile.Parse}, {"export", p.profile.Export}} {
		if code := checkTool(t.tool); code != "" {
			return provider.Detection{Available: false, DiagnosticCode: code}, nil
		}
		res, err := p.runTool(ctx, runOpts{name: t.name + "-probe", tool: t.tool, args: p.profile.VersionArgs,
			dir: p.profile.WorkDir, maxOutputBytes: probeOutputBytes,
			memoryReservationBytes: probeMemoryReservation, diskReservationBytes: probeDiskReservation})
		if err != nil {
			if ctx.Err() != nil {
				return provider.Detection{}, err
			}
			return provider.Detection{Available: false, DiagnosticCode: provider.CodeOf(err)}, nil
		}
		v := versionOf(res.Stdout)
		if v == "" {
			v = versionOf(res.Stderr)
		}
		if !versionMatches(t.tool.VersionConstraint, v) {
			slog.Warn("joern tool version is not the approved one", "component", "provider.joern", "tool", t.name,
				"constraint", t.tool.VersionConstraint, "observed", truncate(v, 64))
			return provider.Detection{Available: false, DiagnosticCode: model.CodeTrustRequired}, nil
		}
		versions = append(versions, t.name+"="+truncate(v, 64))
	}
	return provider.Detection{Available: true, Capabilities: p.Descriptor().Capabilities,
		ObservedVersion: truncate(strings.Join(versions, ","), model.MaxIdentifierBytes)}, nil
}

// checkTool inspects one executable without running it.
func checkTool(t Tool) string {
	info, err := os.Stat(t.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return model.CodeProviderUnavailable
	}
	if t.Checksum == "" {
		return ""
	}
	f, err := os.Open(t.Path)
	if err != nil {
		return model.CodeProviderUnavailable
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return model.CodeProviderUnavailable
	}
	if hex.EncodeToString(h.Sum(nil)) != t.Checksum {
		return model.CodeTrustRequired
	}
	return ""
}

// IndexUnit produces the workspace unit: materialize the pinned view into a
// private run directory, parse, export twice, import both exports into the
// scratch, project and publish. Every artifact lives under the run directory
// and is removed on every return path; the unit's deadline bounds the whole
// sequence, each tool its own step.
func (p *Provider) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	if req.Unit.ScopeKey != provider.ScopeWorkspace {
		return model.ProviderResult{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the joern provider builds one workspace unit; scope " + truncate(req.Unit.ScopeKey, 64) + " is not supported"}
	}
	ctx, cancel := context.WithTimeout(ctx, p.profile.Timeout)
	defer cancel()

	id, err := model.NewRandomID()
	if err != nil {
		return model.ProviderResult{}, err
	}
	runDir := filepath.Join(p.profile.WorkDir, "runs", "run-"+id[:16])
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return model.ProviderResult{}, internalErr("joern run directory: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(runDir); err != nil {
			slog.Error("joern run directory was not removed", "component", "provider.joern", "run", string(req.Run), "error", err)
		}
	}()

	mat, err := snapshot.Materialize(ctx, req.Content, model.FileSelection{}, snapshot.MaterializeOptions{Dir: filepath.Join(runDir, "src"), MaxBytes: p.profile.Parse.DiskBudgetBytes})
	if err != nil {
		return model.ProviderResult{}, err
	}
	defer mat.Close()

	sc, err := openScratch(ctx, filepath.Join(runDir, "scratch.db"), p.profile.Export.DiskBudgetBytes)
	if err != nil {
		return model.ProviderResult{}, err
	}
	defer sc.close()
	err = req.Content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		return sc.putFile(ctx, fv)
	})
	if err != nil {
		return model.ProviderResult{}, err
	}

	cpg := filepath.Join(runDir, "cpg.bin")
	exportAll, exportPDG := filepath.Join(runDir, "export-all"), filepath.Join(runDir, "export-pdg")
	values := map[string]string{placeholderInputDir: mat.Root(), placeholderCPG: cpg}
	steps := []struct {
		name string
		tool Tool
		args []string
		out  string
	}{
		{"parse", p.profile.Parse, p.profile.ParseArgs, ""},
		{"export-all", p.profile.Export, p.profile.ExportAllArgs, exportAll},
		{"export-pdg", p.profile.Export, p.profile.ExportPDGArgs, exportPDG},
	}
	for _, st := range steps {
		if st.out != "" {
			if err := os.Mkdir(st.out, 0o700); err != nil {
				return model.ProviderResult{}, internalErr("joern export directory: %v", err)
			}
			values[placeholderOutDir] = st.out
		}
		args, err := substitute(st.args, values)
		if err != nil {
			return model.ProviderResult{}, err
		}
		if _, err := p.runTool(ctx, runOpts{name: st.name, tool: st.tool, args: args, dir: runDir, out: io.Discard,
			maxOutputBytes: toolOutputBytes, memoryReservationBytes: st.tool.MemoryBudgetBytes,
			diskReservationBytes: st.tool.DiskBudgetBytes}); err != nil {
			return model.ProviderResult{}, err
		}
		budget, what := p.profile.Parse.DiskBudgetBytes, runDir
		if st.out != "" {
			budget, what = p.profile.Export.DiskBudgetBytes, st.out
		}
		if err := checkDisk(what, budget); err != nil {
			return model.ProviderResult{}, err
		}
	}

	// Decoders buffer at most one record; that buffer is charged before the
	// first byte is decoded.
	if r, ok := sink.(interface {
		Reserve(context.Context, int64) (func(), error)
	}); ok {
		release, err := r.Reserve(ctx, maxRecordBytes)
		if err != nil {
			return model.ProviderResult{}, err
		}
		defer release()
	}
	csvBytes, err := importNeo4jCSV(ctx, sc, exportAll)
	if err != nil {
		return model.ProviderResult{}, err
	}
	xmlBytes, err := importGraphML(ctx, sc, exportPDG)
	if err != nil {
		return model.ProviderResult{}, err
	}
	if err := sc.project(ctx); err != nil {
		return model.ProviderResult{}, err
	}

	em := &emitter{req: req, sink: sink, sc: sc, repo: req.Binding.RepositoryID, materializationRoot: mat.Root(), language: metaLanguage(ctx, sc)}
	for _, step := range []func(context.Context) error{em.locate, em.identify, em.emitNodes, em.emitAliases, em.stageRelations, em.emitRelations} {
		if err := step(ctx); err != nil {
			return model.ProviderResult{}, err
		}
	}

	result := model.ProviderResult{RunID: req.Run, State: model.RunSucceeded, RecordsEmitted: em.records, BytesProcessed: uint64(csvBytes + xmlBytes),
		Capabilities: p.capabilities(sc, em)}
	slog.Info("joern unit imported", "component", "provider.joern", "unit", string(req.Unit.ID), "run", string(req.Run),
		"records", em.records, "export_bytes", result.BytesProcessed, "methods_matched", em.matched, "methods_external", em.external,
		"methods_dropped", em.dropped, "no_strong_key", em.noStrongKey, "occurrences_without_range", em.noRange, "evidence_clipped", em.clipped,
		"dangling_call_edges", sc.dangling, "unknown_labels", sc.unknownN)
	return result, nil
}

// capabilities reports each published capability at workspace scope: fresh
// when every method bound, every occurrence carried a verified range and
// every evidence row fit; otherwise partial with the reason (an occurrence
// whose coordinates did not verify is an unverified source binding like a
// dropped method, not a separate outcome); and one unavailable row per
// unknown label the export carried.
func (p *Provider) capabilities(sc *scratch, em *emitter) []model.CapabilityState {
	state, code := model.CapabilityFresh, ""
	switch {
	case em.dropped > 0 || sc.dangling > 0 || em.noRange > 0:
		state, code = model.CapabilityPartial, model.CodeSourceBindingUnverified
	case em.clipped > 0:
		state, code = model.CapabilityPartial, model.CodeResourceLimit
	}
	var out []model.CapabilityState
	for _, c := range []string{CapabilityCalls, CapabilityControlDependence, CapabilityDataDependence} {
		out = append(out, model.CapabilityState{ProviderID: providerID, Capability: c, Scope: provider.ScopeWorkspace, State: state, DiagnosticCode: code})
	}
	labels := make([]string, 0, len(sc.unknown))
	var tracked uint64
	for l, n := range sc.unknown {
		labels = append(labels, l)
		tracked += n
	}
	sort.Strings(labels)
	for _, l := range labels {
		out = append(out, model.CapabilityState{ProviderID: providerID, Capability: truncate(capabilityUnsupportedLabel+l, model.MaxIdentifierBytes),
			Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable})
	}
	if sc.unknownN > tracked {
		out = append(out, model.CapabilityState{ProviderID: providerID, Capability: capabilityUntrackedUnsupport, Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable})
	}
	return out
}

// runOpts is one invocation of one approved tool. The reservations are
// explicit rather than taken from the tool, because the version probe costs a
// fraction of an analysis run and must not hold the analysis budget.
type runOpts struct {
	name                   string
	tool                   Tool
	args                   []string
	dir                    string
	out                    io.Writer
	maxOutputBytes         int64
	memoryReservationBytes int64
	diskReservationBytes   int64
}

// runTool runs one approved tool through the shared runner with the
// profile's environment allowlist and the caller's reservations, and records
// the process metrics (exit, duration, output bytes, stop reason,
// reservations) as a structured log entry: never the child's output. The
// error is the runner's.
func (p *Provider) runTool(ctx context.Context, o runOpts) (process.Result, error) {
	res, err := p.runner.Run(ctx, process.Spec{
		Path: o.tool.Path, Args: o.args, Dir: o.dir, Env: p.env(),
		Stdout: o.out, Stderr: o.out, MaxStdoutBytes: o.maxOutputBytes, MaxStderrBytes: o.maxOutputBytes,
		Timeout: o.tool.Timeout, Grace: toolGrace,
		MemoryReservationBytes: o.memoryReservationBytes, DiskReservationBytes: o.diskReservationBytes,
	})
	slog.Info("joern tool finished", "component", "provider.joern", "tool", o.name, "exit_code", res.ExitCode, "duration", res.Duration,
		"stdout_bytes", res.StdoutBytes, "stderr_bytes", res.StderrBytes, "timed_out", res.TimedOut, "canceled", res.Canceled,
		"output_truncated", res.OutputTruncated, "signaled", res.Signaled,
		"memory_reservation_bytes", o.memoryReservationBytes, "disk_reservation_bytes", o.diskReservationBytes, "error", provider.CodeOf(err))
	return res, err
}

// env is the child environment: exactly the allowlisted variables the parent
// holds, in sorted order.
func (p *Provider) env() []string {
	out := make([]string, 0, len(p.profile.EnvAllowlist))
	for _, name := range p.profile.EnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// checkDisk bounds what a tool left in dir against its budget.
func checkDisk(dir string, budget int64) error {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if total > budget {
				return resourceLimit("joern output under %s exceeds the profile's %d-byte disk budget", filepath.Base(dir), budget).WithDetail("limit", "disk_budget_bytes")
			}
		}
		return nil
	})
	if err != nil && asTyped(err) == nil {
		return internalErr("joern disk check: %v", err)
	}
	return err
}

// metaLanguage maps Joern's META_DATA language to the snapshot's tag
// vocabulary; it labels external methods, which have no file to take it from.
func metaLanguage(ctx context.Context, sc *scratch) string {
	var lang string
	if err := sc.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'language'`).Scan(&lang); err != nil {
		return ""
	}
	switch strings.ToUpper(lang) {
	case "C", "NEWC":
		return "c"
	case "CSHARP", "CSHARPSRC":
		return "csharp"
	case "GOLANG":
		return "go"
	case "JAVA", "JAVASRC":
		return "java"
	case "JAVASCRIPT", "JSSRC":
		return "javascript"
	case "KOTLIN":
		return "kotlin"
	case "PHP":
		return "php"
	case "PYTHONSRC":
		return "python"
	case "RUBYSRC":
		return "ruby"
	case "SWIFTSRC":
		return "swift"
	}
	return ""
}

// asTyped returns the typed error in err's tree, or nil.
func asTyped(err error) *model.Error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}
