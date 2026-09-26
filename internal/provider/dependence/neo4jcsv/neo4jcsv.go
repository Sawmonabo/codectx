// Package neo4jcsv imports one dependence-engine graph export into provider
// facts.
//
// The engine writes a whole-unit export as a directory of Neo4j bulk-import
// CSV: one `nodes_<LABEL>_header.csv` / `nodes_<LABEL>_data.csv` pair per node
// label and one `edges_<TYPE>_header.csv` / `edges_<TYPE>_data.csv` pair per
// edge type, beside `*_cypher.csv` load scripts that are not data. Node ids
// are the engine's integer node identities, renumbered on every run, and
// edges may name an id whose node row has not been read yet, so the import
// stages the whole export in an on-disk scratch database and derives facts by
// ordered query: a fact is a function of the export's content, never of the
// order its files were read. The staging is written the way a bulk load
// writes -- appended in arrival order, sorted once into each order a later
// phase reads -- so its disk traffic is a small constant times the export's
// bytes (ADR-0009).
//
// The import publishes the five capabilities of Section 11.6 —
// control_depends_on, data_flows_to, reads, writes and calls — plus
// may_refer_to for a write whose target shape does not resolve. Precision is
// static_analysis and every evidence range is verified against the pinned
// snapshot bytes before it is published.
//
// Delta. The engine has no incremental mode, so a refreshed unit is a whole
// re-parse and re-export; the storage update is the delta. Every published
// fact carries an id-independent key (KeySet); an import handed the previous
// run's key set publishes only the relations at least one of whose keys
// changed, and only when no key was removed; a removal disables the filter
// and the import re-publishes in full. It reports the keys the previous run
// had and this one does not.
//
// The engine's name appears nowhere in this package: not in an identifier, a
// native key, an evidence detail or an error message.
package neo4jcsv

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// ProviderID is the product-facing provider id these facts belong to. It is
// also the prefix of every native key this package mints, so a second unit of
// the same provider resolves the same declaration to the same identity.
const ProviderID = "dependence"

// Evidence details. They name the graph relation the fact was derived from in
// neutral terms and are the only vocabulary published on an evidence row
// (Section 11.6 and the Task 11 contract).
const (
	detailCDG        = "cdg"
	detailReachDef   = "reaching_def"
	detailReachDefCB = "reaching_def capture"
	detailAssignment = "assignment"
	detailCall       = "call"
	// detailCallSpeculated is a call whose callee the export invented: a
	// method with no definition in the graph, emitted so the site has a
	// target. The edge is published, and says so.
	detailCallSpeculated = "call speculated"
)

// Bounds of one import. They are contract ceilings, not tunables: an export
// that exceeds one is refused as malformed or over budget rather than
// partially admitted.
const (
	// maxFields bounds the columns of one CSV header.
	maxFields = 128
	// maxLabelBytes bounds a node label or edge type.
	maxLabelBytes = 128
	// maxUnknownLabels bounds the distinct unknown labels reported by name;
	// the rest are counted in aggregate.
	maxUnknownLabels = 32
	// maxDataFlowDepth bounds the def-use walk between two anchored nodes.
	maxDataFlowDepth = 8
	// maxOperandDepth bounds the descent into one assignment's written
	// operand and into its read operands.
	maxOperandDepth = 16
	// pageSize bounds every keyset page read from the scratch.
	pageSize = 512
	// maxRangeFileBytes bounds the source held in memory at once: one file's
	// bytes, read through the pinned view, to convert engine line and column
	// coordinates into verified byte ranges. A larger file keeps its facts,
	// bound to the file but without a range (Section 9.3: absent, not
	// zero-filled).
	maxRangeFileBytes = 4 << 20
	// engineCodeChars is where the engine truncates a node's CODE attribute.
	// Measured on 4.0.627: 583 fields of a 50 MB export sit at exactly this
	// length and none exceeds it. A CODE this long is a prefix of the real
	// source, so it is never used as a byte range.
	engineCodeChars = 1000
)

