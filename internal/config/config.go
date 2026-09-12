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
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
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
// duration is never zero or negative: Section 20.2 forbids a zero setting that
// means unlimited.
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
	Providers Providers `toml:"providers"`
	Context   Context   `toml:"context"`
	Coverage  Coverage  `toml:"coverage"`
	MCP       MCP       `toml:"mcp"`
	// Analyzers is the `[analyzers.<name>]` table of approved profiles. It is
	// user-configuration-only; a project file that declares one is rejected as
	// a trust escalation. Iterate it through SortedAnalyzers so diagnostics and
	// fingerprints have a deterministic order.
	Analyzers map[string]Analyzer `toml:"analyzers"`
}

// SortedAnalyzers returns the approved profiles ordered by name, with each
// profile's Name filled in. Map iteration order is random, so every consumer
// that reports or hashes profiles uses this instead.
func (c Config) SortedAnalyzers() []Analyzer {
	names := make([]string, 0, len(c.Analyzers))
	for name := range c.Analyzers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Analyzer, 0, len(names))
	for _, name := range names {
		a := c.Analyzers[name]
		a.Name = name
		out = append(out, a)
	}
	return out
}

// Workspace is the source-eligibility policy. Every field here feeds
// SourcePolicyHash except the two analysis admission limits, which feed
// AnalysisConfigHash instead (Section 20.2).
type Workspace struct {
	FollowSymlinks     bool  `toml:"follow_symlinks"`
	IncludeUntracked   bool  `toml:"include_untracked"`
	IndexGenerated     bool  `toml:"index_generated"`
	IndexVendor        bool  `toml:"index_vendor"`
	MaxFiles           int64 `toml:"max_files"`
	MaxParseFileBytes  int64 `toml:"max_parse_file_bytes"`
	MaxSearchFileBytes int64 `toml:"max_search_file_bytes"`
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
	RetainGenerations int      `toml:"retain_generations"`
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
	MaxQueryTerms             int      `toml:"max_query_terms"`
	MaxPageItems              int      `toml:"max_page_items"`
	MaxProviderRecordBytes    int64    `toml:"max_provider_record_bytes"`
}

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
}

// Providers groups the four provider families of Section 20.1.
type Providers struct {
	TreeSitter TreeSitter `toml:"tree_sitter"`
	SCIP       SCIP       `toml:"scip"`
	LSP        LSP        `toml:"lsp"`
	Joern      Joern      `toml:"joern"`
}

// TreeSitter configures the bundled structural provider. It runs as a private
// subcommand of this same binary with bundled grammars, so `enabled` is a plain
// boolean: there is no external executable to approve.
type TreeSitter struct {
	Enabled       bool     `toml:"enabled"`
	Languages     []string `toml:"languages"`
	WorkerIdleTTL Duration `toml:"worker_idle_ttl"`
}

// SCIP configures the external index importer.
type SCIP struct {
	Enabled Enablement `toml:"enabled"`
	Timeout Duration   `toml:"timeout"`
}

// LSP configures the snapshot-qualified working-tree overlay.
type LSP struct {
	Enabled                Enablement `toml:"enabled"`
	RequestTimeout         Duration   `toml:"request_timeout"`
	MaxServers             int        `toml:"max_servers"`
	MaxOutstandingRequests int        `toml:"max_outstanding_requests"`
	IdleTTL                Duration   `toml:"idle_ttl"`
}

// Joern configures the optional deep-analysis adapter.
type Joern struct {
	Enabled Enablement `toml:"enabled"`
	Profile string     `toml:"profile"`
	Timeout Duration   `toml:"timeout"`
}

