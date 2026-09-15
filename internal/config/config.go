// Package config resolves the strict layered configuration of Section 20:
// built-in defaults, then the user configuration file, then the small permitted
// subset of the project's .codectx.toml. It owns the trust split between the
// two files and the semantic fingerprints derived from the resolved values.
//
// Nothing in this package executes anything. Trusted analyzer profiles are
// parsed and validated here; resolving, checksumming and running an executable
// belongs to the provider that owns it, through internal/process.
package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// SchemaVersion is the configuration file version this build understands. It is
// the `version` key of Section 20.1 and is not the storage schema version.
const SchemaVersion = 1

// MaxSourceWireCeilingBytes is the hard source-response ceiling of Section
// 16.2. Source responses have their own wire budget that allows encoding and
// envelope expansion around the raw chunk; configuration may lower that budget
// but never raise it past this value.
const MaxSourceWireCeilingBytes = 7340032

// Enablement is the tri-state of an optional external analyzer. `auto` means
// "use an already approved profile when one is available", never "execute
// whatever is found on PATH" (Section 20.2).
type Enablement uint8

const (
	Disabled Enablement = iota
	Enabled
	Auto
)

// String returns the wire spelling, which is also the fingerprint component.
func (e Enablement) String() string {
	switch e {
	case Enabled:
		return "true"
	case Auto:
		return "auto"
	default:
		return "false"
	}
}

// UnmarshalTOML accepts the two spellings Section 20.1 uses for this key, a
// boolean and the string "auto", and rejects every other value rather than
// silently defaulting. TOML booleans never reach a TextUnmarshaler, so this
// type implements toml.Unmarshaler instead.
func (e *Enablement) UnmarshalTOML(v any) error {
	switch value := v.(type) {
	case bool:
		if value {
			*e = Enabled
		} else {
			*e = Disabled
		}
		return nil
	case string:
		if value == "auto" {
			*e = Auto
			return nil
		}
	}
	return fmt.Errorf("want true, false or \"auto\", got %v", v)
}

// Duration is a TOML duration written in Go's syntax ("250ms", "45m"). A
// duration is never negative. Almost every duration must also be positive; the
// exceptions are the two analysis timeouts (providers.scip.timeout and
// providers.dependence.timeout), where 0 means no wall-clock limit at all,
// because a deadline that fails an analysis unit is a scale refusal wearing a
// deadline's clothes. A hung subprocess is caught by the stall timeouts
// instead, which detect absence of progress rather than duration of work.
type Duration time.Duration

// UnmarshalText parses the Go duration spelling used throughout Section 20.1.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std converts to the standard library type.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String returns the canonical spelling used as a fingerprint component.
func (d Duration) String() string { return time.Duration(d).String() }

// Config is the complete resolved configuration. Every field is effective
// policy: nothing here is "unset", and no value means unlimited.
type Config struct {
	Version   int       `toml:"version"`
	Workspace Workspace `toml:"workspace"`
	Index     Index     `toml:"index"`
	Resources Resources `toml:"resources"`
	Storage   Storage   `toml:"storage"`
	Retention Retention `toml:"retention"`
	Tools     Tools     `toml:"tools"`
	Providers Providers `toml:"providers"`
	Context   Context   `toml:"context"`
	Coverage  Coverage  `toml:"coverage"`
	Workflow  Workflow  `toml:"workflow"`
	MCP       MCP       `toml:"mcp"`
}

// Workspace is the source-eligibility policy. Every field here feeds
// SourcePolicyHash except the two analysis admission limits, which feed
// AnalysisConfigHash instead (Section 20.2).
type Workspace struct {
	FollowSymlinks     bool  `toml:"follow_symlinks"`
	IncludeUntracked   bool  `toml:"include_untracked"`
	IndexGenerated     bool  `toml:"index_generated"`
	IndexVendor        bool  `toml:"index_vendor"`
	MaxFiles           Limit `toml:"max_files"`
	MaxParseFileBytes  Limit `toml:"max_parse_file_bytes"`
	MaxSearchFileBytes Limit `toml:"max_search_file_bytes"`
	// MaxDirEntries bounds how many children one directory may hold and
	// MaxDepth how deeply directories may nest. Both unlimited by default:
	// one generated directory or one deep vendored tree must never refuse a
	// capture. A user-set value that is exceeded is reported through the
	// traversal's skip sink -- which the capture records in its notes -- and
	// the walk continues to completion. They are the operator's escape hatch
	// over a directory so wide that sorting it spills to disk.
	MaxDirEntries Limit `toml:"max_dir_entries"`
	MaxDepth      Limit `toml:"max_depth"`
	// MaxIgnoredRoots bounds the ignored-root set the shared traversal policy
	// holds. Unlimited by default: the set is one entry per OUTERMOST ignored
	// path of the worktree and every lookup is a random-access ancestor probe,
	// so it is resident by construction and small on any real checkout. A
	// user-set value that is exceeded does not refuse the workspace: the
	// policy degrades to the base policy -- the ignored trees are walked
	// rather than excluded -- and the degradation is logged.
	MaxIgnoredRoots Limit `toml:"max_ignored_roots"`
}

