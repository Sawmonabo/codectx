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
    created_at TEXT NOT NULL,
    -- When the Section 10.4 collector demoted this blob out of 'ready'. The
    -- grace window before the second reachability check and the deletion is
    -- measured from it, so a restore that flips the state back to 'ready'
    -- clears it: a ready blob is never mid-grace.
    trashed_at TEXT CHECK(trashed_at IS NULL OR state <> 'ready')
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
    c_units INTEGER NOT NULL CHECK(c_units >= 0),
    cpp_units INTEGER NOT NULL CHECK(cpp_units >= 0),
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
    -- failure_json is the typed reason a run produced no unit: its scope key,
    -- message and bounded details, including the tool output the capability
    -- row and the log line both exclude. It is empty for every run that did
    -- not fail. It lives on the run rather than on the unit because a failed
    -- unit has no sealed row to carry it, and it is reachable for exactly as
    -- long as the generation that failed is retained: deleting that
    -- generation nulls generation_id, and the sweep then removes the run.
    failure_json TEXT NOT NULL DEFAULT '',
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
    -- The unit's pending mark: how many search documents it added to the
    -- search index that are not yet known to be in a library commit. Its
    -- batches add to it; the activation that publishes the unit, after the
    -- library commit, clears it. Recover compares a nonzero mark with what the
    -- library's last commit holds and fails a unit that differs, so a unit
    -- whose documents a crash lost is rebuilt (ADR-0013 Decision 4).
    search_pending INTEGER NOT NULL DEFAULT 0 CHECK(search_pending >= 0),
    UNIQUE(id, provider_id, scope_key)
);
CREATE TABLE unit_inputs (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    file_id BLOB NOT NULL REFERENCES files(id),
    content_hash BLOB NOT NULL REFERENCES blobs(hash),
    executable INTEGER NOT NULL CHECK(executable IN (0,1)),
    -- digest is the digest of the table another unit derived from the file
    -- that this unit read in place of its bytes (model.UnitInput.Digest): for
    -- a linking unit, the file's link tables. NULL for an input read as bytes.
    digest BLOB CHECK(digest IS NULL OR length(digest) = 32),
    PRIMARY KEY(unit_id, file_id)
) WITHOUT ROWID;
CREATE TABLE unit_dependencies (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    dependency_id INTEGER NOT NULL REFERENCES units(id),
    PRIMARY KEY(unit_id, dependency_id),
    CHECK(unit_id <> dependency_id)
) WITHOUT ROWID;
CREATE TABLE generation_units (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    unit_id INTEGER NOT NULL,
    PRIMARY KEY(generation_id, provider_id, scope_key),
    UNIQUE(generation_id, unit_id),
    FOREIGN KEY(unit_id, provider_id, scope_key) REFERENCES units(id, provider_id, scope_key)
) WITHOUT ROWID;
CREATE TABLE generation_capabilities (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    capability TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('fresh','partial','stale','unavailable','failed')),
    diagnostic_code TEXT NOT NULL DEFAULT '',
    -- remediation is what an operator can do about this row, in prose. It is a
    -- column rather than an entry in details_json because details_json is the
    -- provider's own bounded budget: a remediation charged to it could be the
    -- entry a busy row drops.
    remediation TEXT NOT NULL DEFAULT '',
    details_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(generation_id, provider_id, capability, scope_key)
) WITHOUT ROWID;
-- unit_capabilities is what one sealed unit reported about its own scope: the
-- capability rows of its provider result, written in the transaction that
-- seals it. A generation that reuses the unit folds these into its report
-- exactly as it folds the rows of a unit it builds, so a reused unit's counts
-- and degradations are never missing from a generation it is a member of.
-- One row per capability the unit reported; the key refuses a second.
CREATE TABLE unit_capabilities (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    capability TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK(state IN ('fresh','partial','stale','unavailable','failed')),
    diagnostic_code TEXT NOT NULL DEFAULT '',
    remediation TEXT NOT NULL DEFAULT '',
    details_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(unit_id, provider_id, capability, scope_key)
) WITHOUT ROWID;
-- node_ids is the node identity dictionary (Section 12.2).
-- id is a storage-internal surrogate: an INTEGER PRIMARY KEY is the b-tree key
-- itself, so it costs zero payload bytes in the table and 1-4 varint bytes
-- wherever it is referenced, against ~33 B for a 32-byte BLOB reference.
-- The surrogate NEVER leaves the store: model.NodeID on the wire, in cursors
-- and in every canonical hash is `canonical`, which is stored exactly once.
-- repository_id is dropped: one store is one repository (schema.sql repositories).
CREATE TABLE node_ids (
    id INTEGER PRIMARY KEY CHECK(id > 0),
    canonical BLOB NOT NULL UNIQUE CHECK(length(canonical) = 32),
    kind TEXT NOT NULL,
    canonical_key BLOB NOT NULL CHECK(length(canonical_key) = 32),
    UNIQUE(kind, canonical_key)
);
CREATE TABLE node_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id INTEGER NOT NULL REFERENCES node_ids(id),
    language TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL,
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    -- The declaration's name token, inside [start_byte, end_byte); NULL for a
    -- node with no name token of its own.
    name_start_byte INTEGER,
    name_end_byte INTEGER,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte)),
    CHECK((name_start_byte IS NULL AND name_end_byte IS NULL)
       OR (start_byte IS NOT NULL AND name_start_byte IS NOT NULL AND name_end_byte IS NOT NULL
           AND name_start_byte >= start_byte AND name_end_byte >= name_start_byte AND name_end_byte <= end_byte))
) WITHOUT ROWID;
-- relation_ids mirrors node_ids: an INTEGER surrogate, the canonical 32-byte
-- RelationID stored once in `canonical`, and the endpoints carried as node
-- surrogates.
CREATE TABLE relation_ids (
    id INTEGER PRIMARY KEY CHECK(id > 0),
    canonical BLOB NOT NULL UNIQUE CHECK(length(canonical) = 32),
    from_node_id INTEGER NOT NULL REFERENCES node_ids(id),
    kind TEXT NOT NULL,
    to_node_id INTEGER NOT NULL REFERENCES node_ids(id),
    UNIQUE(from_node_id, kind, to_node_id)
);
CREATE TABLE relation_facts (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    relation_id INTEGER NOT NULL REFERENCES relation_ids(id),
    PRIMARY KEY(unit_id, relation_id)
) WITHOUT ROWID;
-- evidence.id stays a PRIMARY KEY, refused by call site rather than by size.
-- insertEvidence and copyEvidence upsert with ON CONFLICT(id) DO NOTHING,
-- which SQLite only accepts against a PRIMARY KEY or UNIQUE index, and
-- clipEvidence clips surplus evidence BY id value. The id is a pure function of the evidence
-- fields (model.NewEvidenceID), so this uniqueness is a derived invariant, not
-- an arbitrary surrogate, and the value reaches the wire as
-- ReferenceOccurrence.evidence_id and FactReference.evidence_ids. It is the
-- first 16 bytes of that derivation (model.EvidenceIDBytes, ADR-0003 SS2.2):
-- the width is paid in the record, in the primary key's automatic index and
-- in idx_evidence_unit, the three b-trees that carry it.
CREATE TABLE evidence (
    id BLOB PRIMARY KEY CHECK(length(id) = 16),
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id INTEGER,
    relation_id INTEGER,
    precision TEXT NOT NULL CHECK(precision IN ('compiler','language_server','static_analysis','syntax','heuristic')),
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    native_key_id INTEGER NOT NULL REFERENCES native_keys(id),
    detail TEXT NOT NULL DEFAULT '',
    dispatch TEXT NOT NULL DEFAULT '' CHECK(dispatch IN ('', 'virtual', 'exact')),
    set_size INTEGER NOT NULL DEFAULT 0 CHECK(set_size = 0 OR set_size >= 2),
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
    part INTEGER NOT NULL,
    payload BLOB NOT NULL,
    PRIMARY KEY(unit_id, kind, part)
) WITHOUT ROWID;
-- Intern dictionaries. A WITHOUT ROWID table re-stores its whole primary
-- key inside every secondary index, so a wide PK is paid for once per index.
-- Interning replaces the wide text columns with surrogates so that the PK, and
-- therefore each index built over it, is a handful of varints.
-- Measured on a control fixture store: 274 807 alias rows carry only 403 distinct
-- scope keys (avg 34 B) and 216 385 distinct native keys (avg 52 B), each
-- replicated three times (table + idx_alias_lookup + idx_alias_node); evidence
-- adds 477 388 native-key copies drawn from the same 52 711-value vocabulary.
-- The writer resolves these through a bounded LRU flushed with each batch
-- (ids.go) -- never a whole-repository dictionary in heap.
CREATE TABLE scope_keys (
    id INTEGER PRIMARY KEY CHECK(id > 0),
    key TEXT NOT NULL UNIQUE
);
-- native_keys is a HASH-KEYED dictionary (ADR-0003 SS2.3). `key TEXT UNIQUE`
-- built an automatic index that cost more than the table it indexed -- 38.8 MB
-- against 34.5 MB on a large Python repository, 47.1 against 41.2 on promptfoo -- because
-- interning stored every distinct key twice: once as payload, once as the
-- index key. Declaring the table WITHOUT ROWID PRIMARY KEY(key) does not fix
-- it: both directions are needed (the writer resolves key->id, every reader
-- id->key), so an id-keyed index survives, and a rowid-less table re-stores its
-- whole primary key inside every secondary index, putting the full key back.
-- Instead the id IS the key's digest -- the first 63 bits of SHA-256(key), zero
-- clamped to 1 so that CHECK(id > 0) and the noRef sentinel both hold -- so the
-- writer computes it without a lookup and one b-tree holds one copy of each
-- key. Safety is DETECTION, not improbability: intern.go compares the stored
-- key against the incoming one and probes the next id on disagreement, reusing
-- the read-back the interner already performs, so two colliding keys can never
-- merge into one id. scope_keys keeps its UNIQUE index: 403 distinct values on
-- the control fixture is not an amplifier, and reconcile's scope lookup is by
-- key.
CREATE TABLE native_keys (
    id INTEGER PRIMARY KEY CHECK(id > 0),
    key TEXT NOT NULL
);
-- No `paths` intern table: files.path is 818 rows / 114 KiB on that same
-- fixture and is not an amplifier, and a search document's path is stored in
-- the search index, not here.
CREATE TABLE native_aliases (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    scope_key_id INTEGER NOT NULL REFERENCES scope_keys(id),
    native_key_id INTEGER NOT NULL REFERENCES native_keys(id),
    node_id INTEGER NOT NULL REFERENCES node_ids(id),
    PRIMARY KEY(unit_id, scope_key_id, native_key_id, node_id)
) WITHOUT ROWID;
-- One row per search document of a unit: its identity, from search key to
-- node, file and byte range. The indexed text -- path, kind, name, qualified
-- name, signature and body -- lives only in the search index beside this
-- database (ADR-0013), which stores what a search serves.
CREATE TABLE search_units (
    rowid INTEGER PRIMARY KEY,
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    search_key BLOB NOT NULL CHECK(length(search_key) = 32),
    node_id INTEGER,
    file_id BLOB NOT NULL,
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    -- The search index document holding this row's text, allocated from
    -- search_sequence when the row is written and never reused. It is the
    -- DOCUMENT's identity, not the row's: a delta carry-over copies it forward
    -- with the rest of the row, so the carried unit names the document its
    -- text was indexed into instead of re-deriving a body the producer never
    -- published (a producer's Body is not the source over
    -- [start_byte,end_byte) -- see internal/provider/treesitter/facts.go).
    -- Sharing is confined to one carry chain: CarryOver refuses a predecessor
    -- of another provider or scope key, and generation_units is keyed by
    -- (generation_id, provider_id, scope_key), so at most one row of a chain
    -- is visible in any generation and a doc_id names one document there. The
    -- document is released like node_ids: deleteUnit queues its delete only
    -- when no other unit's row still names it.
    doc_id INTEGER NOT NULL CHECK(doc_id > 0),
    UNIQUE(unit_id, search_key),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id)
);
-- The next unallocated search document id. A batch of search documents takes
-- its ids from here inside the transaction that writes their rows, so an id is
-- durable exactly when its row is, and a crash that loses the rows also gives
-- the ids back: Recover deletes every search index document at or past this
-- value, which only a lost transaction can have added. Collection never
-- lowers it, so a document id is never reused.
CREATE TABLE search_sequence (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    next_doc_id INTEGER NOT NULL CHECK(next_doc_id > 0)
);
-- Search index documents that collection released and the search index has
-- not yet deleted. deleteUnit queues them in the transaction that removes the
-- rows naming them; the next library commit (an activation's, a collection
-- sweep's or Recover's) applies them, and the rows it applied go in the
-- SQLite transaction that follows it.
CREATE TABLE search_doc_deletes (
    rowid INTEGER PRIMARY KEY,
    doc_id INTEGER NOT NULL CHECK(doc_id > 0)
);
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
    node_id INTEGER REFERENCES node_ids(id),
    file_id BLOB REFERENCES files(id),
    requirement TEXT NOT NULL CHECK(requirement IN ('required_full','required_symbol','recommended','optional')),
    score_micros INTEGER NOT NULL,
    estimated_bytes INTEGER NOT NULL CHECK(estimated_bytes >= 0),
    estimated_tokens INTEGER NOT NULL CHECK(estimated_tokens >= 0),
    reasons_json TEXT NOT NULL,
    route_count INTEGER NOT NULL CHECK(route_count >= 0),
    PRIMARY KEY(manifest_id, ordinal),
    CHECK(node_id IS NOT NULL OR file_id IS NOT NULL)
) WITHOUT ROWID;
CREATE TABLE context_entry_routes (
    manifest_id BLOB NOT NULL,
    ordinal INTEGER NOT NULL,
    route_index INTEGER NOT NULL CHECK(route_index >= 0),
    relations BLOB NOT NULL CHECK(length(relations) > 0 AND length(relations) % 32 = 0),
    PRIMARY KEY(manifest_id, ordinal, route_index),
    FOREIGN KEY(manifest_id, ordinal) REFERENCES context_entries(manifest_id, ordinal) ON DELETE CASCADE
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
    -- The owner this lease was taken for, so a lease can be found from its
    -- owner and released when the owner ends. Lease identifiers otherwise flow
    -- one way only, which is why a closed session's lease survived it and an
    -- idempotent re-open minted a second one. It is NULL for an owner that has
    -- no stable identity of its own, such as a staging lease.
    owner_ref BLOB CHECK(owner_ref IS NULL OR length(owner_ref) = 32),
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
-- idx_nodes_file is narrowed to the one column it probes. start_byte was a
-- dead middle column: search.go keysets on coalesce(nf.start_byte, 0), which no
-- plain column index can serve, and unit_id is already the leading column of
-- the PRIMARY KEY suffix every index key carries. Re-audited on a rebuilt
-- store: both consumers keep their plan, gc.go's file probe stays COVERING,
-- and the index is 20.5% smaller.
CREATE INDEX idx_nodes_file ON node_facts(file_id);
CREATE INDEX idx_node_facts_id ON node_facts(node_id, unit_id);
-- No idx_relations_from: dropping the constant repository_id leaves
-- relation_ids with UNIQUE(from_node_id, kind, to_node_id), whose autoindex is
-- column-for-column what idx_relations_from would be. Its call sites (query.go:380,
-- adjacency.go:194, gc.go:196) keep an identical access path. idx_relations_to
-- stays: nothing else on relation_ids leads with to_node_id, and it is the
-- reverse-traversal ("who calls X") path.
CREATE INDEX idx_relations_to ON relation_ids(to_node_id, kind, from_node_id);
CREATE INDEX idx_relation_facts_id ON relation_facts(relation_id, unit_id);
CREATE INDEX idx_evidence_unit ON evidence(unit_id, id);
CREATE INDEX idx_evidence_node ON evidence(node_id, unit_id);
CREATE INDEX idx_evidence_relation ON evidence(relation_id, unit_id);
CREATE INDEX idx_alias_lookup ON native_aliases(scope_key_id, native_key_id, unit_id);
-- The identity sweep in unit deletion asks, per candidate node, whether any
-- alias still points at it. Without this index that question is a full scan of
-- native_aliases per node, which is quadratic in the size of the unit being
-- deleted; the same holds for a manifest entry's node.
CREATE INDEX idx_alias_node ON native_aliases(node_id, unit_id);
-- The two native-key child columns, indexed so the dictionary can be swept.
-- native_keys is the PARENT of both foreign keys below, and every connection
-- runs with foreign_keys=ON, so SQLite must prove no child row survives before
-- it may delete a dictionary row. Without an index on the child column that
-- proof is a full scan of the child table PER DELETED ROW -- measured on a
-- control fixture store (274 807 alias rows, 477 388 evidence rows), ~27 ms
-- per deleted key, i.e. ~90 minutes to clear a full-vocabulary churn of
-- ~201 k keys. That cost is charged to any DELETE on native_keys and cannot be
-- rephrased away; it is also invisible to EXPLAIN QUERY PLAN, which reports
-- only the collector's own probe. With these two indexes the foreign-key check
-- and the collector's two NOT EXISTS probes are all index lookups, and the
-- whole sweep finishes in one run.
-- The trade-off, measured on the same store: the two indexes hold 6 799 360 B
-- and 7 352 320 B, together 14 151 680 B = 2.24 % of a 632 266 752 B store,
-- the second of them on the largest table there. That is the price of a
-- dictionary that can be collected at all: without the sweep, native_keys plus
-- its UNIQUE autoindex strands ~182 B per distinct key on every re-index,
-- monotonically and with no bound (~73 MB per full-vocabulary churn), which
-- overtakes the indexes' fixed share after the second rebuild.
CREATE INDEX idx_alias_native ON native_aliases(native_key_id);
CREATE INDEX idx_evidence_native ON evidence(native_key_id);
CREATE INDEX idx_context_entries_node ON context_entries(node_id);
-- The file arm of the same sweep: gc.go's files collection asks, per candidate
-- file, whether any manifest entry still names it. context_entries carries
-- node_id and file_id side by side and only node_id was indexed, so that arm
-- was a full scan of context_entries per candidate file -- exactly the
-- pathology idx_context_entries_node exists to prevent, on the sibling column.
CREATE INDEX idx_context_entries_file ON context_entries(file_id);
-- An activation reads every member unit's doc ids to build the generation's
-- visible set; carrying doc_id makes that walk index-only.
CREATE INDEX idx_search_unit ON search_units(unit_id, doc_id);
-- deleteUnit releases a document only when no surviving unit's row still
-- names it, and Recover closes a failed unit over the units sharing its doc
-- ids. Without this index both are a scan of search_units per probe.
CREATE INDEX idx_search_doc ON search_units(doc_id);
CREATE INDEX idx_session_expiry ON read_sessions(expires_at, workflow_state);
CREATE INDEX idx_issued_session ON issued_chunks(session_id, confirmed_at, expires_at);
CREATE INDEX idx_observations_session ON session_observations(session_id, scope_version, kind);
CREATE INDEX idx_lease_expiry ON retention_leases(expires_at);
-- One live lease per owner, enforced by the schema rather than by the caller
-- remembering to look first: an idempotent re-open that acquires again is
-- refused instead of pinning a generation twice. The partial index leaves
-- owner-less leases unconstrained.
CREATE UNIQUE INDEX idx_lease_owner ON retention_leases(owner_kind, owner_ref) WHERE owner_ref IS NOT NULL;
-- The grace pass asks for the trashed blobs whose window has elapsed; without
-- this index that question is a full scan of blobs on every collection pass.
CREATE INDEX idx_blob_trash ON blobs(state, trashed_at);
-- Foreign-key child keys. Every connection runs with foreign_keys=ON, so a
-- DELETE of a parent row first proves no child row still names it, and with
-- the child column unindexed that proof is a full scan of the child table PER
-- DELETED ROW -- invisible to the statement's own plan. The same columns are
-- what the collector's reachability probes ask about. Each index below turns
-- one such scan into a lookup:
--   snapshot_files(file_id)        the orphan-file sweep and every files delete
--   snapshot_files(content_hash)   blob reachability and every blobs delete
--   unit_inputs(content_hash)      the same, for the unit side
--   session_files(snapshot_id, file_id, content_hash)
--                                  the snapshot_files cascade of a snapshot delete
--   retention_leases(snapshot_id), retention_leases(generation_id)
--                                  every snapshot and generation delete
--   provider_runs(generation_id)   the ON DELETE SET NULL of a generation delete
--   units(origin_run_id)           the provider-run sweep and every run delete
--   context_manifests(generation_id, snapshot_id)
--                                  every generation delete
--   read_sessions(manifest_id, generation_id, snapshot_id), session_manifests(manifest_id)
--                                  every context-manifest delete
-- The write cost is one key per child row, the largest being unit_inputs and
-- snapshot_files: about 70 bytes per file per unit input or per retained
-- snapshot, against a scan of the whole table per deletion without it.
CREATE INDEX idx_snapshot_files_file ON snapshot_files(file_id);
CREATE INDEX idx_snapshot_files_blob ON snapshot_files(content_hash);
CREATE INDEX idx_unit_input_blob ON unit_inputs(content_hash);
CREATE INDEX idx_session_files_snapshot ON session_files(snapshot_id, file_id, content_hash);
CREATE INDEX idx_lease_snapshot ON retention_leases(snapshot_id);
CREATE INDEX idx_lease_generation ON retention_leases(generation_id);
CREATE INDEX idx_provider_runs_generation ON provider_runs(generation_id);
CREATE INDEX idx_units_origin_run ON units(origin_run_id);
CREATE INDEX idx_context_manifests_generation ON context_manifests(generation_id, snapshot_id);
CREATE INDEX idx_read_sessions_manifest ON read_sessions(manifest_id, generation_id, snapshot_id);
CREATE INDEX idx_session_manifests_manifest ON session_manifests(manifest_id);
-- The supplied `--scip-index` paths one generation was built with, recorded
-- whether or not the path resolved to anything. An unresolved path leaves no
-- unit behind, so the absence of a unit is precisely the observation this table
-- exists to preserve: without the row, "no index was supplied" and "the
-- supplied path matched nothing" are indistinguishable after the fact, and the
-- second silently ships a repository with no cross-file symbols. The path is
-- root-relative by construction, so the row discloses no private absolute root.
CREATE TABLE generation_supplied_indexes (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    path TEXT NOT NULL CHECK(length(path) > 0),
    resolved INTEGER NOT NULL CHECK(resolved IN (0, 1)),
    PRIMARY KEY(generation_id, path)
);
-- The live watch heartbeats of one repository: one row per watching process,
-- published where a second process can read them. Without these rows, a
-- reporting process has no cross-process source for the pending-event count and
-- no way to say whether a watch is running at all.
--
-- The key is the repository AND the writer's own session identity, because a
-- workspace can be watched by more than one process at once -- a `codectx
-- watch` in a terminal and an editor's server, say -- and one row per
-- repository would have each one overwrite the other's claim, so whichever beat
-- last would be the only watch anybody could see. `session_id` is minted once
-- per watch, from the same random-identifier source every other operational id
-- uses, and not the pid: an operating system reuses a pid, so a row keyed by
-- one could be claimed by an unrelated process that inherited the number.
--
-- `expires_at` is written by the watcher itself, not derived by the reader from
-- configuration: a reader whose `[index]` block differs from the watcher's --
-- another checkout, another user, an edited file since the watch started --
-- would otherwise compute a liveness window the writer never promised. The
-- writer states its own deadline and refreshes it while it lives, so an expired
-- row means the process that owned it stopped refreshing, which is the only
-- signal of process death that works on every platform this product supports
-- (a pid can be reused, and probing one is neither portable nor race-free). It
-- is the same rule the run ledger states a run's liveness by, and a second rule
-- for whether a process is still there would drift from it.
--
-- `beat_at` is when the writer last published this row, and is reported beside
-- the watcher rather than judged: liveness is `expires_at` and nothing else.
-- Deriving one from the other would be the reader recomputing the writer's
-- window, which is exactly what `expires_at` exists to prevent.
--
-- `pending_events` and `last_pass_at` are nullable because absent and zero are
-- different answers here as everywhere else: a watch driven only by periodic
-- reconciliation has no notification queue to count, and a watch that has not
-- completed a pass yet has no pass time -- neither is "0 pending" or "the epoch".
--
-- One row per watching process, replaced in place: this is liveness, not
-- history. A row whose writer died is deleted by the next beat published for
-- the repository, by the same elapsed-stamp rule a reader judges it by.
CREATE TABLE watch_heartbeat (
    repository_id BLOB NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    session_id BLOB NOT NULL CHECK(length(session_id) = 32),
    writer_pid INTEGER NOT NULL CHECK(writer_pid > 0),
    beat_at TEXT NOT NULL,
    last_pass_at TEXT,
    pending_events INTEGER CHECK(pending_events IS NULL OR pending_events >= 0),
    expires_at TEXT NOT NULL,
    PRIMARY KEY(repository_id, session_id)
);
-- A sealed capsule's record lists (Section 17.3). context_capsules carries the
-- capsule's identity and its per-list counts; the records themselves live here,
-- one row each, so sealing a session that observed a repository-sized record
-- set writes in bounded batches and reading one back is a keyset page rather
-- than a single JSON blob that must be held whole.
--
-- The primary key IS the read order: a page is one (session_id, list) range
-- scanned by ordinal, which is the list's canonical order, so no sort is
-- needed and a continuation resumes at an ordinal instead of re-reading.
-- row_key is the record's own cursor key, unique within a (session_id, list);
-- the index on it turns a caller's cursor into the ordinal to resume from, and
-- a cursor naming no row is refused rather than silently restarting the list.
-- The rows are written inside the same transaction that inserts the capsule, so
-- a capsule never exists without its records and the seal stays write-once.
CREATE TABLE context_capsule_rows (
    session_id BLOB NOT NULL REFERENCES context_capsules(session_id) ON DELETE CASCADE,
    list TEXT NOT NULL CHECK(list IN ('scope','accepted_facts','rejected_facts','contradictions',
        'unresolved','scope_review_ids','coverage','waivers')),
    ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
    row_key TEXT NOT NULL CHECK(length(row_key) > 0),
    row_json TEXT NOT NULL,
    PRIMARY KEY(session_id, list, ordinal)
) WITHOUT ROWID;
-- The coverage and waiver lists key their row on FileID alone, which this index
-- would reject a second time. That state is unreachable: a session pins exactly
-- one snapshot (read_sessions.id, snapshot_id is what session_files' foreign
-- key names, and nothing updates snapshot_id), snapshot_files is keyed
-- (snapshot_id, file_id), so one file has exactly one content hash inside one
-- session -- and both session_files and coverage_waivers are keyed
-- (session_id, file_id, content_hash). One file therefore yields at most one
-- coverage row and at most one waiver per session.
CREATE UNIQUE INDEX idx_capsule_row_key ON context_capsule_rows(session_id, list, row_key);

-- The packed per-generation adjacency (ADR-0005 Decision 1). A generation's
-- walk reads structure from here and from nowhere else: visibility is resolved
-- once, at activation, instead of once per candidate edge per page, and the
-- edge stream is scanned sequentially rather than descended as a b-tree.
--
-- It is derived from facts that are already sealed, so it is neither a fact nor
-- an identity: no provider version and no analysis fingerprint of its own. It
-- is dropped with the generation it belongs to.
--
-- generation_graph is written LAST, after every part, so its presence is the
-- commit marker: a reader that finds the row is guaranteed every part behind it.
CREATE TABLE generation_graph (
    generation_id INTEGER PRIMARY KEY REFERENCES generations(id) ON DELETE CASCADE,
    max_node INTEGER NOT NULL CHECK(max_node >= 0),
    max_relation INTEGER NOT NULL CHECK(max_relation >= 0),
    node_count INTEGER NOT NULL CHECK(node_count >= 0),
    edge_count INTEGER NOT NULL CHECK(edge_count >= 0),
    kinds BLOB NOT NULL,
    node_kinds BLOB NOT NULL
);
-- One chunk of one stream. The streams are the two directions' offset
-- directories and edge streams plus the per-node and per-relation side arrays;
-- `part` is 0-based and the parts of a stream concatenate to it, so a reader
-- holds a bounded window of parts rather than a whole direction.
CREATE TABLE generation_graph_parts (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    stream TEXT NOT NULL CHECK(stream IN ('out.off','out.edges','in.off','in.edges',
        'node.kind','node.container','node.bytes','rel.evidence')),
    part INTEGER NOT NULL CHECK(part >= 0),
    bytes BLOB NOT NULL,
    PRIMARY KEY(generation_id, stream, part)
) WITHOUT ROWID;

-- One generation's visible search documents: a bitmap over doc ids (bit
-- id%8 of byte id/8, least significant first) of every search_units.doc_id of
-- its member units. A search opens the generation's view of the search index
-- with it, so a document of a unit the generation does not select is never a
-- candidate and never counts in a statistic. It is written in the activation
-- transaction, after the search index commit that holds those documents.
CREATE TABLE generation_search (
    generation_id INTEGER PRIMARY KEY REFERENCES generations(id) ON DELETE CASCADE,
    visible BLOB NOT NULL
);
