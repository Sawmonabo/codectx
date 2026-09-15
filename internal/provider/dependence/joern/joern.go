// Package joern is the Joern backend of the dependence provider: the one
// place in the product that knows which code-property-graph engine produces
// the facts, what its command line looks like, how its heap cap is placed and
// what its diagnostics mean. Everything outside this package, the tool lock
// and docs/providers-dependence.md speaks only of "the engine".
//
// The pinned commands are the two noninteractive tools of the engine's own
// distribution. There is no product-owned analysis script, no interpreter
// server and no second export:
//
//	joern-parse  --language <frontend> --max-num-def 40000 <source> --output <graph>
//	joern-export <graph> --repr=all --format=neo4jcsv --out <export>
//
// Both were run against Joern 4.0.627 on all six frontends before they were
// pinned; the runs are recorded in the lane report. The engine is not
// run-to-run deterministic, so those runs establish that each frontend parses
// and exports, not that two runs of one are equal. `--repr=pdg|cdg|ddg` is
// not implemented for CSV or GraphML in this release, and the single `all`
// export already carries every edge family the provider imports.
package joern

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// frontend is the engine's own name for the parser of one language family.
// The mapping lives here because those spellings are the engine's vocabulary,
// not the product's.
var frontend = map[dependence.Family]string{
	dependence.FamilyC:          "c",
	dependence.FamilyGo:         "golang",
	dependence.FamilyJava:       "javasrc",
	dependence.FamilyJavaScript: "jssrc",
	dependence.FamilyPython:     "pythonsrc",
	dependence.FamilyRust:       "rust",
}

// maxNumDef is the per-method definition cap the pinned parse always passes.
// Measured on a 1.05M-line Python tree against the engine default of 4000
// (docs/research/10-round3-empirical.md Section 9a): 23% more parse time, 3%
// more memory, every skipped method removed, and no change to any other fact
// count beyond the engine's own run-to-run variance (Section 9a's CDG and CALL
// counts were equal; the variance band is recorded in
// docs/providers-dependence.md). It is part of the cache key and there is no
// second parse at a higher limit.
const maxNumDef = "40000"

// Output bounds. The child's stdout is the banner and is discarded, so it is
// unbounded: bytes nobody keeps cost no memory, and bounding them once made a
// talkative run a refusal. Its stderr is the classifier's only input and is
// bounded before it is read -- what the bound drops is reported through
// process.Result.OutputTruncated, never by failing the unit.
const (
	maxStdoutBytes int64 = 0
	maxStderrBytes int64 = 8 << 20
	grace                = 10 * time.Second
)

// maxProbeBytes bounds the liveness probe's read of the export's method file.
// The probe only needs to know whether there is a first data row.
const maxProbeBytes = 64 << 10

// Backend is the engine adapter. It holds the payload's identity, the locator
// that can produce its command lines, and the shared process runner; it starts
// nothing else and owns no state beyond the memoized resolution.
type Backend struct {
	locator *Locator
	runner  *process.Runner

	// identity is what the payload *is* -- version, payload digest, runtime
	// digest -- fixed at construction and the same string whether the payload
	// was installed before this process started or during it. Section 11.6
	// keys the graph cache and the provider descriptor on it, so it must not
	// depend on when the bytes arrived.
	identity dependence.Engine

	// once resolves the command lines on first use. The error is memoized with
	// them: a payload that could not be installed is not re-attempted once per
	// unit, which on a cold machine would be one multi-gigabyte fetch attempt
	// per language family rather than one.
	once       sync.Once
	resolved   dependence.Engine
	resolveErr error
}