// Index is operational scheduling policy: none of it is a semantic input.
type Index struct {
	// Workers is the only setting where 0 is meaningful: it selects the count
	// from available CPUs and memory reservations, which is still a finite
	// bound and never "unlimited".
	Workers           int      `toml:"workers"`
	MaxParserWorkers  int      `toml:"max_parser_workers"`
	BatchRecords      int      `toml:"batch_records"`
	BatchBytes        int64    `toml:"batch_bytes"`
	QueueBytes        int64    `toml:"queue_bytes"`
	WatchPendingPaths int      `toml:"watch_pending_paths"`
	WatchPendingBytes int64    `toml:"watch_pending_bytes"`
	WatchDebounce     Duration `toml:"watch_debounce"`
	ReconcileInterval Duration `toml:"reconcile_interval"`
	// RetainRefs is retention by ref, not by snapshot count (Section 12.4):
	// the results of the last N distinct refs the user actually indexed stay
	// on disk, so switching A -> B -> C -> A finds A's units and reuses them
	// without a run. It keeps a finite default because a generation is
	// reconstructible by re-indexing, so evicting one is lifecycle retention
	// and not a dropped row; 0 retains every ref, matching MaxRetainedBytes.
	RetainRefs Limit `toml:"retain_refs"`
	// MaxRetainedBytes is the second setting where 0 is meaningful: the
	// retained store has no default size limit, so 0 leaves retention governed
	// by RetainRefs alone. A user-set value evicts least-recently-used refs
	// first and never the active one.
	MaxRetainedBytes Limit `toml:"max_retained_bytes"`
	// WatchMaxDirectories is how many directories one watcher may watch.
	// Unlimited by default: a repository's directory count is a property of
	// the repository, and the host's own notification limit is the real
	// ceiling -- reaching it is refused by the host and reported as incomplete
	// coverage. A user-set value stops the watch set at that many directories
	// and reports coverage incomplete with the reason, never silently.
	WatchMaxDirectories Limit `toml:"watch_max_directories"`
	// CaptureMaxRetries bounds how many validation passes of a snapshot
	// capture may find the worktree changed under them before the capture is
	// declared unstable. 0 -- the default -- is unlimited, and the capture is
	// then ended by CaptureRetryDeadline alone: a retry count that refuses a
	// busy monorepo would be a scale refusal, whereas a deadline ends a
	// worktree that genuinely never settles. Attempts are reported either way.
	CaptureMaxRetries Limit `toml:"capture_max_retries"`
	// MaxEvidencePerFact is how many evidence occurrences one node or relation
	// fact may carry in a sealed unit. Unlimited (0) by default: a fact keeps
	// every occurrence the providers found, up to the record ceiling
	// model.MaxEvidencePerFact, which is a wire and record bound rather than a
	// setting. A user-set value clips a fact at that many occurrences, and the
	// cut is reported on the unit's capability detail, never silent. It is a
	// semantic input -- a unit sealed under a clip concludes less from the same
	// bytes -- so it is part of AnalysisConfigHash.
	MaxEvidencePerFact Limit `toml:"max_evidence_per_fact"`
	// CaptureRetryDeadline bounds the whole validated capture. It is finite by
	// design and not a size bound: with CaptureMaxRetries unlimited it is the
	// only thing that ends a capture of a worktree that is always changing.
	CaptureRetryDeadline Duration `toml:"capture_retry_deadline"`
}

// Resources is the memory, concurrency, disk and response policy.
type Resources struct {
	BaseMemoryBudgetBytes     int64    `toml:"base_memory_budget_bytes"`
	QueryMemoryBytes          int64    `toml:"query_memory_bytes"`
	CacheBytes                int64    `toml:"cache_bytes"`
	MaxConcurrentQueries      int      `toml:"max_concurrent_queries"`
	MaxConcurrentGraphQueries int      `toml:"max_concurrent_graph_queries"`
	MaxConcurrentHeavy        int      `toml:"max_concurrent_heavy_analyzers"`
	MaxTempBytes              int64    `toml:"max_temp_bytes"`
	MinFreeDiskBytes          int64    `toml:"min_free_disk_bytes"`
	MaxMetadataResponseBytes  int64    `toml:"max_metadata_response_bytes"`
	MaxSourceResponseBytes    int64    `toml:"max_source_response_bytes"`
	QueryTimeout              Duration `toml:"query_timeout"`
	MaxQueryTextBytes         int      `toml:"max_query_text_bytes"`
	MaxQueryTerms             Limit    `toml:"max_query_terms"`
	MaxPageItems              int      `toml:"max_page_items"`
	MaxProviderRecordBytes    Limit    `toml:"max_provider_record_bytes"`
}

