package config

import (
	"math"
	"net/url"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

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
		{"index.retain_refs", int64(c.Index.RetainRefs)},
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
		{"providers.lsp.max_overlay_bytes", c.Providers.LSP.MaxOverlayBytes},
		{"providers.dependence.cache_bytes", c.Providers.Dependence.CacheBytes},
		{"providers.dependence.unit_memory_floor_bytes", c.Providers.Dependence.UnitMemoryFloorBytes},
		{"tools.max_fetch_bytes", c.Tools.MaxFetchBytes},
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
	// The two settings whose 0 is a documented policy rather than a limit. A
	// negative value is still rejected: it is not a third meaning.
	for _, p := range []struct {
		key, zeroMeans string
		v              int64
	}{
		{"index.max_retained_bytes", "retention governed by index.retain_refs alone", c.Index.MaxRetainedBytes},
		{"providers.dependence.unit_memory_ceiling_bytes", "the machine-derived allocation", c.Providers.Dependence.UnitMemoryCeilingBytes},
	} {
		if p.v < 0 {
			return configInvalid("%s is %d; use 0 for %s", p.key, p.v, p.zeroMeans)
		}
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
		{"providers.dependence.timeout", c.Providers.Dependence.Timeout},
		{"tools.fetch_timeout", c.Tools.FetchTimeout},
		{"coverage.session_ttl", c.Coverage.SessionTTL},
	} {
		if d.v <= 0 {
			return configInvalid("%s is %s; a duration must be positive", d.key, d.v)
		}
	}
	if err := c.validateStructuralCeilings(); err != nil {
		return err
	}
	if err := c.validateBudgets(); err != nil {
		return err
	}
	return c.validateTools()
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
		{"resources.max_page_items", int64(c.Resources.MaxPageItems), model.MaxPageItems},
		{"resources.max_query_text_bytes", int64(c.Resources.MaxQueryTextBytes), model.MaxQueryTextBytes},
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
	if c.Providers.LSP.MaxOverlayBytes < c.Resources.MaxSourceResponseBytes {
		return configInvalid("providers.lsp.max_overlay_bytes %d is smaller than resources.max_source_response_bytes %d; one served source response must fit the overlay",
			c.Providers.LSP.MaxOverlayBytes, c.Resources.MaxSourceResponseBytes)
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
	// A ceiling under the floor is not a narrow budget, it is a provider that
	// rejects every unit before it runs while reporting a configured limit.
	if ceiling := c.Providers.Dependence.UnitMemoryCeilingBytes; ceiling > 0 && ceiling < c.Providers.Dependence.UnitMemoryFloorBytes {
		return configInvalid("providers.dependence.unit_memory_ceiling_bytes %d is below providers.dependence.unit_memory_floor_bytes %d; every unit would be rejected before it runs",
			ceiling, c.Providers.Dependence.UnitMemoryFloorBytes)
	}
	// The token estimate is derived from bytes, so its arithmetic must not
	// overflow before the compiler ever runs.
	if _, err := model.EstimateTokensUTF8Bytes(c.Context.DefaultMaxBytes); err != nil {
		return configInvalid("context.default_max_bytes %d cannot be converted to a token estimate: %v",
			c.Context.DefaultMaxBytes, err)
	}
	return nil
}

// validateTools enforces the toolchain policy of Sections 11.7 and 20.2. The
// three override fields are all required together: an override names a binary
// the lock does not describe, so an exact version and a checksum are the only
// things that make it identifiable at all, and a store location or mirror that
// is not a usable absolute location would fail at the first fetch instead of
// at startup.
func (c Config) validateTools() error {
	if c.Tools.CacheDir != "" && !isAbsolutePath(c.Tools.CacheDir) {
		return configInvalid("tools.cache_dir %q is not an absolute path", c.Tools.CacheDir)
	}
	if c.Tools.Mirror != "" {
		// https only, exactly as the toolchain resolver requires: a payload host
		// reached in plaintext is not a posture Section 21's one outbound path
		// accepts, and accepting it here would turn a configuration mistake into
		// a CTX_ARGUMENT_INVALID from the composition root with no key named.
		u, err := url.Parse(c.Tools.Mirror)
		if err != nil || u.Host == "" || u.Scheme != "https" {
			return configInvalid("tools.mirror %q is not an absolute https URL prefix", c.Tools.Mirror)
		}
	}
	// Map iteration order is random and the first failure names its tool, so
	// the same configuration must not report a different one each run.
	names := make([]string, 0, len(c.Tools.Override))
	for name := range c.Tools.Override {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		o := c.Tools.Override[name]
		key := "tools.override." + name
		if !isAbsolutePath(o.Executable) {
			return configInvalid("%s.executable %q is not an absolute path; PATH lookup is not an approval", key, o.Executable)
		}
		if o.Version == "" {
			return configInvalid("%s.version is required; an override states the exact version of the binary it names", key)
		}
		if !model.ValidHexID(o.Checksum) {
			return configInvalid("%s.checksum is required and must be %d lowercase hex characters", key, model.IDHexLen)
		}
	}
	return nil
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