// Context is the context-compiler ranking and budget policy. Every field feeds
// ContextPolicyHash, which is the ManifestID policy input of Section 20.2.
type Context struct {
	DefaultPhase           string `toml:"default_phase"`
	DefaultEstimatedTokens int64  `toml:"default_estimated_tokens"`
	DefaultMaxBytes        int64  `toml:"default_max_bytes"`
	DefaultMaxFiles        int    `toml:"default_max_files"`
	MaxSlices              int    `toml:"max_slices"`
	MaxGraphDepth          int    `toml:"max_graph_depth"`
	MaxVisitedNodes        int    `toml:"max_visited_nodes"`
	MaxGraphEdges          int    `toml:"max_graph_edges"`
	MaxReasonPathsPerEntry int    `toml:"max_reason_paths_per_entry"`
	MaxManifestBytes       int64  `toml:"max_manifest_bytes"`
	MaxCapsuleBytes        int64  `toml:"max_capsule_bytes"`
	StrictReadGate         bool   `toml:"strict_read_gate"`
	// AllowExploratoryWaiverConsolidation is user-only and never weakens strict
	// read readiness (Section 20.2).
	AllowExploratoryWaiverConsolidation bool `toml:"allow_exploratory_waiver_consolidation"`
}

// Coverage is the source-read chunking and receipt policy.
type Coverage struct {
	ChunkBytes                     int64    `toml:"chunk_bytes"`
	MaxChunkBytes                  int64    `toml:"max_chunk_bytes"`
	SessionTTL                     Duration `toml:"session_ttl"`
	MaxReceiptsPerConfirmation     int      `toml:"max_receipts_per_confirmation"`
	MaxUnconfirmedChunksPerSession int      `toml:"max_unconfirmed_chunks_per_session"`
}

// MCP is the server transport policy.
type MCP struct {
	Transport string `toml:"transport"`
	Watch     bool   `toml:"watch"`
}

// NetworkPolicy is an analyzer profile's declared network posture. `denied` is
// a policy statement and an input to the diagnostics that report whether an
// actual OS-level restriction is active; on its own it is not a network
// boundary (Section 21).
type NetworkPolicy string

const (
	NetworkDenied  NetworkPolicy = "denied"
	NetworkAllowed NetworkPolicy = "allowed"
)

// Valid reports whether p is a known wire spelling.
func (p NetworkPolicy) Valid() bool { return p == NetworkDenied || p == NetworkAllowed }

// Analyzer is one approved analyzer profile (Section 20.2). It exists only in
// the user configuration: a project file that declares one is an escalation.
//
// The profile is a description, not a capability: this package validates its
// shape, and the provider that owns the tool resolves, checksums and runs it
// through internal/process.
type Analyzer struct {
	// Name is the profile's table key, filled in by Load.
	Name string `toml:"-"`
	// Executable is an absolute path. Section 20.2 forbids PATH lookup, so a
	// bare command name is rejected rather than resolved.
	Executable string `toml:"executable"`
	// VersionConstraint is the exact version or range the caller must observe
	// before admitting output from this tool.
	VersionConstraint string `toml:"version_constraint"`
	// Checksum is the expected lowercase SHA-256 hex of the executable, which
	// is how writable-repository executable substitution is detected.
	Checksum string `toml:"checksum"`
	// Args is a fixed argument array. Placeholders are the typed substitutions
	// named by AnalyzerSubstitutions; no other template syntax is accepted and
	// there is no shell, so a value can never become a shell fragment.
	Args []string `toml:"args"`
	// EnvAllowlist names the environment variables the child may inherit. Any
	// variable not named here is absent from the child environment entirely.
	EnvAllowlist []string `toml:"env_allowlist"`
	// WorkDir is the absolute private working directory for this analyzer.
	WorkDir string `toml:"work_dir"`
	// MemoryBudgetBytes and DiskBudgetBytes are the reservations the runner
	// accounts before the child starts.
	MemoryBudgetBytes int64 `toml:"memory_budget_bytes"`
	DiskBudgetBytes   int64 `toml:"disk_budget_bytes"`
	// Network is the declared posture, not an enforced sandbox.
	Network NetworkPolicy `toml:"network"`
	Timeout Duration      `toml:"timeout"`
}