// The two accepted spellings of storage.synchronous. They are the whole set:
// SQLite's OFF and EXTRA are deliberately not offered. OFF gives up the
// corruption-free guarantee WAL+NORMAL provides, and EXTRA only adds a
// directory fsync for rollback-journal transactions the store never runs.
const (
	SynchronousNormal = "normal"
	SynchronousFull   = "full"
)

// Storage is the SQLite and data-directory policy.
type Storage struct {
	// DataDir is always absolute after Load: the empty default is resolved to
	// the user-private directory DefaultDataDir returns, so no consumer derives
	// it a second time. Load does not create it; the storage owner creates it
	// with user-private permissions.
	DataDir                string   `toml:"data_dir"`
	BusyTimeout            Duration `toml:"busy_timeout"`
	ReadConnections        int      `toml:"read_connections"`
	WriterCacheKiB         int      `toml:"writer_cache_kib"`
	ReaderCacheKiB         int      `toml:"reader_cache_kib"`
	WALHighWaterBytes      int64    `toml:"wal_high_water_bytes"`
	ClosedSessionRetention Duration `toml:"closed_session_retention"`
	QueryCursorTTL         Duration `toml:"query_cursor_ttl"`
	// Synchronous is the SQLite synchronous mode of the single writer
	// connection: "normal" (the default) or "full". The store is rebuildable
	// derived data and the database runs in WAL mode, where NORMAL is
	// corruption-free -- a power loss can only roll back the most recent
	// commits -- so the per-commit WAL fsync FULL costs is not bought back by
	// any promise codectx makes. Set "full" to keep it anyway. Readers are
	// unaffected: they are query_only and never write. See docs/adr/ADR-0004.
	Synchronous string `toml:"synchronous"`
}

// Retention configures the process-level collector (internal/retention): the
// scheduled reclaim of sessions, spools, snapshots and the tool store, and the
// Section 10.4 blob grace protocol it owns.
type Retention struct {
	// BlobGrace is how long a blob that nothing references waits, once it has
	// been trashed, before the collector rechecks reachability and deletes its
	// row and its content-addressed object.
	//
	// It is the protocol's safety margin, not a tuning knob for throughput: the
	// window is what protects a reader that pinned a generation in the instant
	// the manifest naming a blob went away. Shortening it narrows that
	// protection; lengthening it only delays reclaim. It must be positive --
	// a zero window would delete an object in the same pass that trashed it.
	BlobGrace Duration `toml:"blob_grace"`
}

// Providers groups the provider families of Section 20.1.
type Providers struct {
	TreeSitter TreeSitter `toml:"tree_sitter"`
	SCIP       SCIP       `toml:"scip"`
	LSP        LSP        `toml:"lsp"`
	Dependence Dependence `toml:"dependence"`
	Manifest   Manifest   `toml:"manifest"`
}

// Manifest configures the build-metadata and documentation provider of
// Section 11.2. All four bounds are how much of one manifest the user wants
// indexed -- two over the lists it declares, two over the parse that reads
// them -- and none has a default, because how much a manifest declares is a
// property of the repository. A manifest is parsed as a whole file whose size
// workspace.max_parse_file_bytes already bounds, so leaving all four
// unlimited costs one file's heap, never the repository's.
type Manifest struct {
	// MaxDependencies is how many dependencies the user wants one manifest to
	// declare. Unlimited by default; a user-set value that is crossed cuts
	// the list and is reported on the file's capability row with the count
	// that crossed it and this bound.
	MaxDependencies Limit `toml:"max_dependencies"`
	// MaxEntries is the same contract for a manifest's modules, replaced and
	// excluded modules, workspace members, properties, and a document's
	// headings and links.
	MaxEntries Limit `toml:"max_entries"`
	// MaxTOMLLines is how many lines of one TOML manifest the user wants the
	// evidence-range line scan to place. Unlimited by default. A user-set
	// value that is crossed stops the scan there: the facts whose lines the
	// scan did place keep their exact ranges and only the facts past the
	// bound carry evidence without one, and the cut is reported with the
	// line count that crossed it.
	MaxTOMLLines Limit `toml:"max_toml_lines"`
	// MaxXMLElements is the same contract for the token stream of one POM:
	// unlimited by default, and a user-set value that is crossed stops the
	// parse there and reports the cut with the element count, publishing
	// what parsed cleanly before it rather than calling the file malformed.
	MaxXMLElements Limit `toml:"max_xml_elements"`
}