// Options are the inputs of one import beyond the export itself.
//
// Language, UnitScopeKey, ProjectRoot, UnitRoot, Limits and PreviousKeys
// describe the export and where it came from. Repository, Unit, Run and
// Content are not decorative: an Evidence
// row is invalid without the unit, provider version and origin run, a
// RelationID cannot be derived without the repository, and a byte range
// cannot be verified without the pinned bytes. ScratchDir is where the
// on-disk staging database lives; the file is reused by every later import
// through the same slot and is never removed by an import.
type Options struct {
	// Language is the source language recorded on fileless nodes, normally
	// the export's META_DATA LANGUAGE. A located node takes its language from
	// the snapshot file instead.
	Language string
	// UnitScopeKey is the unit's scope (`pkg:<frontend-native project key>`,
	// or the workspace scope for C/C++). It is the alias scope of every
	// fileless identity this import publishes.
	UnitScopeKey string
	// ProjectRoot is the absolute directory the engine parsed. An absolute
	// FILENAME is accepted only under it.
	ProjectRoot string
	// UnitRoot is that same directory as a root-relative snapshot path, "" for
	// a unit that is the whole repository. The export's paths are relative to
	// the directory the engine was given, not to the repository, so this is
	// the prefix that turns one into a snapshot path. Without it every fact of
	// a unit below the repository root binds to a path the snapshot does not
	// have, and the unit seals with nothing in it.
	UnitRoot string
	// Limits are the sink's per-batch bounds. MaxRecordBytes also bounds one
	// CSV record before the decoder can allocate it.
	Limits provider.Limits
	// MaxEvidencePerFact is the effective per-fact evidence clip: the operator's
	// index.max_evidence_per_fact, or the model's record ceiling when they set
	// none. Zero selects the ceiling. Occurrences past it are counted and
	// disclosed, never dropped in silence.
	MaxEvidencePerFact int
	// MaxStagedRows is the user's `providers.dependence.max_staged_rows`: how
	// many rows the caller wants one import to stage. 0, the default, is
	// unlimited. It is a reporting threshold and never a refusal -- crossing
	// it stages, publishes and reports everything (Report.OverStagedRows).
	MaxStagedRows config.Limit
	// MaxDerivedRows is the user's `providers.dependence.max_derived_rows`:
	// how many relation occurrences the caller wants one import to project
	// from its staged rows. 0, the default, is unlimited. Like MaxStagedRows
	// it is a reporting threshold and never a refusal -- crossing it projects,
	// publishes and reports everything (Report.OverDerivedRows).
	MaxDerivedRows config.Limit
	// MaxExportFiles is the user's `providers.dependence.max_export_files`:
	// how many entries one export directory may hold. Unlimited by default --
	// the file count is a property of the export's label vocabulary, not of
	// the repository, and the directory is read one entry at a time -- so only
	// a user-set bound refuses an import, and it says so.
	MaxExportFiles config.Limit
	// PreviousKeys is the key set the previous sealed run of this unit
	// published. The zero value is the absent set and means a full import.
	PreviousKeys KeySet

	// Repository identifies the repository RelationIDs are derived in.
	Repository model.RepositoryID
	// Unit is the immutable unit storage opened for this run.
	Unit model.UnitSpec
	// Run is the producing provider run every evidence row names.
	Run model.ProviderRunID
	// Content is the pinned snapshot view every source byte is read from.
	Content model.SnapshotView
	// ScratchDir is the private directory the staging databases live in. An
	// import takes a slot there, empties the slot's file and gives it back;
	// the file is created once and written over by every later import, and
	// only the provider's sweep of a dead run removes it. Empty means the
	// process temp directory.
	ScratchDir string
	// StagingCacheKiB is the staging database's page cache, the user's
	// `providers.dependence.staging_cache_kib`. It bounds the memory one
	// import holds for its staging and is the buffer the engine sorts in; 0
	// selects the default.
	StagingCacheKiB int
	// KeysPath is where the fresh key set is written. Empty means a file
	// beside the staging database, written over by the next import through
	// that slot; a caller that wants to keep the key set for the next refresh
	// must name a path.
	KeysPath string
	// OnPhase, when set, is called as each phase of the import completes,
	// with the phase's name: the provider logs them with their durations, and
	// a measurement reads its counters between them.
	OnPhase func(phase string)
}

// Import phases, in order, as OnPhase reports them.
const (
	PhaseFilesStaged      = "files staged"
	PhaseExportStaged     = "export staged"
	PhaseProjected        = "entities and edges projected"
	PhaseReadsWrites      = "reads and writes derived"
	PhaseLocated          = "located"
	PhaseIdentified       = "identified"
	PhaseRelationsStaged  = "relations staged"
	PhaseKeysSaved        = "keys saved"
	PhaseNodesEmitted     = "nodes emitted"
	PhaseAliasesEmitted   = "aliases emitted"
	PhaseRelationsEmitted = "relations emitted"
)