// AnalyzerSubstitutions is the closed set of typed placeholders an approved
// profile's argument array may contain. Substitution values are supplied by the
// provider at run time from validated private paths; an unknown placeholder is
// a configuration error rather than a literal argument, because a silently
// literal "${input_dir}" would make the tool read the wrong tree.
var AnalyzerSubstitutions = map[string]bool{
	"input_dir":   true,
	"output_file": true,
	"work_dir":    true,
	"manifest":    true,
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
			MaxFiles:           250000,
			MaxParseFileBytes:  5242880,
			MaxSearchFileBytes: 26214400,
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
			RetainGenerations: 3,
		},
		Resources: Resources{
			BaseMemoryBudgetBytes:     805306368,
			QueryMemoryBytes:          33554432,
			CacheBytes:                33554432,
			MaxConcurrentQueries:      4,
			MaxConcurrentGraphQueries: 2,
			MaxConcurrentHeavy:        1,
			MaxTempBytes:              4294967296,
			MinFreeDiskBytes:          1073741824,
			MaxMetadataResponseBytes:  262144,
			MaxSourceResponseBytes:    7340032,
			QueryTimeout:              Duration(10 * time.Second),
			MaxQueryTextBytes:         8192,
			MaxQueryTerms:             32,
			MaxPageItems:              200,
			MaxProviderRecordBytes:    4194304,
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
		},
		Providers: Providers{
			TreeSitter: TreeSitter{
				Enabled:       true,
				Languages:     []string{"go", "javascript", "typescript", "tsx", "python", "java", "rust", "c", "cpp"},
				WorkerIdleTTL: Duration(60 * time.Second),
			},
			SCIP: SCIP{Enabled: Auto, Timeout: Duration(20 * time.Minute)},
			LSP: LSP{
				Enabled:                Auto,
				RequestTimeout:         Duration(15 * time.Second),
				MaxServers:             1,
				MaxOutstandingRequests: 8,
				IdleTTL:                Duration(60 * time.Second),
			},
			Joern: Joern{Enabled: Disabled, Profile: "pinned-default", Timeout: Duration(45 * time.Minute)},
		},
		Context: Context{
			DefaultPhase:                        "sweep",
			DefaultEstimatedTokens:              80000,
			DefaultMaxBytes:                     524288,
			DefaultMaxFiles:                     200,
			MaxSlices:                           16,
			MaxGraphDepth:                       3,
			MaxVisitedNodes:                     50000,
			MaxGraphEdges:                       100000,
			MaxReasonPathsPerEntry:              3,
			MaxManifestBytes:                    8388608,
			MaxCapsuleBytes:                     8388608,
			StrictReadGate:                      true,
			AllowExploratoryWaiverConsolidation: false,
		},
		Coverage: Coverage{
			ChunkBytes:                     65536,
			MaxChunkBytes:                  1048576,
			SessionTTL:                     Duration(24 * time.Hour),
			MaxReceiptsPerConfirmation:     16,
			MaxUnconfirmedChunksPerSession: 64,
		},
		MCP: MCP{Transport: "stdio", Watch: true},
	}
}