// TreeSitter configures the bundled structural provider. It runs as a private
// subcommand of this same binary with bundled grammars, so `enabled` is a plain
// boolean: there is no external executable to approve.
type TreeSitter struct {
	Enabled       bool     `toml:"enabled"`
	Languages     []string `toml:"languages"`
	WorkerIdleTTL Duration `toml:"worker_idle_ttl"`
	// MaxCalleeReferences is how many distinct cross-file callee names the
	// user wants one file to mint nodes for. Unlimited by default: a
	// generated file names what it names, and the count is bounded by the
	// file, whose size workspace.max_parse_file_bytes already bounds, so an
	// unlimited bound costs one file's heap and never the repository's. A
	// user-set value that is crossed counts every call past it into the
	// file's dropped count and reports the file partial.
	MaxCalleeReferences Limit `toml:"max_callee_references"`
	// MaxRecordsPerFile is how many declarations, imports or references --
	// each counted separately -- the user wants one file to yield. Unlimited
	// by default: it replaces three hard-coded worker ceilings of 20000, 4000
	// and 60000, and a generated or vendored file that crosses one is a
	// property of the repository rather than a fault. What a file yields is
	// bounded by its own size, which workspace.max_parse_file_bytes already
	// bounds, so an unlimited value costs one file's heap and never the
	// repository's. A user-set value that is crossed reports the file partial
	// and its structural coverage truncated; the worker that extracts and the
	// parent that reads its frames apply the same number.
	MaxRecordsPerFile Limit `toml:"max_records_per_file"`
}

// SCIP configures the external index importer.
type SCIP struct {
	Enabled Enablement `toml:"enabled"`
	// Timeout bounds one whole import. 0, the default, means no wall-clock
	// limit: a monorepo's import is slow, not broken.
	Timeout Duration `toml:"timeout"`
	// StallTimeout is a hang detector, not a size limit. It is how long the
	// runner tolerates a subprocess making no progress at all -- no stdout, no
	// stderr, no CPU, no growth of its output file -- before failing the unit
	// with reason `stalled` and reporting it. It is finite by default because
	// a wedged process makes no progress no matter how large the repository.
	StallTimeout Duration `toml:"stall_timeout"`
	// MaxIndexBytes is how large an index the user wants one unit to import.
	// Unlimited by default: an index is streamed record by record and never
	// held whole, so its size bounds nothing in heap. A user-set value never
	// refuses the import -- exceeding it is reported on the unit's capability
	// rows with the size seen and this bound.
	MaxIndexBytes Limit `toml:"max_index_bytes"`
	// MaxManifestBytes is how large a supplied input-hash manifest, or a
	// compilation database the C/C++ profile normalizes, the user wants read.
	// Unlimited by default. A manifest is scanned line by line, so the bound
	// is a reporting threshold; a compilation database is parsed whole, and a
	// user-set value there skips the normalization and reports the skip.
	MaxManifestBytes Limit `toml:"max_manifest_bytes"`
	// MaxDocuments is how many documents the user wants one index to describe.
	// Unlimited by default: documents stream past a bounded record buffer and
	// spool to disk. A user-set value never drops a document and never fails
	// the unit -- exceeding it is reported with the count and this bound.
	MaxDocuments Limit `toml:"max_documents"`
	// MaxOccurrencesPerDocument is how many occurrences the user wants one
	// document to carry. Unlimited by default and reported the same way: the
	// largest per-document count seen is published beside this bound.
	MaxOccurrencesPerDocument Limit `toml:"max_occurrences_per_document"`
	// MaxSpoolBytes is how many bytes the user wants one import to spool to
	// its private scratch database. Unlimited by default: the spool is an
	// on-disk, keyset-paged database, so the figure bounds disk rather than
	// heap and is already covered by resources.max_temp_bytes. A user-set
	// value never fails the import -- exceeding it is reported.
	MaxSpoolBytes Limit `toml:"max_spool_bytes"`
	// MaxSourceFileBytes is how large a document's source the user allows to
	// be held whole while its positions are converted. Unlimited by default,
	// as workspace.max_parse_file_bytes is. This one bounds heap, so a
	// user-set value does skip the document -- and every skip is reported on
	// the capability row with this bound named.
	MaxSourceFileBytes Limit `toml:"max_source_file_bytes"`
	// MaxMaterializeBytes is how much content the user allows a profile's
	// private materialization to copy. Unlimited by default; a user-set value
	// leaves files out of the copy, which the materializer reports, and is
	// published on the capability row with this bound named.
	MaxMaterializeBytes Limit `toml:"max_materialize_bytes"`
}