// New binds the engine's identity to the shared runner without installing
// anything. The payload's version and digest come from the toolchain lock: the
// pinned release has no noninteractive version flag (`joern-parse --version` is
// rejected as an unknown option and `joern --version` opens the interactive
// console), so a version probe would be a guess dressed as a measurement.
//
// A payload the store already holds is described here and used as it was
// resolved. A payload the lock pins but the store does not hold is *not*
// fetched: construction keeps the pinned identity and the first Parse or
// Export resolves the command lines, which is what installs it. Section 11.6's
// "the provider never delays base readiness" is a guarantee about
// OpenWorkspace, and resolving here broke it by roughly two gigabytes on every
// cold open.
func New(ctx context.Context, locator *Locator, runner *process.Runner) (*Backend, error) {
	if locator == nil || runner == nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the dependence backend needs an engine locator and the shared process runner"}
	}
	e, installed, err := locator.LocateInstalled(ctx)
	if err != nil {
		return nil, err
	}
	if e.Digest == "" {
		return nil, &model.Error{Code: model.CodeProviderUnavailable,
			Message: "the analysis engine payload has no pinned identity"}
	}
	b := &Backend{locator: locator, runner: runner, identity: e}
	if installed {
		if len(e.ParseArgv) == 0 || len(e.ExportArgv) == 0 {
			return nil, &model.Error{Code: model.CodeProviderUnavailable,
				Message: "the analysis engine payload is incomplete"}
		}
		b.once.Do(func() { b.resolved = e })
	}
	return b, nil
}

// Engine is the payload's identity. It is complete in Version, Digest and
// RuntimeDigest from construction; its argument arrays are populated only once
// a unit has resolved the payload, because an uninstalled payload has no path
// on this machine to name.
func (b *Backend) Engine() dependence.Engine { return b.identity }

// resolve produces the command lines, installing the payload on the first call
// that needs them.
//
// The digest is re-checked against the identity construction published: the
// descriptor version and every cache key already committed to that string, so a
// payload that resolved to different bytes must fail the unit rather than seal
// facts under an identity that did not produce them. PinnedFingerprint's
// contract makes the two equal for the pinned payload, so this can only fire on
// a lock or store defect.
func (b *Backend) resolve(ctx context.Context) (dependence.Engine, error) {
	b.once.Do(func() {
		e, err := b.locator.Locate(ctx)
		switch {
		case err != nil:
			b.resolveErr = err
		case len(e.ParseArgv) == 0 || len(e.ExportArgv) == 0 || e.Digest == "":
			b.resolveErr = &model.Error{Code: model.CodeProviderUnavailable,
				Message: "the analysis engine payload is incomplete"}
		case e.Digest != b.identity.Digest:
			b.resolveErr = &model.Error{Code: model.CodeToolDigestMismatch,
				Message:     "the installed analysis payload is not the one this workspace's facts are keyed on",
				Remediation: "run `codectx tools verify` and re-install the payload"}
		default:
			b.resolved = e
		}
	})
	if b.resolveErr != nil {
		return dependence.Engine{}, b.resolveErr
	}
	return b.resolved, nil
}

// Argv is the pinned parse argument array for a family, without the paths. It
// is what the cache key folds in, so changing a pinned argument invalidates
// every cached graph instead of silently reusing one built differently.
func (b *Backend) Argv(f dependence.Family) []string {
	return []string{"--language", frontend[f], "--max-num-def", maxNumDef}
}

// NeutralOptions is the frontend's fixed allowlist of semantics-neutral parse
// options. It is empty for every frontend: every option this release offers
// changes results (the Rust helper's --no-sysroot drops type resolution, the
// overlay switches drop whole fact families), so there is nothing that could
// be added to confirm a crash without changing what a success would mean.
func (b *Backend) NeutralOptions(dependence.Family) []string { return nil }