// validate enforces the whole resolved configuration, including the cross-field
// budget rules of Section 20.1's closing paragraph. No zero or negative setting
// means unlimited, so every bound is checked for a positive value first.
func (c Config) validate() error {
	if c.Version != SchemaVersion {
		return configInvalid("version is %d; this build understands configuration version %d", c.Version, SchemaVersion)
	}
	for _, p := range []struct {
		key string
		v   int64
	}{
		{"workspace.max_files", c.Workspace.MaxFiles},
		{"workspace.max_parse_file_bytes", c.Workspace.MaxParseFileBytes},
		{"workspace.max_search_file_bytes", c.Workspace.MaxSearchFileBytes},
		{"index.max_parser_workers", int64(c.Index.MaxParserWorkers)},
		{"index.batch_records", int64(c.Index.BatchRecords)},
		{"index.batch_bytes", c.Index.BatchBytes},
		{"index.queue_bytes", c.Index.QueueBytes},
		{"index.watch_pending_paths", int64(c.Index.WatchPendingPaths)},
		{"index.watch_pending_bytes", c.Index.WatchPendingBytes},
		{"index.retain_generations", int64(c.Index.RetainGenerations)},
		{"resources.base_memory_budget_bytes", c.Resources.BaseMemoryBudgetBytes},
		{"resources.query_memory_bytes", c.Resources.QueryMemoryBytes},
		{"resources.cache_bytes", c.Resources.CacheBytes},
		{"resources.max_concurrent_queries", int64(c.Resources.MaxConcurrentQueries)},
		{"resources.max_concurrent_graph_queries", int64(c.Resources.MaxConcurrentGraphQueries)},
		{"resources.max_concurrent_heavy_analyzers", int64(c.Resources.MaxConcurrentHeavy)},
		{"resources.max_temp_bytes", c.Resources.MaxTempBytes},
		{"resources.min_free_disk_bytes", c.Resources.MinFreeDiskBytes},
		{"resources.max_metadata_response_bytes", c.Resources.MaxMetadataResponseBytes},
		{"resources.max_source_response_bytes", c.Resources.MaxSourceResponseBytes},
		{"resources.max_query_text_bytes", int64(c.Resources.MaxQueryTextBytes)},
		{"resources.max_query_terms", int64(c.Resources.MaxQueryTerms)},
		{"resources.max_page_items", int64(c.Resources.MaxPageItems)},
		{"resources.max_provider_record_bytes", c.Resources.MaxProviderRecordBytes},
		{"storage.read_connections", int64(c.Storage.ReadConnections)},
		{"storage.writer_cache_kib", int64(c.Storage.WriterCacheKiB)},
		{"storage.reader_cache_kib", int64(c.Storage.ReaderCacheKiB)},
		{"storage.wal_high_water_bytes", c.Storage.WALHighWaterBytes},
		{"providers.lsp.max_servers", int64(c.Providers.LSP.MaxServers)},
		{"providers.lsp.max_outstanding_requests", int64(c.Providers.LSP.MaxOutstandingRequests)},
		{"context.default_estimated_tokens", c.Context.DefaultEstimatedTokens},
		{"context.default_max_bytes", c.Context.DefaultMaxBytes},
		{"context.default_max_files", int64(c.Context.DefaultMaxFiles)},
		{"context.max_slices", int64(c.Context.MaxSlices)},
		{"context.max_graph_depth", int64(c.Context.MaxGraphDepth)},
		{"context.max_visited_nodes", int64(c.Context.MaxVisitedNodes)},
		{"context.max_graph_edges", int64(c.Context.MaxGraphEdges)},
		{"context.max_reason_paths_per_entry", int64(c.Context.MaxReasonPathsPerEntry)},
		{"context.max_manifest_bytes", c.Context.MaxManifestBytes},
		{"context.max_capsule_bytes", c.Context.MaxCapsuleBytes},
		{"coverage.chunk_bytes", c.Coverage.ChunkBytes},
		{"coverage.max_chunk_bytes", c.Coverage.MaxChunkBytes},
		{"coverage.max_receipts_per_confirmation", int64(c.Coverage.MaxReceiptsPerConfirmation)},
		{"coverage.max_unconfirmed_chunks_per_session", int64(c.Coverage.MaxUnconfirmedChunksPerSession)},
	} {
		if p.v <= 0 {
			return configInvalid("%s is %d; no zero or negative setting means unlimited", p.key, p.v)
		}
	}
	if c.Index.Workers < 0 {
		return configInvalid("index.workers is %d; use 0 to choose from available CPUs and reservations", c.Index.Workers)
	}
	for _, d := range []struct {
		key string
		v   Duration
	}{
		{"index.watch_debounce", c.Index.WatchDebounce},
		{"index.reconcile_interval", c.Index.ReconcileInterval},
		{"resources.query_timeout", c.Resources.QueryTimeout},
		{"storage.busy_timeout", c.Storage.BusyTimeout},
		{"storage.closed_session_retention", c.Storage.ClosedSessionRetention},
		{"storage.query_cursor_ttl", c.Storage.QueryCursorTTL},
		{"providers.tree_sitter.worker_idle_ttl", c.Providers.TreeSitter.WorkerIdleTTL},
		{"providers.scip.timeout", c.Providers.SCIP.Timeout},
		{"providers.lsp.request_timeout", c.Providers.LSP.RequestTimeout},
		{"providers.lsp.idle_ttl", c.Providers.LSP.IdleTTL},
		{"providers.joern.timeout", c.Providers.Joern.Timeout},
		{"coverage.session_ttl", c.Coverage.SessionTTL},
	} {
		if d.v <= 0 {
			return configInvalid("%s is %s; a duration must be positive", d.key, d.v)
		}
	}
	if err := c.validateStructuralCeilings(); err != nil {
		return err
	}
	return c.validateBudgets()
}