// LSP configures the snapshot-qualified working-tree overlay.
type LSP struct {
	Enabled                Enablement `toml:"enabled"`
	RequestTimeout         Duration   `toml:"request_timeout"`
	MaxServers             int        `toml:"max_servers"`
	MaxOutstandingRequests int        `toml:"max_outstanding_requests"`
	IdleTTL                Duration   `toml:"idle_ttl"`
	// MaxOverlayBytes bounds, separately, the materialized snapshot, the
	// pinned bytes cached for coordinate conversion, and the bytes sent to and
	// received from a server over its lifetime.
	MaxOverlayBytes Limit `toml:"max_overlay_bytes"`
}

// Dependence configures the control-dependence, data-dependence and fallback
// call provider of Section 11.6. Everything here is scheduling and admission
// policy: which backend produces the facts is a tool-lock entry the product
// owns and never a configuration value, so no key names one.
type Dependence struct {
	// Enabled defaults to auto: dependence units run as low-priority
	// background work once the base generation is active, and a query for a
	// dependence fact promotes its units and is answered `pending` until they
	// seal. true blocks the index on those units; false disables the provider.
	Enabled Enablement `toml:"enabled"`
	// Timeout bounds one whole unit. 0, the default, means no wall-clock
	// limit; StallTimeout is what catches a wedged unit.
	Timeout Duration `toml:"timeout"`
	// StallTimeout is the same progress-based hang detector SCIP.StallTimeout
	// describes, applied to one dependence unit.
	StallTimeout Duration `toml:"stall_timeout"`
	// CacheBytes budgets the per-unit parsed-graph cache under the private
	// data directory. That cache is what makes an unchanged unit cost nothing
	// on refresh, so it has its own budget rather than sharing the query cache.
	CacheBytes int64 `toml:"cache_bytes"`
	// UnitMemoryFloorBytes is the smallest allocation a unit may be sized to.
	// Sizing a unit near its live set costs time instead of memory, so the
	// floor keeps a small unit from being starved into a much slower run.
	UnitMemoryFloorBytes int64 `toml:"unit_memory_floor_bytes"`
	// UnitMemoryCeilingBytes is 0 by default, which means the allocation is
	// derived from the machine: free memory minus the base index footprint
	// minus a safety margin. There is no default memory ceiling, and only a
	// non-zero value here may reject a unit before it runs.
	UnitMemoryCeilingBytes int64 `toml:"unit_memory_ceiling_bytes"`
	// MaxUnitsPerFamily is how many projects of one language family the user
	// wants a plan to hold. Unlimited by default: a monorepo's project count
	// is a property of the repository, not something the product refuses on
	// the user's behalf. A user-set value never drops a project and never
	// refuses the plan -- exceeding it is reported on the family's capability
	// rows, naming the family, the project count and this bound.
	MaxUnitsPerFamily Limit `toml:"max_units_per_family"`
	// MaxStagedRows is how many rows the user wants one unit's import to
	// stage. Unlimited by default: staging is an on-disk, keyset-paged
	// database, so the row count bounds disk rather than heap. A user-set
	// value never fails the unit and never stops the import -- exceeding it is
	// reported on the unit's capability rows with the staged count and this
	// bound.
	MaxStagedRows Limit `toml:"max_staged_rows"`
	// MaxDerivedRows is how many relation occurrences the user wants one
	// unit's import to project from its staged rows. Unlimited by default: the
	// projection is computed and paged inside the same on-disk staging
	// database, so the occurrence count bounds disk rather than heap. A
	// user-set value never fails the unit and never truncates the projection --
	// exceeding it is reported on the unit's capability rows with the derived
	// count and this bound.
	MaxDerivedRows Limit `toml:"max_derived_rows"`
	// MaxExportFiles is how many entries the user allows in one analysis
	// export directory before the import refuses it. Unlimited by default: the
	// count is a property of the export's label vocabulary rather than of the
	// repository, and the directory is read one entry at a time. A user-set
	// value is the only thing that refuses an import here, and the refusal
	// names this key.
	MaxExportFiles Limit `toml:"max_export_files"`
}

