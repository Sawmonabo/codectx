// Package dependence is the Section 11.6 dependence provider: control
// dependence, data dependence through captures and globals, reads, writes and
// fallback call facts for the nine supported languages, produced by a managed
// code-property-graph engine that runs one frontend-native project at a time.
//
// The engine is a backend, not a product surface. Nothing outside the
// dependence/joern subpackage, docs/providers-dependence.md and the tool lock
// names it: the provider id is `dependence`, the configuration table is
// `[providers.dependence]`, the capabilities are control_depends_on,
// data_flows_to, reads, writes and calls, and the evidence details are cdg,
// reaching_def, assignment and call. Swapping the engine is a change to one
// subpackage.
//
// This package owns the parts of Section 11.6 that survive an engine swap:
// which units exist (units.go), what makes a cached graph reusable
// (cache.go), how much memory a unit reserves (govern.go), how a failed or
// degraded run is classified and published (failure.go) and the provider
// contract itself (provider.go). The engine dialect — argv, environment,
// stderr vocabulary, payload digest — lives behind Backend.
package dependence

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence/neo4jcsv"
)

// Engine identifies the resolved analysis payload. Version and Digest are the
// provenance that reaches `status`, `doctor` and the ledger, through
// Detection.ObservedVersion and the descriptor version, and nowhere else.
// ParseArgv and ExportArgv are complete argv prefixes (a launcher plus its
// arguments, for example a managed JDK and `-jar`), so a runtime-dependent
// payload needs no second concept here.
type Engine struct {
	ParseArgv  []string
	ExportArgv []string
	// Env is the complete child environment the payload needs as KEY=VALUE
	// pairs. internal/process never merges the parent environment, so this is
	// the whole of it.
	Env     []string
	Version string
	Digest  string
	// RuntimeDigest is the payload digest of the runtime the engine executes
	// under. Section 11.6 makes it part of the unit's semantic closure: the
	// same engine on a different runtime is not the same analysis.
	RuntimeDigest string
}

// Backend is the engine adapter the provider drives. It is the only place the
// engine's vocabulary exists: it builds the pinned argv, places the heap cap
// in the child environment, runs both steps through the shared process runner
// and translates what it observed into the neutral Outcome this package acts
// on. A backend never decides policy — whether to retry, subdivide or fail is
// failure.go's job.
type Backend interface {
	// Engine is the payload this backend resolved, fixed for its lifetime.
	Engine() Engine
	// Parse builds the graph for one unit. The implementation must name the
	// step's output to the process runner as its progress file: the engine is
	// quiet, so its output is the only thing that tells the stall detector the
	// step is still working.
	Parse(ctx context.Context, req ParseRequest) (Outcome, error)
	// Export writes the graph out for import and reports whether the result is
	// a live graph at all. Like Parse, it must name its output directory as the
	// run's progress file.
	Export(ctx context.Context, req ExportRequest) (ExportOutcome, error)
	// NeutralOptions is the frontend's fixed allowlist of semantics-neutral
	// parse options, used once to confirm a crash reproduces before a unit is
	// subdivided. It is empty for every frontend today (Section 11.6): every
	// known option changes results, so there is nothing honest to retry with.
	NeutralOptions(f Family) []string
	// Argv is the pinned parse argument array for f, without the paths. It is
	// part of the cache key, so a change to the pinned arguments invalidates
	// every cached graph rather than silently reusing one.
	Argv(f Family) []string
}

// ParseRequest is one parse step. SourceDir is a private materialization the
// provider owns and deletes; the backend never reads the live checkout.
type ParseRequest struct {
	SourceDir  string
	OutputPath string
	Family     Family
	// HeapCapBytes is the frontend heap cap. Section 11.6: a cap is lossless
	// where it succeeds, so it bounds scheduling, not results.
	HeapCapBytes int64
	// ExtraArgs is the neutral-option allowlist for a crash-confirmation
	// rerun. It is empty on the first attempt.
	ExtraArgs []string
	// ReservationBytes is what the runner admits this child against, Timeout
	// bounds it (zero meaning no wall clock) and StallTimeout terminates it if
	// it makes no observable progress for that long.
	ReservationBytes int64
	Timeout          time.Duration
	StallTimeout     time.Duration
}

// ExportRequest is one export step. Export scales with the graph rather than
// the source, so it carries its own cap and reservation.
type ExportRequest struct {
	GraphPath        string
	OutputDir        string
	HeapCapBytes     int64
	ReservationBytes int64
	Timeout          time.Duration
	StallTimeout     time.Duration
}