func (o Options) phase(name string) {
	if o.OnPhase != nil {
		o.OnPhase(name)
	}
}

func (o Options) validate() error {
	if err := o.Limits.Validate(); err != nil {
		return err
	}
	if o.UnitScopeKey == "" || len(o.UnitScopeKey) > model.MaxScopeKeyBytes {
		return argumentInvalid("import options carry no usable unit scope key")
	}
	if o.ProjectRoot == "" || !filepath.IsAbs(o.ProjectRoot) {
		return argumentInvalid("import options carry no absolute project root")
	}
	if u := o.UnitRoot; u != "" {
		if c := path.Clean(u); c != u || path.IsAbs(u) || c == "." || c == ".." || strings.HasPrefix(c, "../") {
			return argumentInvalid("import options carry a unit root that is not a clean root-relative path")
		}
	}
	if o.Content == nil {
		return argumentInvalid("import options carry no pinned snapshot view")
	}
	if err := o.Unit.Validate(); err != nil {
		return err
	}
	if o.Repository == "" || o.Run == "" {
		return argumentInvalid("import options carry no repository or run identity")
	}
	return nil
}

// Report is what one import tells its caller. Nodes, Relations and Aliases
// count the records handed to the sink; Changed, Unchanged and Removed
// describe the delta against Options.PreviousKeys and are the whole key set
// against the absent previous set.
type Report struct {
	Nodes, Relations, Aliases int
	// UnknownLabels counts the rows of every export label this import does
	// not map, by label, bounded at maxUnknownLabels distinct names.
	UnknownLabels map[string]int
	// ExternalMethods counts the declarations the export marked external:
	// callees that belong to another unit, kept and aliased by full name.
	ExternalMethods             int
	Changed, Unchanged, Removed int
	// Keys is the fact key set this import published.
	Keys KeySet

	// DroppedMethods counts declarations refused because their path is not in
	// the pinned snapshot or an attribute exceeds its bound. The synthetic
	// per-package initializer of the Go frontend is the expected member of
	// this count: it names a package, not a file.
	DroppedMethods int
	// UnlocatedFacts counts published facts whose coordinates did not verify
	// against the pinned bytes and therefore carry no range.
	UnlocatedFacts int
	// UnresolvedWrites counts assignment targets whose shape did not resolve
	// to a declaration and were published as may_refer_to instead of writes.
	UnresolvedWrites int
	// ClippedEvidence counts occurrence rows dropped because the fact already
	// carries model.MaxEvidencePerFact of them. The fact is still published;
	// the count is what keeps that truncation from being silent.
	ClippedEvidence int
	// TruncatedFields counts, by field name, the descriptive storage values
	// this import cut to their model ceiling before writing them: name,
	// qualified_name, signature and evidence detail. The model accepts an
	// oversize storage field, so the cut is this producer's to make; the count
	// is what keeps it from being silent. Identity fields are never cut — an
	// entity whose native key or scope key is over its ceiling is counted in
	// DroppedMethods instead.
	TruncatedFields map[string]int
	// UnknownRows is the total rows of unmapped labels, including the labels
	// past the maxUnknownLabels distinct names UnknownLabels can name.
	UnknownRows uint64
	// IgnoredFiles counts the export entries this import did not read: the
	// _cypher.csv load scripts and any name that is not a node or edge part.
	// An export whose data files all fell here would publish nothing, so the
	// count is what separates an empty export from an unread one.
	IgnoredFiles int
	// BytesRead is the export bytes this import consumed.
	BytesRead uint64
	// StagedRows is how many rows this import staged, and OverStagedRows
	// whether that crossed a user-set Options.MaxStagedRows. Crossing it
	// stages and publishes everything anyway; the flag is what keeps the
	// crossing from being silent.
	StagedRows     int64
	OverStagedRows bool
	// DerivedRows is how many relation occurrences this import projected, and
	// OverDerivedRows whether that crossed a user-set Options.MaxDerivedRows.
	// The projection is neither truncated nor refused when it does; the flag
	// is what keeps the crossing from being silent.
	DerivedRows     int64
	OverDerivedRows bool
}