// Tools is the managed analyzer toolchain policy of Section 11.7. The product
// owns every external analyzer and every runtime one needs, pinned by the
// embedded tool lock, so nothing here approves an executable: these are the
// store, fetch and offline settings, plus the one escape hatch that replaces a
// pinned binary. The whole table is user-configuration-only; a project file
// that sets any of it is a trust escalation (Section 20.2).
type Tools struct {
	// Offline makes every fetch a typed refusal without opening a socket.
	Offline bool `toml:"offline"`
	// CacheDir is the absolute tool store. Empty is resolved by Load to
	// DefaultToolStoreDir -- one machine-wide store shared by every workspace,
	// which the toolchain owner creates user-private on the first install.
	// Defaults() leaves it empty so it stays a pure function; a resolved
	// configuration always names a store.
	CacheDir string `toml:"cache_dir"`
	// Mirror is an absolute https prefix that replaces the scheme and host of
	// every lock asset URL, keeping the original host as the first path segment
	// so one mirror serves every publisher the lock names. The digests stay the
	// lock's, so a mirror can relocate bytes but never change which bytes are
	// accepted.
	Mirror string `toml:"mirror"`
	// MaxFetchBytes and FetchTimeout bound one payload download.
	MaxFetchBytes int64    `toml:"max_fetch_bytes"`
	FetchTimeout  Duration `toml:"fetch_timeout"`
	// Override is the `[tools.override.<name>]` table, keyed by lock entry
	// name. Iterate it in sorted order so diagnostics are deterministic.
	Override map[string]ToolOverride `toml:"override"`
}

// ToolOverride is the only way to run a tool the lock did not ship for this
// platform, or to replace one it did. It changes the binary and never the
// invocation: profile argv, environment allowlists, budgets, work directories
// and network posture are product code, not configuration (Section 20.2).
//
// All three fields are required. An override names a binary outside the lock,
// so the checksum is what makes it verifiable at all; without it the entry
// would admit whatever happens to sit at that path on the next run.
type ToolOverride struct {
	// Executable is an absolute path. PATH lookup is never an approval.
	Executable string `toml:"executable"`
	// Version is the exact version this binary is, not a constraint: it is
	// recorded as the tool identity behind the units the tool produces.
	Version string `toml:"version"`
	// Checksum is the required lowercase hex SHA-256 of Executable, verified
	// at the start of every run.
	Checksum string `toml:"checksum"`
}

// Context is the context-compiler ranking and budget policy. Every field feeds
// ContextPolicyHash, which is the ManifestID policy input of Section 20.2.
type Context struct {
	DefaultPhase           string `toml:"default_phase"`
	DefaultEstimatedTokens int64  `toml:"default_estimated_tokens"`
	DefaultMaxBytes        int64  `toml:"default_max_bytes"`
	DefaultMaxFiles        int    `toml:"default_max_files"`
	MaxSlices              int    `toml:"max_slices"`
	MaxGraphDepth          Limit  `toml:"max_graph_depth"`
	MaxVisitedNodes        Limit  `toml:"max_visited_nodes"`
	MaxGraphEdges          Limit  `toml:"max_graph_edges"`
	MaxReasonPathsPerEntry Limit  `toml:"max_reason_paths_per_entry"`
	MaxManifestBytes       Limit  `toml:"max_manifest_bytes"`
	MaxCapsuleBytes        Limit  `toml:"max_capsule_bytes"`
	// MaxCapsuleRecordsPerList bounds one sealing capsule list; zero is
	// unlimited and is the default, so a completion is never refused for the
	// number of observations a session recorded.
	MaxCapsuleRecordsPerList Limit `toml:"max_capsule_records_per_list"`
	// MaxCapsuleCoverageFiles is the same bound for the capsule's coverage
	// list alone, which grows with the session's pinned file set.
	MaxCapsuleCoverageFiles Limit `toml:"max_capsule_coverage_files"`
	StrictReadGate          bool  `toml:"strict_read_gate"`
	// AllowExploratoryWaiverConsolidation is user-only and never weakens strict
	// read readiness (Section 20.2).
	AllowExploratoryWaiverConsolidation bool `toml:"allow_exploratory_waiver_consolidation"`
	// MaxSeeds bounds Section 15.2 seed discovery: the identities a context
	// compile examines before it stops. Unlimited by default, so every identity
	// the task names is examined; a user-set value that a task exceeds is
	// reported as a named exclusion on the manifest, never applied silently.
	MaxSeeds Limit `toml:"max_seeds"`
	// MaxStartNodes bounds how many of the discovered seeds become roots of the
	// Section 15.2 boundary walk. Unlimited by default, so every resolvable
	// seed a task names is walked from; a user-set value that a task exceeds
	// leaves the seeds past it unwalked and says so, with the dropped count, as
	// a named exclusion on the manifest.
	//
	// The set is never chunked across several walks to honour a cut: each walk
	// ranks its own page, so two half-width walks would answer two local
	// rankings rather than the one global order a caller reads.
	MaxStartNodes Limit `toml:"max_start_nodes"`
}