// Outcome is what the backend observed in neutral terms. Class is empty when
// the step produced a usable result; SkippedMethods is orthogonal to it,
// because a definition-cap skip degrades data_flows_to without failing the
// unit.
type Outcome struct {
	Class FailureClass
	// Pass and Exception name the failing analysis pass and the exception
	// class for FailureEngine. They are diagnostic strings from the child, not
	// a vocabulary this package interprets.
	Pass      string
	Exception string
	// SkippedMethods are the fully qualified methods the engine declined to
	// analyse for data flow because they exceed the definition cap, bounded by
	// MaxReportedSkips; SkippedCount is the true total.
	SkippedMethods []string
	SkippedCount   int
	ExitCode       int
	Duration       time.Duration
	// StderrBytes and PeakBytes are the analyzer tree's own metrics, recorded
	// separately from the base index's accounting (Section 22). PeakBytes is
	// the peak of the summed resident memory over the whole analyzer tree,
	// sampled while it ran. PeakUnsampled reports that the platform cannot
	// observe it at all, so the figure is unavailable rather than zero and is
	// published as absent rather than as an observed zero.
	StderrBytes   int64
	PeakBytes     int64
	PeakUnsampled bool
	// StderrTail is the last of what the child wrote to its standard error,
	// bounded to what one error detail carries and cut at a line boundary,
	// with the private paths of this run reduced to their names. It is the
	// evidence a failed unit leaves behind: without it the only record of a
	// crash is its byte count, and a reader has to reproduce the run to learn
	// what the child said.
	StderrTail string
}

// ExportOutcome adds the liveness probe of the produced export. Live reports
// whether the export carries any method at all. Section 11.6 requires every
// result to be validated for non-emptiness before admission: an engine helper
// can crash, be hidden behind a zero exit, and leave a graph with no methods
// in it. The provider, not the backend, decides what a dead export means for a
// given unit — an empty one is honest for a unit with no source.
type ExportOutcome struct {
	Outcome
	Live  bool
	Bytes int64
}

// Family is the set of languages one parse of the engine handles together. It
// is the product's own vocabulary, not the engine's frontend names: the nine
// supported languages collapse onto six families, and the backend maps a
// family to whatever the engine calls that frontend.
type Family string

const (
	// FamilyC covers C and C++. Header resolution spans the whole tree, so it
	// is the one family whose unit is the repository (Section 11.6).
	FamilyC Family = "c"
	// FamilyGo covers Go.
	FamilyGo Family = "go"
	// FamilyJava covers Java.
	FamilyJava Family = "java"
	// FamilyJavaScript covers JavaScript, TypeScript and TSX, which share one
	// project and one frontend.
	FamilyJavaScript Family = "javascript"
	// FamilyPython covers Python.
	FamilyPython Family = "python"
	// FamilyRust covers Rust.
	FamilyRust Family = "rust"
)

// Families is every family in a fixed order, so unit plans and reservations
// are a function of the repository alone.
var Families = []Family{FamilyC, FamilyGo, FamilyJava, FamilyJavaScript, FamilyPython, FamilyRust}

// The import contract is the export reader's own, not a mirror of it. A
// mirror drifts: the declared one carried five of the ten option fields the
// reader requires and six of its thirteen report fields, so no adapter built
// from it could satisfy the reader's own validation. ImportReport is an alias, so
// there is exactly one definition of an import's results and Importer is
// satisfiable by the reader as written; Importer's own options parameter
// names neo4jcsv.Options for the same reason.
//
// The export format is not the engine. The engine's dialect lives behind
// Backend; neo4jcsv reads the bulk-import CSV the export step writes and
// names nothing of the engine, which is why this package may depend on it.
//
// The fact key set has no alias of its own: the delta API this package
// exposes (ImportOptions.PreviousKeys, Report.Keys) names neo4jcsv.KeySet
// directly, and a second spelling one function away from it is the drift this
// comment warns about.

// ImportReport is what one import published and what it refused.
type ImportReport = neo4jcsv.Report

// Importer streams one export into the sink. It is an interface so a test can
// drive the provider's failure paths without an engine; the production
// binding is defaultImporter, which New uses.
type Importer interface {
	Import(ctx context.Context, exportDir string, res provider.Resolver, sink provider.Sink, opts neo4jcsv.Options) (ImportReport, error)
}

// defaultImporter is the production importer: the export reader itself. The
// adaptation is the identity, which is the point — a translating adapter is
// where the two field lists would drift apart again. The export reader's own
// option struct is named here rather than aliased, because ImportOptions is
// the coordinator-facing pair (PreviousKeys, KeysPath) the provider fills the
// rest of.
type defaultImporter struct{}

func (defaultImporter) Import(ctx context.Context, exportDir string, res provider.Resolver,
	sink provider.Sink, opts neo4jcsv.Options) (ImportReport, error) {
	return neo4jcsv.Import(ctx, exportDir, res, sink, opts)
}

func invalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}

func internalErr(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}

func resourceLimit(msg string) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: msg}
}