// Parse builds the graph for one unit. The heap cap reaches the frontend
// through JAVA_OPTS, which the launcher forwards to the separate frontend
// process as its own -Xmx; that forwarding was verified against the real
// release by inducing an out-of-memory failure with a cap the unit could not
// fit in.
func (b *Backend) Parse(ctx context.Context, req dependence.ParseRequest) (dependence.Outcome, error) {
	fe, ok := frontend[req.Family]
	if !ok {
		return dependence.Outcome{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the analysis engine has no frontend for this language family"}
	}
	e, err := b.resolve(ctx)
	if err != nil {
		return dependence.Outcome{}, err
	}
	args := append([]string{}, e.ParseArgv[1:]...)
	args = append(args, "--language", fe, "--max-num-def", maxNumDef)
	args = append(args, req.ExtraArgs...)
	args = append(args, req.SourceDir, "--output", req.OutputPath)
	return stepOutcome(b.run(ctx, e, e.ParseArgv[0], args, filepath.Dir(req.OutputPath),
		req.HeapCapBytes, req.ReservationBytes, req.Timeout, req.StallTimeout))
}

// Export writes the graph out as the single Neo4j CSV export the importer
// reads, and probes whether the result is a live graph at all.
func (b *Backend) Export(ctx context.Context, req dependence.ExportRequest) (dependence.ExportOutcome, error) {
	e, err := b.resolve(ctx)
	if err != nil {
		return dependence.ExportOutcome{}, err
	}
	// The engine refuses an output directory that already exists.
	if err := os.RemoveAll(req.OutputDir); err != nil {
		return dependence.ExportOutcome{}, &model.Error{Code: model.CodeInternal,
			Message: "the previous analysis export could not be removed: " + err.Error()}
	}
	args := append([]string{}, e.ExportArgv[1:]...)
	args = append(args, req.GraphPath, "--repr=all", "--format=neo4jcsv", "--out", req.OutputDir)
	outcome, err := stepOutcome(b.run(ctx, e, e.ExportArgv[0], args, filepath.Dir(req.OutputDir),
		req.HeapCapBytes, req.ReservationBytes, req.Timeout, req.StallTimeout))
	if err != nil {
		return dependence.ExportOutcome{}, err
	}
	out := dependence.ExportOutcome{Outcome: outcome}
	out.Live, out.Bytes = probe(req.OutputDir)
	return out, nil
}

// stepOutcome turns the runner's result and error into the neutral outcome, or
// into a real error. A child that exited non-zero or was stopped on its
// deadline is not a runner failure: it is the engine reporting something the
// classifier has to read, and discarding it would turn every classified
// failure class into one untyped "the tool failed". A child that never
// started, a denied executable or a broken output stream stays an error.
func stepOutcome(res process.Result, err error) (dependence.Outcome, error) {
	if err == nil {
		return classify(res), nil
	}
	var typed *model.Error
	if errors.As(err, &typed) && (typed.Code == model.CodeProviderUnavailable || typed.Code == model.CodeProviderTimeout) {
		// Distinguish "ran and failed" from "never started": the latter leaves
		// a zero result, which would classify as an unremarkable success.
		if res.ExitCode != 0 || res.StderrBytes > 0 || res.TimedOut {
			return classify(res), nil
		}
	}
	return dependence.Outcome{}, err
}

// run starts one child through the shared runner. The environment is complete
// rather than merged: internal/process never inherits the parent's, so the
// payload's own variables, the heap cap and the engine's log level are all of
// it. The log level is pinned because the classifier reads the warnings the
// engine emits at WARN, and a host environment that raised the level would
// silence a definition-cap skip into a silent loss of data dependence.
func (b *Backend) run(ctx context.Context, e dependence.Engine, path string, args []string, dir string,
	heapCap, reservation int64, timeout, stallTimeout time.Duration) (process.Result, error) {

	env := append([]string{}, e.Env...)
	env = append(env, "JAVA_OPTS=-Xmx"+strconv.FormatInt(max(heapCap, 1<<20)/(1<<20), 10)+"m", "SL_LOGGING_LEVEL=WARN")
	return b.runner.Run(ctx, process.Spec{Path: path, Args: args, Dir: dir, Env: env,
		Stdout: io.Discard, MaxStdoutBytes: maxStdoutBytes, MaxStderrBytes: maxStderrBytes,
		Timeout: timeout, StallTimeout: stallTimeout, Grace: grace, MemoryReservationBytes: reservation})
}

// probe reports whether the export carries any method rows at all, and its
// byte total. A method file that is missing or empty is the signature of a
// crashed frontend helper the orchestrator hid behind a zero exit: reproduced
// here by parsing a directory with no source of the frontend's language, which
// exited 0, wrote an 8 KB graph and exported one placeholder file node with no
// method node file at all. The probe is a liveness question and stops at the
// first row; counting every method is the importer's job.
func probe(dir string) (live bool, total int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	f, err := os.Open(methodRowsFile(dir))
	if err != nil {
		return false, total
	}
	defer f.Close()
	buf := make([]byte, maxProbeBytes)
	n, _ := io.ReadFull(f, buf)
	return len(bytes.TrimSpace(buf[:n])) > 0, total
}

// methodRowsFile is where the export's method rows land. The export writes one
// header, data and cypher file per node label, so the absence of this one file
// is exactly "the graph has no methods".
func methodRowsFile(dir string) string { return filepath.Join(dir, "nodes_METHOD_data.csv") }
