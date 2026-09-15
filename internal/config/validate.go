package config

import (
	"math"
	"net/url"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

// validate enforces the whole resolved configuration, including the cross-field
// budget rules of Section 20.1's closing paragraph.
//
// Two groups, and the difference between them is the whole scale posture. A
// BOUND (Limit) is what the product refuses to exceed on the user's behalf: 0
// means unlimited, that is the default for every one of them, and only a
// negative value is rejected. A RESERVATION (worker counts, batch sizes, queue
// and memory budgets, connection counts, wire ceilings) is not a limit on the
// repository at all -- it is how much machine the work is given, it must be
// positive because a zero-sized batch or a zero-connection reader is a broken
// reservation rather than an unbounded one, and it never refuses a repository:
// it serialises and defers the work instead.
func (c Config) validate() error {
	if c.Version != SchemaVersion {
		return configInvalid("version is %d; this build understands configuration version %d", c.Version, SchemaVersion)
	}
	// Reservations: positive, never "unlimited".
	for _, p := range []struct {
		key string
		v   int64
	}{
		{"index.max_parser_workers", int64(c.Index.MaxParserWorkers)},
		{"index.batch_records", int64(c.Index.BatchRecords)},
		{"index.batch_bytes", c.Index.BatchBytes},
		{"index.queue_bytes", c.Index.QueueBytes},
		{"index.watch_pending_paths", int64(c.Index.WatchPendingPaths)},
		{"index.watch_pending_bytes", c.Index.WatchPendingBytes},
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
		{"resources.max_page_items", int64(c.Resources.MaxPageItems)},
		{"storage.read_connections", int64(c.Storage.ReadConnections)},
		{"storage.writer_cache_kib", int64(c.Storage.WriterCacheKiB)},
		{"storage.reader_cache_kib", int64(c.Storage.ReaderCacheKiB)},
		{"storage.wal_high_water_bytes", c.Storage.WALHighWaterBytes},
		{"providers.lsp.max_servers", int64(c.Providers.LSP.MaxServers)},
		{"providers.lsp.max_outstanding_requests", int64(c.Providers.LSP.MaxOutstandingRequests)},
		{"providers.dependence.cache_bytes", c.Providers.Dependence.CacheBytes},
		{"providers.dependence.unit_memory_floor_bytes", c.Providers.Dependence.UnitMemoryFloorBytes},
		{"tools.max_fetch_bytes", c.Tools.MaxFetchBytes},
		{"context.default_estimated_tokens", c.Context.DefaultEstimatedTokens},
		{"context.default_max_bytes", c.Context.DefaultMaxBytes},
		{"context.default_max_files", int64(c.Context.DefaultMaxFiles)},
		{"context.max_slices", int64(c.Context.MaxSlices)},
		{"coverage.chunk_bytes", c.Coverage.ChunkBytes},
		{"coverage.max_chunk_bytes", c.Coverage.MaxChunkBytes},
		{"coverage.max_receipts_per_confirmation", int64(c.Coverage.MaxReceiptsPerConfirmation)},
	} {
		if p.v <= 0 {
			return configInvalid("%s is %d; a reservation sizes the work and must be positive", p.key, p.v)
		}
	}
	if c.Index.Workers < 0 {
		return configInvalid("index.workers is %d; use 0 to choose from available CPUs and reservations", c.Index.Workers)
	}
	// Bounds: 0 (or "unlimited") means unlimited and is the default. Only a
	// negative value is rejected; it is not a third meaning.
	for _, p := range []struct {
		key string
		v   Limit
	}{
		{"workspace.max_files", c.Workspace.MaxFiles},
		{"workspace.max_parse_file_bytes", c.Workspace.MaxParseFileBytes},
		{"workspace.max_search_file_bytes", c.Workspace.MaxSearchFileBytes},
		{"index.retain_refs", c.Index.RetainRefs},
		{"index.max_retained_bytes", c.Index.MaxRetainedBytes},
		{"resources.max_query_terms", c.Resources.MaxQueryTerms},
		{"resources.max_provider_record_bytes", c.Resources.MaxProviderRecordBytes},
		{"providers.lsp.max_overlay_bytes", c.Providers.LSP.MaxOverlayBytes},
		{"context.max_graph_depth", c.Context.MaxGraphDepth},
		{"context.max_visited_nodes", c.Context.MaxVisitedNodes},
		{"context.max_graph_edges", c.Context.MaxGraphEdges},
		{"context.max_reason_paths_per_entry", c.Context.MaxReasonPathsPerEntry},
		{"context.max_manifest_bytes", c.Context.MaxManifestBytes},
		{"context.max_capsule_bytes", c.Context.MaxCapsuleBytes},
		{"context.max_capsule_records_per_list", c.Context.MaxCapsuleRecordsPerList},
		{"context.max_capsule_coverage_files", c.Context.MaxCapsuleCoverageFiles},
		{"coverage.max_unconfirmed_chunks_per_session", c.Coverage.MaxUnconfirmedChunksPerSession},
		{"providers.dependence.max_units_per_family", c.Providers.Dependence.MaxUnitsPerFamily},
		{"providers.dependence.max_staged_rows", c.Providers.Dependence.MaxStagedRows},
	} {
		if p.v < 0 {
			return configInvalid("%s is %d; a bound is a positive value, or 0 (%q) for no bound at all",
				p.key, int64(p.v), unlimitedSpelling)
		}
	}
	// providers.dependence.unit_memory_ceiling_bytes is a reservation ceiling
	// whose 0 means "derive the allocation from the machine", not "unlimited",
	// so it is neither a Limit nor required to be positive.
	if v := c.Providers.Dependence.UnitMemoryCeilingBytes; v < 0 {
		return configInvalid("providers.dependence.unit_memory_ceiling_bytes is %d; use 0 for the machine-derived allocation", v)
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
		{"retention.blob_grace", c.Retention.BlobGrace},
		{"providers.tree_sitter.worker_idle_ttl", c.Providers.TreeSitter.WorkerIdleTTL},
		{"providers.scip.stall_timeout", c.Providers.SCIP.StallTimeout},
		{"providers.lsp.request_timeout", c.Providers.LSP.RequestTimeout},
		{"providers.lsp.idle_ttl", c.Providers.LSP.IdleTTL},
		{"providers.dependence.stall_timeout", c.Providers.Dependence.StallTimeout},
		{"tools.fetch_timeout", c.Tools.FetchTimeout},
		{"coverage.session_ttl", c.Coverage.SessionTTL},
	} {
		if d.v <= 0 {
			return configInvalid("%s is %s; a duration must be positive", d.key, d.v)
		}
	}
	// The two analysis timeouts are the exception: 0 means no wall-clock limit,
	// because a deadline that fails an analysis unit refuses a repository for
	// being large. The stall timeouts above are what catch a wedged subprocess.
	for _, d := range []struct {
		key string
		v   Duration
	}{
		{"providers.scip.timeout", c.Providers.SCIP.Timeout},
		{"providers.dependence.timeout", c.Providers.Dependence.Timeout},
	} {
		if d.v < 0 {
			return configInvalid("%s is %s; use 0 for no wall-clock limit", d.key, d.v)
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

// validateStructuralCeilings rejects a configured value larger than the
// contract ceiling internal/model enforces. Only the class-A pagination and
// class-B wire bounds are cross-checked here: those really are what a stored
// column or a single bounded response can hold, and they are not the bounds the
// scale posture removes. context.max_reason_paths_per_entry left this list when
// it became an unlimited-by-default bound -- a per-entry explanation count is a
// report threshold, not a wire ceiling, so model.MaxReasonPathsPerEntry no
// longer caps it.
func (c Config) validateStructuralCeilings() error {
	for _, p := range []struct {
		key      string
		v, limit int64
	}{
		{"resources.max_page_items", int64(c.Resources.MaxPageItems), model.MaxPageItems},
		{"resources.max_query_text_bytes", int64(c.Resources.MaxQueryTextBytes), model.MaxQueryTextBytes},
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
	if overlay := c.Providers.LSP.MaxOverlayBytes; !overlay.IsUnlimited() && overlay.Value() < c.Resources.MaxSourceResponseBytes {
		return configInvalid("providers.lsp.max_overlay_bytes %s is smaller than resources.max_source_response_bytes %d; one served source response must fit the overlay",
			overlay, c.Resources.MaxSourceResponseBytes)
	}
	if c.Resources.MaxMetadataResponseBytes >= c.Resources.MaxSourceResponseBytes {
		return configInvalid("resources.max_metadata_response_bytes %d is not smaller than the source budget %d; generic tools must stay under the smaller metadata limit",
			c.Resources.MaxMetadataResponseBytes, c.Resources.MaxSourceResponseBytes)
	}
	if c.Index.BatchBytes > c.Index.QueueBytes {
		return configInvalid("index.batch_bytes %d exceeds index.queue_bytes %d; a batch must fit the queue reservation",
			c.Index.BatchBytes, c.Index.QueueBytes)
	}
	// An unlimited record bound is not a record that must fit one batch: the
	// batch is a reservation the writer streams through, so the pairing only
	// binds when the user set a record ceiling.
	if c.Resources.MaxProviderRecordBytes.Exceeded(c.Index.BatchBytes) {
		return configInvalid("resources.max_provider_record_bytes %s exceeds index.batch_bytes %d; one record must fit one batch",
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
	// Both pairings are one-sided now: a caller budget can only be "more than
	// the bound" when a bound was set at all. Against an unlimited workspace or
	// an unlimited manifest there is nothing to exceed.
	if c.Workspace.MaxFiles.Exceeded(int64(c.Context.DefaultMaxFiles)) {
		return configInvalid("context.default_max_files %d exceeds workspace.max_files %s",
			c.Context.DefaultMaxFiles, c.Workspace.MaxFiles)
	}
	if c.Context.MaxManifestBytes.Exceeded(c.Context.DefaultMaxBytes) {
		return configInvalid("context.default_max_bytes %d exceeds context.max_manifest_bytes %s",
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