// Coverage is the source-read chunking and receipt policy.
type Coverage struct {
	ChunkBytes                     int64    `toml:"chunk_bytes"`
	MaxChunkBytes                  int64    `toml:"max_chunk_bytes"`
	SessionTTL                     Duration `toml:"session_ttl"`
	MaxReceiptsPerConfirmation     int      `toml:"max_receipts_per_confirmation"`
	MaxUnconfirmedChunksPerSession Limit    `toml:"max_unconfirmed_chunks_per_session"`
}

// Workflow is the Section 17 workflow-service policy.
type Workflow struct {
	// MaxObservationReferences is how many references the user wants one
	// recorded observation -- or one scope review, counted across all eight of
	// its categories -- to carry. Unlimited by default: a review of a large
	// scope cites what it read, and an attestation is not refused for the size
	// of the repository it attests to. A user-set value is the operator's own
	// ceiling and exceeding it is reported with the count and this bound,
	// never silently trimmed.
	MaxObservationReferences Limit `toml:"max_observation_references"`
}

// MCP is the server transport policy.
type MCP struct {
	Transport string `toml:"transport"`
	Watch     bool   `toml:"watch"`
}

// TraversalPolicy is the traversal policy this configuration describes. The Git
// owner then fills in Ignore, ForceInclude and ForceIncludeDir from what the
// repository actually tracks.
//
// The mapping lives here rather than in internal/workspace so that the
// dependency runs one way: configuration knows about traversal, traversal knows
// nothing about configuration and stays usable by a caller that has none.
func (c Config) TraversalPolicy() workspace.Policy {
	return workspace.Policy{
		FollowSymlinks:   c.Workspace.FollowSymlinks,
		IndexVendor:      c.Workspace.IndexVendor,
		IndexGenerated:   c.Workspace.IndexGenerated,
		IncludeUntracked: c.Workspace.IncludeUntracked,
		MaxFiles:         c.Workspace.MaxFiles.Value(),
		MaxDirEntries:    c.Workspace.MaxDirEntries.Value(),
		MaxDepth:         c.Workspace.MaxDepth.Value(),
		MaxIgnoredRoots:  c.Workspace.MaxIgnoredRoots.Value(),
		DataDir:          c.Storage.DataDir,
	}
}