// validateStructuralCeilings rejects a configured limit larger than the
// contract ceiling internal/model enforces. Configuration may lower an
// effective limit but never raise one past what a stored column or a bounded
// response can hold.
func (c Config) validateStructuralCeilings() error {
	for _, p := range []struct {
		key      string
		v, limit int64
	}{
		{"workspace.max_files", c.Workspace.MaxFiles, model.MaxCoverageFilesPerCapsule},
		{"resources.max_page_items", int64(c.Resources.MaxPageItems), model.MaxPageItems},
		{"resources.max_query_text_bytes", int64(c.Resources.MaxQueryTextBytes), model.MaxQueryTextBytes},
		{"resources.max_provider_record_bytes", c.Resources.MaxProviderRecordBytes, model.MaxSearchBodyBytes * 128},
		{"context.max_reason_paths_per_entry", int64(c.Context.MaxReasonPathsPerEntry), model.MaxReasonPathsPerEntry},
		{"coverage.max_chunk_bytes", c.Coverage.MaxChunkBytes, model.MaxRawChunkBytes},
		{"coverage.max_receipts_per_confirmation", int64(c.Coverage.MaxReceiptsPerConfirmation), model.MaxReceiptsPerConfirmation},
	} {
		if p.v > p.limit {
			return configInvalid("%s is %d; the contract ceiling is %d", p.key, p.v, p.limit)
		}
	}
	if c.Providers.TreeSitter.Enabled && len(c.Providers.TreeSitter.Languages) == 0 {
		return configInvalid("providers.tree_sitter is enabled with an empty language list")
	}
	for i, lang := range c.Providers.TreeSitter.Languages {
		if lang == "" || len(lang) > model.MaxLanguageBytes {
			return configInvalid("providers.tree_sitter.languages[%d] is not a language tag of 1..%d bytes", i, model.MaxLanguageBytes)
		}
	}
	if !model.Phase(c.Context.DefaultPhase).Valid() {
		return configInvalid("context.default_phase %q is not a known workflow phase", c.Context.DefaultPhase)
	}
	if c.MCP.Transport != "stdio" {
		return configInvalid("mcp.transport %q is not supported; this build serves MCP over stdio", c.MCP.Transport)
	}
	if c.Providers.Joern.Profile == "" {
		return configInvalid("providers.joern.profile is empty; `auto` selects an approved profile, never an arbitrary binary")
	}
	return nil
}