// Import streams one export directory into sink and reports what it
// published. The sink receives records in reference order: every node fact
// before the aliases and relations that name its identity.
func Import(ctx context.Context, exportDir string, res provider.Resolver, sink provider.Sink, opts Options) (Report, error) {
	rep := Report{UnknownLabels: map[string]int{}, TruncatedFields: map[string]int{}}
	if err := opts.validate(); err != nil {
		return rep, err
	}
	if res == nil || sink == nil {
		return rep, argumentInvalid("import needs both a resolver and a sink")
	}
	sc, err := openScratch(ctx, opts.ScratchDir, opts.StagingCacheKiB, opts.MaxStagedRows)
	if err != nil {
		return rep, err
	}
	defer sc.close()

	if opts.MaxEvidencePerFact <= 0 {
		opts.MaxEvidencePerFact = model.MaxEvidencePerFact
	}
	e := &emitter{sc: sc, res: res, sink: sink, opts: opts, language: opts.Language}
	if err := e.stageFiles(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseFilesStaged)
	n, err := importExport(ctx, sc, exportDir, opts.Limits.MaxRecordBytes, opts.MaxExportFiles)
	rep.BytesRead = uint64(n)
	if err != nil {
		return rep, err
	}
	opts.phase(PhaseExportStaged)
	if err := sc.order(ctx); err != nil {
		return rep, err
	}
	if err := sc.project(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseProjected)
	if err := e.deriveReadsWrites(ctx); err != nil {
		return rep, err
	}
	if err := sc.occurrences(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseReadsWrites)
	if err := e.locate(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseLocated)
	if err := e.identify(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseIdentified)
	if err := e.stageRelations(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseRelationsStaged)
	keysPath := opts.KeysPath
	if keysPath == "" {
		keysPath = sc.defaultKeys
	}
	fresh, err := sc.saveKeys(ctx, keysPath)
	if err != nil {
		return rep, err
	}
	delta, err := sc.markDelta(ctx, fresh, opts.PreviousKeys)
	if err != nil {
		return rep, err
	}
	opts.phase(PhaseKeysSaved)
	if err := e.emitNodes(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseNodesEmitted)
	if err := e.emitAliases(ctx); err != nil {
		return rep, err
	}
	opts.phase(PhaseAliasesEmitted)
	if err := e.emitRelations(ctx, deltaFilter(opts.PreviousKeys, delta)); err != nil {
		return rep, err
	}
	opts.phase(PhaseRelationsEmitted)

	rep.Nodes, rep.Relations, rep.Aliases = e.nodes, e.relations, e.aliases
	rep.ExternalMethods, rep.DroppedMethods = e.external, e.dropped
	rep.UnlocatedFacts, rep.UnresolvedWrites = e.noRange, e.unresolved
	rep.ClippedEvidence, rep.UnknownRows, rep.IgnoredFiles = e.clipped, sc.unknownN, sc.ignoredFiles
	rep.StagedRows, rep.OverStagedRows = sc.rows, sc.overRows
	rep.DerivedRows = sc.derivedRows
	rep.OverDerivedRows = opts.MaxDerivedRows.Exceeded(sc.derivedRows)
	rep.Changed, rep.Unchanged, rep.Removed = delta.Changed, delta.Unchanged, delta.Removed
	rep.Keys = fresh
	for label, n := range sc.unknown {
		rep.UnknownLabels[label] = int(n)
	}
	for field, n := range e.truncatedFields {
		rep.TruncatedFields[field] = n
	}
	return rep, nil
}

// deltaFilter reports whether this import may emit only the relations whose
// keys the previous run did not publish.
//
// It may not when the previous run's key set lost a key. A removed key is a
// digest: it cannot be mapped back to the relation it backed, and the relation
// may still exist with its remaining occurrences — one of two identical calls
// deleted, say, leaves the edge with an unchanged key and no changed one.
// Storage drops that edge's previous row, because one of the keys it was
// stored under is replaced, and a filtered emit would never republish it: the
// edge would vanish from a delta-built unit that a full build still holds.
// Emitting every relation is the sound over-approximation — an edge published
// with its whole evidence list is byte-identical to what a full import writes
// — and it costs import work, never correctness. Node facts are unaffected;
// they are emitted whole on every run.
func deltaFilter(prev KeySet, d Delta) bool { return !prev.Empty() && d.Removed == 0 }

func internalErr(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

func outputInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderOutputInvalid, Message: fmt.Sprintf(format, args...)}
}

func argumentInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}
