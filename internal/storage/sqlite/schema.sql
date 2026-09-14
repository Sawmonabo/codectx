CREATE TABLE schema_meta (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    version INTEGER NOT NULL CHECK(version = 1),
    fingerprint TEXT NOT NULL
);
CREATE TABLE repositories (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    root_path TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE blobs (
    hash BLOB PRIMARY KEY CHECK(length(hash) = 32),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    state TEXT NOT NULL CHECK(state IN ('ready','quarantined','trash')),
    created_at TEXT NOT NULL
);
CREATE TABLE blob_blocks (
    blob_hash BLOB NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    block_index INTEGER NOT NULL CHECK(block_index >= 0),
    digest BLOB NOT NULL CHECK(length(digest) = 32),
    PRIMARY KEY(blob_hash, block_index)
) WITHOUT ROWID;
CREATE TABLE line_checkpoints (
    blob_hash BLOB NOT NULL REFERENCES blobs(hash) ON DELETE CASCADE,
    byte_offset INTEGER NOT NULL CHECK(byte_offset >= 0),
    line_number INTEGER NOT NULL CHECK(line_number >= 1),
    line_start_byte INTEGER NOT NULL CHECK(line_start_byte >= 0 AND line_start_byte <= byte_offset),
    PRIMARY KEY(blob_hash, byte_offset)
) WITHOUT ROWID;
CREATE TABLE files (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    path TEXT NOT NULL,
    UNIQUE(repository_id, path)
);
CREATE TABLE snapshots (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    head_object_id TEXT NOT NULL DEFAULT '',
    source_policy_hash TEXT NOT NULL,
    manifest_hash TEXT NOT NULL,
    file_count INTEGER NOT NULL CHECK(file_count >= 0),
    source_bytes INTEGER NOT NULL CHECK(source_bytes >= 0),
    capture_consistency TEXT NOT NULL CHECK(capture_consistency IN ('validated_capture','operator_frozen')),
    created_at TEXT NOT NULL,
    UNIQUE(repository_id, id)
);
CREATE TABLE snapshot_files (
    snapshot_id BLOB NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    file_id BLOB NOT NULL REFERENCES files(id),
    status TEXT NOT NULL CHECK(status IN ('tracked','modified','added','deleted','untracked')),
    content_hash BLOB REFERENCES blobs(hash),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0),
    git_object_id TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL DEFAULT '',
    executable INTEGER NOT NULL CHECK(executable IN (0,1)),
    PRIMARY KEY(snapshot_id, file_id),
    UNIQUE(snapshot_id, file_id, content_hash),
    CHECK((status = 'deleted' AND content_hash IS NULL AND size_bytes = 0)
       OR (status <> 'deleted' AND content_hash IS NOT NULL))
) WITHOUT ROWID;
-- generations.ref is the ref the generation was built from (Section 12.4):
-- retention keeps the last retain_refs DISTINCT refs the user actually
-- indexed, not the last N generations, which is what makes A -> B -> C -> A
-- find A's units still on disk. The value is the caller's: a branch name, the
-- HEAD object id when HEAD is detached, or the fixed sentinel '(none)' for a
-- workspace that is not a Git repository. Storage never interprets it, only
-- groups by it.
CREATE TABLE generations (
    id INTEGER PRIMARY KEY,
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    snapshot_id BLOB NOT NULL,
    ref TEXT NOT NULL CHECK(length(ref) > 0),
    analysis_key BLOB CHECK(analysis_key IS NULL OR length(analysis_key) = 32),
    semantic_config_hash TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('staging','active','superseded','failed')),
    health TEXT NOT NULL CHECK(health IN ('fresh','degraded','failed')),
    created_at TEXT NOT NULL,
    activated_at TEXT,
    UNIQUE(repository_id, id),
    UNIQUE(id, snapshot_id),
    FOREIGN KEY(repository_id, snapshot_id) REFERENCES snapshots(repository_id, id)
);
CREATE TABLE active_generations (
    repository_id BLOB PRIMARY KEY REFERENCES repositories(id),
    generation_id INTEGER NOT NULL,
    FOREIGN KEY(repository_id, generation_id) REFERENCES generations(repository_id, id)
);
CREATE TABLE provider_runs (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER REFERENCES generations(id) ON DELETE SET NULL,
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running','succeeded','partial','skipped','timed_out','failed','canceled')),
    counters_json TEXT NOT NULL DEFAULT '{}',
    diagnostic_code TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    completed_at TEXT
);
CREATE TABLE units (
    id INTEGER PRIMARY KEY,
    unit_key BLOB NOT NULL UNIQUE CHECK(length(unit_key) = 32),
    provider_id TEXT NOT NULL,
    provider_version TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    dependency_hash TEXT NOT NULL,
    origin_run_id BLOB NOT NULL REFERENCES provider_runs(id),
    state TEXT NOT NULL CHECK(state IN ('building','sealed','failed','quarantined')),
    source_binding TEXT NOT NULL CHECK(source_binding IN ('verified','unverified')),
    UNIQUE(id, provider_id, scope_key)
);
CREATE TABLE unit_inputs (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    file_id BLOB NOT NULL REFERENCES files(id),
    content_hash BLOB NOT NULL REFERENCES blobs(hash),
    executable INTEGER NOT NULL CHECK(executable IN (0,1)),
    PRIMARY KEY(unit_id, file_id)
) WITHOUT ROWID;
CREATE TABLE unit_dependencies (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    dependency_id INTEGER NOT NULL REFERENCES units(id),
    PRIMARY KEY(unit_id, dependency_id),
    CHECK(unit_id <> dependency_id)
) WITHOUT ROWID;
-- carried marks the Section 13.3 stale-with-distance member: the scope's
-- previous sealed unit, kept in the generation while its fresh rebuild is
-- still running. Such a unit's inputs differ from the generation's snapshot by
-- definition, so AttachCarried admits it where AttachUnit refuses, and the two
-- distance columns record how far behind it is (generations, and changed files
-- between its snapshot and this one). All three columns are folded into the
-- membership digest, and therefore into the AnalysisKey: without them a
-- generation carrying a stale unit would be byte-identical to one that rebuilt
-- it, and a stale answer would be indistinguishable from a fresh one.
CREATE TABLE generation_units (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    unit_id INTEGER NOT NULL,
    carried INTEGER NOT NULL CHECK(carried IN (0,1)),
    distance_generations INTEGER NOT NULL CHECK(distance_generations >= 0),
    distance_files INTEGER NOT NULL CHECK(distance_files >= 0),
    PRIMARY KEY(generation_id, provider_id, scope_key),
    UNIQUE(generation_id, unit_id),
    FOREIGN KEY(unit_id, provider_id, scope_key) REFERENCES units(id, provider_id, scope_key),
    CHECK(carried = 1 OR (distance_generations = 0 AND distance_files = 0))
) WITHOUT ROWID;
CREATE TABLE generation_capabilities (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    capability TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('fresh','partial','stale','unavailable','failed')),
    diagnostic_code TEXT NOT NULL DEFAULT '',
    details_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(generation_id, provider_id, capability, scope_key)
) WITHOUT ROWID;
CREATE TABLE node_ids (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    kind TEXT NOT NULL,
    canonical_key TEXT NOT NULL,
    UNIQUE(repository_id, kind, canonical_key)
);
CREATE TABLE node_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB NOT NULL REFERENCES node_ids(id),
    language TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte))
) WITHOUT ROWID;
CREATE TABLE relation_ids (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    repository_id BLOB NOT NULL REFERENCES repositories(id),
    from_node_id BLOB NOT NULL REFERENCES node_ids(id),
    kind TEXT NOT NULL,
    to_node_id BLOB NOT NULL REFERENCES node_ids(id),
    UNIQUE(repository_id, from_node_id, kind, to_node_id)
);
CREATE TABLE relation_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    relation_id BLOB NOT NULL REFERENCES relation_ids(id),
    PRIMARY KEY(unit_id, relation_id)
) WITHOUT ROWID;
-- fact_keys carries the producer's own id-independent delta keys for one fact
-- (Section 11.4). A fact is backed by every key that produced it, not by one:
-- a canonical edge is published once but is derived from N occurrences, each
-- with its own key, and a refresh re-emits the whole fact when any of them
-- changes. A carry-over therefore keeps a fact unless ANY of its keys is
-- replaced, which is the same condition the producer re-emits on, so the fresh
-- and carried sets partition exactly. A fact with no row here carries no key
-- and can only be replaced by its retention bucket.
CREATE TABLE fact_keys (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB,
    relation_id BLOB,
    fact_key TEXT NOT NULL,
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, relation_id) REFERENCES relation_facts(unit_id, relation_id),
    CHECK((node_id IS NOT NULL AND relation_id IS NULL) OR (node_id IS NULL AND relation_id IS NOT NULL))
);
CREATE TABLE evidence (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id BLOB,
    relation_id BLOB,
    precision TEXT NOT NULL CHECK(precision IN ('compiler','language_server','static_analysis','syntax','heuristic')),
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    native_key TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    content_hash_bound INTEGER NOT NULL DEFAULT 0 CHECK(content_hash_bound IN (0,1)),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, relation_id) REFERENCES relation_facts(unit_id, relation_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((node_id IS NOT NULL AND relation_id IS NULL) OR (node_id IS NULL AND relation_id IS NOT NULL)),
    CHECK(content_hash_bound = 0 OR file_id IS NOT NULL),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte))
);
CREATE TABLE unit_delta_state (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY(unit_id, kind)
) WITHOUT ROWID;
CREATE TABLE native_aliases (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    scope_key TEXT NOT NULL,
    native_key TEXT NOT NULL,
    node_id BLOB NOT NULL,
    PRIMARY KEY(unit_id, scope_key, native_key, node_id),
    FOREIGN KEY(node_id) REFERENCES node_ids(id)
) WITHOUT ROWID;
CREATE TABLE search_units (
    rowid INTEGER PRIMARY KEY,
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    search_key BLOB NOT NULL CHECK(length(search_key) = 32),
    node_id BLOB,
    file_id BLOB NOT NULL,
    path TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    body TEXT NOT NULL,
    token_count INTEGER NOT NULL CHECK(token_count >= 0),
    UNIQUE(unit_id, search_key),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id)
);
CREATE VIRTUAL TABLE search_fts USING fts5(
    name, qualified_name, signature, path, body,
    content='search_units', content_rowid='rowid', tokenize='unicode61', detail='full'
);
CREATE VIRTUAL TABLE search_vocab USING fts5vocab(search_fts, 'instance');
CREATE TABLE context_manifests (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER NOT NULL,
    snapshot_id BLOB NOT NULL,
    phase TEXT NOT NULL CHECK(phase IN ('sweep','verify','consolidate')),
    request_hash TEXT NOT NULL,
    policy_version TEXT NOT NULL,
    request_json TEXT NOT NULL,
    completeness_json TEXT NOT NULL,
    canonical_hash TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE(id, generation_id, snapshot_id),
    FOREIGN KEY(generation_id, snapshot_id) REFERENCES generations(id, snapshot_id)
);
CREATE TABLE context_entries (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    node_id BLOB REFERENCES node_ids(id),
    file_id BLOB REFERENCES files(id),
    requirement TEXT NOT NULL CHECK(requirement IN ('required_full','required_symbol','recommended','optional')),
    score_micros INTEGER NOT NULL,
    estimated_bytes INTEGER NOT NULL CHECK(estimated_bytes >= 0),
    estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens >= 0),
    reasons_json TEXT NOT NULL,
    evidence_paths_json TEXT NOT NULL,
    PRIMARY KEY(manifest_id, ordinal),
    CHECK(node_id IS NOT NULL OR file_id IS NOT NULL)
) WITHOUT ROWID;
CREATE TABLE context_slices (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    slice_index INTEGER NOT NULL CHECK(slice_index >= 0),
    estimated_bytes INTEGER NOT NULL CHECK(estimated_bytes >= 0),
    estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens >= 0),
    PRIMARY KEY(manifest_id, slice_index)
) WITHOUT ROWID;
CREATE TABLE context_slice_entries (
    manifest_id BLOB NOT NULL,
    slice_index INTEGER NOT NULL,
    position INTEGER NOT NULL CHECK(position >= 0),
    entry_ordinal INTEGER NOT NULL,
    PRIMARY KEY(manifest_id, slice_index, position),
    FOREIGN KEY(manifest_id, slice_index) REFERENCES context_slices(manifest_id, slice_index) ON DELETE CASCADE,
    FOREIGN KEY(manifest_id, entry_ordinal) REFERENCES context_entries(manifest_id, ordinal) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE excluded_context_entries (
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    reference_json TEXT NOT NULL,
    reason TEXT NOT NULL,
    PRIMARY KEY(manifest_id, ordinal)
) WITHOUT ROWID;
CREATE TABLE read_sessions (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    actor_id TEXT NOT NULL CHECK(length(trim(actor_id)) > 0),
    idempotency_key TEXT CHECK(idempotency_key IS NULL OR length(idempotency_key) > 0),
    open_request_hash TEXT NOT NULL,
    manifest_id BLOB NOT NULL,
    generation_id INTEGER NOT NULL,
    snapshot_id BLOB NOT NULL,
    workflow_state TEXT NOT NULL CHECK(workflow_state IN ('sweep_open','verify_open','consolidate_open','complete','closed')),
    state_version INTEGER NOT NULL CHECK(state_version >= 1),
    scope_version INTEGER NOT NULL CHECK(scope_version >= 1),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    closed_at TEXT,
    UNIQUE(id, snapshot_id),
    UNIQUE(actor_id, idempotency_key),
    FOREIGN KEY(manifest_id, generation_id, snapshot_id)
        REFERENCES context_manifests(id, generation_id, snapshot_id)
);
CREATE TABLE session_manifests (
    session_id BLOB NOT NULL REFERENCES read_sessions(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    manifest_id BLOB NOT NULL REFERENCES context_manifests(id),
    phase TEXT NOT NULL CHECK(phase IN ('sweep','verify','consolidate')),
    activated_at TEXT NOT NULL,
    PRIMARY KEY(session_id, ordinal)
) WITHOUT ROWID;
CREATE TABLE session_files (
    session_id BLOB NOT NULL,
    snapshot_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    requirement TEXT NOT NULL CHECK(requirement IN ('required_full','required_symbol','recommended','optional')),
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, snapshot_id) REFERENCES read_sessions(id, snapshot_id) ON DELETE CASCADE,
    FOREIGN KEY(snapshot_id, file_id, content_hash) REFERENCES snapshot_files(snapshot_id, file_id, content_hash)
) WITHOUT ROWID;
CREATE TABLE issued_chunks (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    expires_at TEXT NOT NULL,
    confirmed_at TEXT,
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
);
CREATE TABLE served_ranges (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte > start_byte),
    PRIMARY KEY(session_id, file_id, content_hash, start_byte, end_byte),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE source_acknowledgements (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    acknowledged_at TEXT NOT NULL,
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE coverage_waivers (
    session_id BLOB NOT NULL,
    file_id BLOB NOT NULL,
    content_hash BLOB NOT NULL,
    reason TEXT NOT NULL CHECK(length(trim(reason)) > 0),
    created_at TEXT NOT NULL,
    PRIMARY KEY(session_id, file_id, content_hash),
    FOREIGN KEY(session_id, file_id, content_hash)
        REFERENCES session_files(session_id, file_id, content_hash) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE session_observations (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    session_id BLOB NOT NULL REFERENCES read_sessions(id) ON DELETE CASCADE,
    scope_version INTEGER NOT NULL CHECK(scope_version >= 1),
    kind TEXT NOT NULL CHECK(kind IN ('accept_fact','reject_fact','contradiction','unresolved','scope_review')),
    references_json TEXT NOT NULL,
    note TEXT NOT NULL CHECK(length(trim(note)) > 0),
    created_at TEXT NOT NULL
);
CREATE TABLE context_capsules (
    session_id BLOB PRIMARY KEY REFERENCES read_sessions(id) ON DELETE CASCADE,
    canonical_hash TEXT NOT NULL,
    capsule_json TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE TABLE retention_leases (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    generation_id INTEGER REFERENCES generations(id),
    snapshot_id BLOB REFERENCES snapshots(id),
    owner_kind TEXT NOT NULL CHECK(owner_kind IN ('query','cursor','session','staging')),
    expires_at TEXT NOT NULL,
    CHECK(generation_id IS NOT NULL OR snapshot_id IS NOT NULL)
);
CREATE INDEX idx_generation_snapshot ON generations(snapshot_id);
-- Retention ranks a repository's refs by the most recent generation each was
-- activated for, then sweeps everything below the retain_refs cut.
CREATE INDEX idx_generation_ref ON generations(repository_id, ref, activated_at);
CREATE INDEX idx_generation_units_unit ON generation_units(unit_id, generation_id);
CREATE INDEX idx_unit_input_file ON unit_inputs(file_id, unit_id);
CREATE INDEX idx_unit_dependencies_reverse ON unit_dependencies(dependency_id, unit_id);
CREATE INDEX idx_nodes_name ON node_facts(name, unit_id, node_id);
CREATE INDEX idx_nodes_qname ON node_facts(qualified_name, unit_id, node_id);
CREATE INDEX idx_nodes_file ON node_facts(file_id, start_byte, unit_id);
CREATE INDEX idx_node_facts_id ON node_facts(node_id, unit_id);
CREATE INDEX idx_relations_from ON relation_ids(from_node_id, kind, to_node_id);
CREATE INDEX idx_relations_to ON relation_ids(to_node_id, kind, from_node_id);
CREATE INDEX idx_relation_facts_id ON relation_facts(relation_id, unit_id);
CREATE UNIQUE INDEX idx_fact_keys ON fact_keys(unit_id, fact_key, coalesce(node_id, x''), coalesce(relation_id, x''));
CREATE INDEX idx_fact_keys_node ON fact_keys(unit_id, node_id);
CREATE INDEX idx_fact_keys_relation ON fact_keys(unit_id, relation_id);
CREATE INDEX idx_evidence_unit ON evidence(unit_id, id);
CREATE INDEX idx_evidence_node ON evidence(node_id, unit_id);
CREATE INDEX idx_evidence_relation ON evidence(relation_id, unit_id);
CREATE INDEX idx_alias_lookup ON native_aliases(scope_key, native_key, unit_id);
-- The identity sweep in unit deletion asks, per candidate node, whether any
-- alias still points at it. Without this index that question is a full scan of
-- native_aliases per node, which is quadratic in the size of the unit being
-- deleted; the same holds for a manifest entry's node.
CREATE INDEX idx_alias_node ON native_aliases(node_id, unit_id);
CREATE INDEX idx_context_entries_node ON context_entries(node_id);
CREATE INDEX idx_search_unit ON search_units(unit_id, rowid);
CREATE INDEX idx_session_expiry ON read_sessions(expires_at, workflow_state);
CREATE INDEX idx_issued_session ON issued_chunks(session_id, confirmed_at, expires_at);
CREATE INDEX idx_observations_session ON session_observations(session_id, scope_version, kind);
CREATE INDEX idx_lease_expiry ON retention_leases(expires_at);