// validateBudgets enforces the related-value rules of Section 20.1: a source
// chunk plus its worst-case wire encoding must fit the source response budget,
// batches must fit the queue and memory reservations, baseline concurrency must
// fit the aggregate memory budget, and byte arithmetic must not overflow.
func (c Config) validateBudgets() error {
	if c.Coverage.ChunkBytes > c.Coverage.MaxChunkBytes {
		return configInvalid("coverage.chunk_bytes %d exceeds coverage.max_chunk_bytes %d",
			c.Coverage.ChunkBytes, c.Coverage.MaxChunkBytes)
	}
	// base64 is the widest encoding a chunk response may use, and the envelope
	// carries the binding, ranges and receipt around it.
	const sourceEnvelopeBytes = 4096
	worstCase := (c.Coverage.MaxChunkBytes+2)/3*4 + sourceEnvelopeBytes
	if worstCase > c.Resources.MaxSourceResponseBytes {
		return configInvalid("coverage.max_chunk_bytes %d needs %d wire bytes once base64-encoded, over resources.max_source_response_bytes %d",
			c.Coverage.MaxChunkBytes, worstCase, c.Resources.MaxSourceResponseBytes)
	}
	if c.Resources.MaxSourceResponseBytes > MaxSourceWireCeilingBytes {
		return configInvalid("resources.max_source_response_bytes %d exceeds the %d-byte source wire ceiling",
			c.Resources.MaxSourceResponseBytes, MaxSourceWireCeilingBytes)
	}
	if c.Resources.MaxMetadataResponseBytes >= c.Resources.MaxSourceResponseBytes {
		return configInvalid("resources.max_metadata_response_bytes %d is not smaller than the source budget %d; generic tools must stay under the smaller metadata limit",
			c.Resources.MaxMetadataResponseBytes, c.Resources.MaxSourceResponseBytes)
	}
	if c.Index.BatchBytes > c.Index.QueueBytes {
		return configInvalid("index.batch_bytes %d exceeds index.queue_bytes %d; a batch must fit the queue reservation",
			c.Index.BatchBytes, c.Index.QueueBytes)
	}
	if c.Resources.MaxProviderRecordBytes > c.Index.BatchBytes {
		return configInvalid("resources.max_provider_record_bytes %d exceeds index.batch_bytes %d; one record must fit one batch",
			c.Resources.MaxProviderRecordBytes, c.Index.BatchBytes)
	}
	if c.Resources.MaxConcurrentGraphQueries > c.Resources.MaxConcurrentQueries {
		return configInvalid("resources.max_concurrent_graph_queries %d exceeds resources.max_concurrent_queries %d",
			c.Resources.MaxConcurrentGraphQueries, c.Resources.MaxConcurrentQueries)
	}
	concurrent, err := mulNoOverflow("resources.max_concurrent_queries * resources.query_memory_bytes",
		int64(c.Resources.MaxConcurrentQueries), c.Resources.QueryMemoryBytes)
	if err != nil {
		return err
	}
	baseline, err := addNoOverflow("baseline memory reservation", concurrent, c.Resources.CacheBytes, c.Index.QueueBytes)
	if err != nil {
		return err
	}
	if baseline > c.Resources.BaseMemoryBudgetBytes {
		return configInvalid("query, cache and queue reservations need %d bytes, over resources.base_memory_budget_bytes %d",
			baseline, c.Resources.BaseMemoryBudgetBytes)
	}
	if c.Resources.MaxTempBytes <= c.Resources.MinFreeDiskBytes {
		return configInvalid("resources.max_temp_bytes %d does not exceed the free-space reserve resources.min_free_disk_bytes %d; temporary work would always be refused",
			c.Resources.MaxTempBytes, c.Resources.MinFreeDiskBytes)
	}
	if c.Context.DefaultMaxFiles > int(c.Workspace.MaxFiles) {
		return configInvalid("context.default_max_files %d exceeds workspace.max_files %d",
			c.Context.DefaultMaxFiles, c.Workspace.MaxFiles)
	}
	if c.Context.DefaultMaxBytes > c.Context.MaxManifestBytes {
		return configInvalid("context.default_max_bytes %d exceeds context.max_manifest_bytes %d",
			c.Context.DefaultMaxBytes, c.Context.MaxManifestBytes)
	}
	// The token estimate is derived from bytes, so its arithmetic must not
	// overflow before the compiler ever runs.
	if _, err := model.EstimateTokensUTF8Bytes(c.Context.DefaultMaxBytes); err != nil {
		return configInvalid("context.default_max_bytes %d cannot be converted to a token estimate: %v",
			c.Context.DefaultMaxBytes, err)
	}
	return c.validateAnalyzers()
}