// Defaults returns the built-in configuration of Section 20.1. Storage.DataDir
// is empty here and is resolved against the workspace root by Load.
func Defaults() Config {
	return Config{
		Version: SchemaVersion,
		Workspace: Workspace{
			FollowSymlinks:     false,
			IncludeUntracked:   true,
			IndexGenerated:     false,
			IndexVendor:        false,
			MaxFiles:           Unlimited,
			MaxParseFileBytes:  Unlimited,
			MaxSearchFileBytes: Unlimited,
			MaxDirEntries:      Unlimited,
			MaxDepth:           Unlimited,
			MaxIgnoredRoots:    Unlimited,
		},
		Index: Index{
			Workers:           0,
			MaxParserWorkers:  2,
			BatchRecords:      1000,
			BatchBytes:        4194304,
			QueueBytes:        16777216,
			WatchPendingPaths: 10000,
			WatchPendingBytes: 2097152,
			WatchDebounce:     Duration(250 * time.Millisecond),
			ReconcileInterval: Duration(30 * time.Second),
			RetainRefs:        8,
			// 0 = unlimited retries, bounded by the deadline below.
			CaptureMaxRetries:    Unlimited,
			CaptureRetryDeadline: Duration(10 * time.Minute),
			MaxRetainedBytes:     0,
			// Unlimited: only a user-set bound stops the watch set.
			WatchMaxDirectories: Unlimited,
			// Unlimited: a fact keeps every occurrence up to the record
			// ceiling; only a user-set clip cuts one.
			MaxEvidencePerFact: Unlimited,
		},
		Resources: Resources{
			BaseMemoryBudgetBytes:     805306368,
			QueryMemoryBytes:          33554432,
			CacheBytes:                33554432,
			MaxConcurrentQueries:      4,
			MaxConcurrentGraphQueries: 2,
			MaxConcurrentHeavy:        1,
			MaxTempBytes:              0,
			MinFreeDiskBytes:          1073741824,
			MaxMetadataResponseBytes:  262144,
			MaxSourceResponseBytes:    7340032,
			QueryTimeout:              Duration(10 * time.Second),
			MaxQueryTextBytes:         8192,
			MaxQueryTerms:             Unlimited,
			MaxPageItems:              200,
			MaxProviderRecordBytes:    Unlimited,
		},
		Storage: Storage{
			DataDir:                "",
			BusyTimeout:            Duration(5 * time.Second),
			ReadConnections:        2,
			WriterCacheKiB:         8192,
			ReaderCacheKiB:         4096,
			WALHighWaterBytes:      67108864,
			ClosedSessionRetention: Duration(7 * 24 * time.Hour),
			QueryCursorTTL:         Duration(15 * time.Minute),
			Synchronous:            SynchronousNormal,
		},
		Retention: Retention{
			BlobGrace: Duration(24 * time.Hour),
		},
		Tools: Tools{
			Offline:       false,
			CacheDir:      "",
			Mirror:        "",
			MaxFetchBytes: 2147483648,
			FetchTimeout:  Duration(10 * time.Minute),
		},
		Providers: Providers{
			TreeSitter: TreeSitter{
				Enabled:             true,
				Languages:           []string{"go", "javascript", "typescript", "tsx", "python", "java", "rust", "c", "cpp"},
				WorkerIdleTTL:       Duration(60 * time.Second),
				MaxCalleeReferences: Unlimited,
				MaxRecordsPerFile:   Unlimited,
			},
			SCIP: SCIP{Enabled: Auto, Timeout: 0, StallTimeout: Duration(5 * time.Minute),
				MaxIndexBytes: Unlimited, MaxManifestBytes: Unlimited, MaxDocuments: Unlimited,
				MaxOccurrencesPerDocument: Unlimited, MaxSpoolBytes: Unlimited,
				MaxSourceFileBytes: Unlimited, MaxMaterializeBytes: Unlimited},
			LSP: LSP{
				Enabled:                Auto,
				RequestTimeout:         Duration(15 * time.Second),
				MaxServers:             1,
				MaxOutstandingRequests: 8,
				IdleTTL:                Duration(60 * time.Second),
				MaxOverlayBytes:        Unlimited,
			},
			Dependence: Dependence{
				Enabled:                Auto,
				Timeout:                0,
				StallTimeout:           Duration(5 * time.Minute),
				CacheBytes:             4294967296,
				UnitMemoryFloorBytes:   805306368,
				UnitMemoryCeilingBytes: 0,
				MaxUnitsPerFamily:      Unlimited,
				MaxStagedRows:          Unlimited,
				MaxDerivedRows:         Unlimited,
				MaxExportFiles:         Unlimited,
			},
			Manifest: Manifest{MaxDependencies: Unlimited, MaxEntries: Unlimited,
				MaxTOMLLines: Unlimited, MaxXMLElements: Unlimited},
		},
		Context: Context{
			DefaultPhase:                        "sweep",
			DefaultEstimatedTokens:              80000,
			DefaultMaxBytes:                     524288,
			DefaultMaxFiles:                     200,
			MaxSlices:                           16,
			MaxGraphDepth:                       Unlimited,
			MaxVisitedNodes:                     Unlimited,
			MaxGraphEdges:                       Unlimited,
			MaxReasonPathsPerEntry:              Unlimited,
			MaxManifestBytes:                    Unlimited,
			MaxCapsuleBytes:                     Unlimited,
			MaxCapsuleRecordsPerList:            Unlimited,
			MaxCapsuleCoverageFiles:             Unlimited,
			StrictReadGate:                      true,
			AllowExploratoryWaiverConsolidation: false,
			MaxSeeds:                            Unlimited,
			MaxStartNodes:                       Unlimited,
		},
		Coverage: Coverage{
			ChunkBytes:                     65536,
			MaxChunkBytes:                  1048576,
			SessionTTL:                     Duration(24 * time.Hour),
			MaxReceiptsPerConfirmation:     16,
			MaxUnconfirmedChunksPerSession: Unlimited,
		},
		Workflow: Workflow{MaxObservationReferences: Unlimited},
		MCP:      MCP{Transport: "stdio", Watch: true},
	}
}

func configInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeConfigInvalid, Message: fmt.Sprintf(format, args...)}
}

func trustRequired(format string, args ...any) *model.Error {
	return (&model.Error{Code: model.CodeTrustRequired, Message: fmt.Sprintf(format, args...)}).
		WithRemediation("Move the setting to the user configuration file, which is outside the repository and not written by the code being analyzed.")
}

// quoteLimit renders a bound as a fingerprint component. An unlimited bound has
// exactly one spelling, so `0` and "unlimited" hash identically and neither
// collides with any finite value.
func quoteLimit(l Limit) string { return l.String() }

// quoteInt renders a number as a fingerprint component.
func quoteInt(v int64) string { return strconv.FormatInt(v, 10) }

// quoteBool renders a boolean as a fingerprint component.
func quoteBool(v bool) string { return strconv.FormatBool(v) }