// validateAnalyzers enforces the approved-profile shape of Section 20.2.
func (c Config) validateAnalyzers() error {
	for _, a := range c.SortedAnalyzers() {
		key := "analyzers." + a.Name
		if !isAbsolutePath(a.Executable) {
			return configInvalid("%s.executable %q is not an absolute path; PATH lookup is not an approval", key, a.Executable)
		}
		if a.VersionConstraint == "" {
			return configInvalid("%s.version_constraint is required; an unconstrained tool cannot be recorded as approved", key)
		}
		if a.Checksum != "" && !model.ValidHexID(a.Checksum) {
			return configInvalid("%s.checksum is not %d lowercase hex characters", key, model.IDHexLen)
		}
		if !isAbsolutePath(a.WorkDir) {
			return configInvalid("%s.work_dir %q is not an absolute private directory", key, a.WorkDir)
		}
		if a.MemoryBudgetBytes <= 0 || a.DiskBudgetBytes <= 0 {
			return configInvalid("%s declares memory %d and disk %d; a budget must be positive",
				key, a.MemoryBudgetBytes, a.DiskBudgetBytes)
		}
		if a.Timeout <= 0 {
			return configInvalid("%s.timeout is %s; a profile must bound its own runtime", key, a.Timeout)
		}
		if !a.Network.Valid() {
			return configInvalid("%s.network %q is not %q or %q", key, a.Network, NetworkDenied, NetworkAllowed)
		}
		for i, arg := range a.Args {
			if err := validateArgTemplate(key, i, arg); err != nil {
				return err
			}
		}
		for i, name := range a.EnvAllowlist {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return configInvalid("%s.env_allowlist[%d] %q is not an environment variable name", key, i, name)
			}
		}
	}
	return nil
}

// validateArgTemplate rejects an argument containing anything but the closed
// set of typed substitutions. An unknown placeholder would otherwise reach the
// child as a literal and silently point the tool at the wrong path.
func validateArgTemplate(key string, i int, arg string) error {
	if strings.ContainsRune(arg, 0) {
		return configInvalid("%s.args[%d] contains a NUL byte", key, i)
	}
	rest := arg
	for {
		open := strings.Index(rest, "${")
		if open < 0 {
			if strings.Contains(rest, "}") && strings.Contains(arg, "$") {
				return configInvalid("%s.args[%d] %q has an unbalanced substitution", key, i, arg)
			}
			return nil
		}
		rest = rest[open+2:]
		close := strings.Index(rest, "}")
		if close < 0 {
			return configInvalid("%s.args[%d] %q has an unterminated substitution", key, i, arg)
		}
		name := rest[:close]
		if !AnalyzerSubstitutions[name] {
			return configInvalid("%s.args[%d] uses unknown substitution ${%s}", key, i, name)
		}
		rest = rest[close+1:]
	}
}

func mulNoOverflow(what string, a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	product := a * b
	if product/b != a {
		return 0, configInvalid("%s overflows 64-bit arithmetic", what)
	}
	return product, nil
}

func addNoOverflow(what string, values ...int64) (int64, error) {
	var total int64
	for _, v := range values {
		if total > math.MaxInt64-v {
			return 0, configInvalid("%s overflows 64-bit arithmetic", what)
		}
		total += v
	}
	return total, nil
}

func configInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeConfigInvalid, Message: fmt.Sprintf(format, args...)}
}

func trustRequired(format string, args ...any) *model.Error {
	return (&model.Error{Code: model.CodeTrustRequired, Message: fmt.Sprintf(format, args...)}).
		WithRemediation("Move the setting to the user configuration file, which is outside the repository and not written by the code being analyzed.")
}

// quoteInt renders a number as a fingerprint component.
func quoteInt(v int64) string { return strconv.FormatInt(v, 10) }

// quoteBool renders a boolean as a fingerprint component.
func quoteBool(v bool) string { return strconv.FormatBool(v) }
