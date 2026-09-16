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
    metadata_json TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id),
    CHECK((start_byte IS NULL AND end_byte IS NULL)
       OR (file_id IS NOT NULL AND start_byte IS NOT NULL AND end_byte IS NOT NULL
           AND start_byte >= 0 AND end_byte >= start_byte))
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
    node_id INTEGER,
    relation_id INTEGER,
    fact_key TEXT NOT NULL,
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, relation_id) REFERENCES relation_facts(unit_id, relation_id),
    CHECK((node_id IS NOT NULL AND relation_id IS NULL) OR (node_id IS NULL AND relation_id IS NOT NULL))
);
-- evidence.id stays a PRIMARY KEY, refused by call site rather than by size.
-- units.go:748 and delta.go:437 upsert with ON CONFLICT(id) DO NOTHING, which
-- SQLite only accepts against a PRIMARY KEY or UNIQUE index, and delta.go:573
-- clips surplus evidence BY id value. The id is a pure function of the evidence
-- fields (model.NewEvidenceID), so this uniqueness is a derived invariant, not
-- an arbitrary surrogate, and the value reaches the wire as
-- ReferenceOccurrence.evidence_id and FactReference.evidence_ids.
CREATE TABLE evidence (
    id BLOB PRIMARY KEY CHECK(length(id) = 32),
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    node_id INTEGER,
    relation_id INTEGER,
    precision TEXT NOT NULL CHECK(precision IN ('compiler','language_server','static_analysis','syntax','heuristic')),
    file_id BLOB,
    start_byte INTEGER,
    end_byte INTEGER,
    native_key_id INTEGER NOT NULL REFERENCES native_keys(id),
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
-- against 34.5 MB on m32rimm, 47.1 against 41.2 on promptfoo -- because
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
-- No `paths` intern table: the only high-multiplicity path column is
-- search_units.path, and `path` is one of the columns search_fts indexes, fed
-- from this column on every write. Interning it would put a surrogate id where
-- the index expects text, so the path field would leave the full-text index.
-- files.path is 818 rows / 114 KiB on that same fixture and is not an
-- amplifier.
CREATE TABLE native_aliases (
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    scope_key_id INTEGER NOT NULL REFERENCES scope_keys(id),
    native_key_id INTEGER NOT NULL REFERENCES native_keys(id),
    node_id INTEGER NOT NULL REFERENCES node_ids(id),
    PRIMARY KEY(unit_id, scope_key_id, native_key_id, node_id)
) WITHOUT ROWID;
CREATE TABLE search_units (
    rowid INTEGER PRIMARY KEY,
    unit_id INTEGER NOT NULL REFERENCES units(id) ON DELETE CASCADE,
    search_key BLOB NOT NULL CHECK(length(search_key) = 32),
    node_id INTEGER,
    file_id BLOB NOT NULL,
    path TEXT NOT NULL,
    kind TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    qualified_name TEXT NOT NULL DEFAULT '',
    signature TEXT NOT NULL DEFAULT '',
    start_byte INTEGER NOT NULL CHECK(start_byte >= 0),
    end_byte INTEGER NOT NULL CHECK(end_byte >= start_byte),
    token_count INTEGER NOT NULL CHECK(token_count >= 0),
    -- The search_fts rowid holding this document's indexed text. It is the
    -- DOCUMENT's identity, not the row's: a delta carry-over copies this
    -- column forward with the rest of the row, so the carried unit points at
    -- the posting the text was originally indexed into instead of re-deriving
    -- a body the producer never published (a producer's Body is not the source
    -- over [start_byte,end_byte) -- see internal/provider/treesitter/facts.go).
    -- Sharing is confined to one carry chain: CarryOver refuses a predecessor
    -- of another provider or scope key, and generation_units is keyed by
    -- (generation_id, provider_id, scope_key), so at most one row of a chain
    -- is visible in any generation and a doc_id still names one document
    -- there. The posting is released like node_ids: deleteUnit removes it only
    -- when no other unit's row still names it.
    doc_id INTEGER NOT NULL CHECK(doc_id > 0),
    -- The lexical segment this document's postings were folded into, or NULL
    -- while the unit is still building. It is the DOCUMENT's segment, not the
    -- row's: a carry-over copies it forward with doc_id, which is what makes a
    -- carried unit own exactly the segments that hold at least one of its
    -- carried documents rather than every segment its predecessor ever owned.
    -- The reference is NO ACTION: a segment a document still names must not be
    -- collectable. It is the SOURCE OF TRUTH for which segments a generation
    -- reads and for which segments are garbage: a compaction re-points every
    -- row of its inputs at the merged segment, so no row names an absorbed
    -- segment afterwards and a retired unit that is attached again finds its
    -- documents where its rows point.
    segment_id INTEGER REFERENCES lexical_segments(id),
    UNIQUE(unit_id, search_key),
    FOREIGN KEY(unit_id, node_id) REFERENCES node_facts(unit_id, node_id),
    FOREIGN KEY(unit_id, file_id) REFERENCES unit_inputs(unit_id, file_id)
);
-- The lexical tier stores no second copy of the source (ADR-0003 §2.1).
-- search_units carries no `body` column and search_fts is CONTENTLESS, so the
-- indexed body text exists only as index postings. That makes the postings
-- irreplaceable rather than merely economical: no row of this database can
-- reconstruct the text a document was indexed with, which is why a carry-over
-- REUSES a carried document's posting through search_units.doc_id instead of
-- rebuilding one. `contentless_delete=1` is
-- what keeps DELETE and INSERT OR REPLACE available on unit invalidation
-- (https://sqlite.org/fts5.html#contentless_tables, engine floor 3.43.0; the
-- embedded engine is 3.53.4). Snippets and highlights are served from the
-- content store through the verified range reader, over the file_id and byte
-- range each row already carries -- never from this index. The index's rebuild
-- source is therefore the content store, not the database (docs/storage.md).
CREATE VIRTUAL TABLE search_fts USING fts5(
    name, qualified_name, signature, path, body,
    content='', contentless_delete=1, tokenize='unicode61', detail='full'
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
    node_id INTEGER REFERENCES node_ids(id),
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
-- fact_keys uniqueness, re-specified for INTEGER node_id/relation_id.
-- The old expression index UNIQUE(unit_id, fact_key, coalesce(node_id, x''),
-- coalesce(relation_id, x'')) used an empty BLOB as the "absent" sentinel over
-- two columns that are now INTEGER. A coalesce(...,0) sentinel would work --
-- CHECK(id > 0) on node_ids/relation_ids makes 0 unreachable -- but a sentinel
-- is not needed at all. The table's CHECK guarantees exactly one of node_id /
-- relation_id is non-NULL, and SQLite treats NULLs in a UNIQUE index as
-- distinct, so UNIQUE(unit_id, node_id, fact_key) constrains exactly the
-- node-backed rows and never collides across the relation-backed ones (every
-- one of which carries node_id NULL). Two such indexes enforce precisely what
-- the sentinel expression enforced, over narrower keys, and -- ordered
-- (unit_id, <ref>, fact_key) -- each also subsumes the (unit_id, <ref>) probe
-- index it replaces (delta.go:318 keySurvives). Three indexes become two.
-- They are deliberately NOT partial: a partial index cannot serve the units
-- ON DELETE CASCADE (DELETE FROM fact_keys WHERE unit_id = ?), which would then
-- degrade to a table scan. EXPLAIN QUERY PLAN shows both: the cascade and
-- the (unit_id, <ref>) probe each SEARCH one of these indexes. The target-less ON CONFLICT DO NOTHING at units.go:840 and
-- delta.go:365 binds to any unique index, so it keeps working unchanged.
CREATE UNIQUE INDEX idx_fact_keys_node ON fact_keys(unit_id, node_id, fact_key);
CREATE UNIQUE INDEX idx_fact_keys_relation ON fact_keys(unit_id, relation_id, fact_key);
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
CREATE INDEX idx_search_unit ON search_units(unit_id, rowid);
-- deleteUnit releases a posting only when no surviving unit's row still names
-- it, and every read resolves a document by doc_id. Without this index both
-- are a scan of search_units per probe.
CREATE INDEX idx_search_doc ON search_units(doc_id);
-- The segment arm of the same column. An activation walks the documents of
-- each segment it is about to merge, re-points every row of the merged inputs
-- and the collector asks, per candidate segment, whether any row still names
-- it; all three are a scan of search_units per segment without this index, and
-- carrying doc_id in it makes the walk index-only.
CREATE INDEX idx_search_segment ON search_units(segment_id, doc_id);
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
-- The live watch heartbeat of one repository: what a `codectx index --watch`
-- process in another terminal knows, published where a second process can read
-- it. Without the row, a reporting process has no cross-process source for the
-- pending-event count and no way to say whether a watch is running at all.
--
-- `expires_at` is written by the watcher itself, not derived by the reader from
-- configuration: a reader whose `[index]` block differs from the watcher's --
-- another checkout, another user, an edited file since the watch started --
-- would otherwise compute a liveness window the writer never promised. The
-- writer states its own deadline and refreshes it while it lives, so an expired
-- row means the process that owned it stopped refreshing, which is the only
-- signal of process death that works on every platform this product supports
-- (a pid can be reused, and probing one is neither portable nor race-free).
--
-- `pending_events` and `last_pass_at` are nullable because absent and zero are
-- different answers here as everywhere else: a watch driven only by periodic
-- reconciliation has no notification queue to count, and a watch that has not
-- completed a pass yet has no pass time -- neither is "0 pending" or "the epoch".
--
-- One row per repository, replaced in place: this is liveness, not history.
CREATE TABLE watch_heartbeat (
    repository_id BLOB PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    writer_pid INTEGER NOT NULL CHECK(writer_pid > 0),
    last_pass_at TEXT,
    pending_events INTEGER CHECK(pending_events IS NULL OR pending_events >= 0),
    expires_at TEXT NOT NULL
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

-- generation_lexical is written LAST, after every generation_segments row, so
-- its presence is the commit marker: a reader that finds the row is guaranteed
-- the generation's whole segment set behind it. It also carries the
-- generation's document statistics, which the scorer would otherwise recompute
-- by probing every visible document (ADR-0007 Decision 1).
CREATE TABLE generation_lexical (
    generation_id INTEGER PRIMARY KEY REFERENCES generations(id) ON DELETE CASCADE,
    doc_count INTEGER NOT NULL CHECK(doc_count >= 0),
    token_total INTEGER NOT NULL CHECK(token_total >= 0),
    -- The generation's visible documents, one bit per document rowid. A
    -- document an inherited segment still holds but whose unit left the
    -- generation is hidden here; nothing is rewritten to hide it, and a reader
    -- takes the bitmap as it stands instead of scanning the generation's
    -- documents to rebuild it.
    visible BLOB NOT NULL
);

-- A lexical segment is an immutable packed structure over a set of documents:
-- the term directory, the term text it points into, the posting lists and the
-- per-document attributes of the documents they name, each
-- chunked into lexical_segment_parts. A segment is written once, by the seal of
-- the unit whose documents it holds, and is never rewritten; an activation
-- names segments, it does not rebuild them (ADR-0007 Decision 1).
CREATE TABLE lexical_segments (
    id INTEGER PRIMARY KEY,
    term_count INTEGER NOT NULL CHECK(term_count >= 0),
    doc_count INTEGER NOT NULL CHECK(doc_count >= 0),
    bytes INTEGER NOT NULL CHECK(bytes >= 0)
);

-- One chunk of one segment stream. `term.dir` is the fixed-width term
-- directory in term order -- term slice, document frequency and the slice of
-- `post.list` holding that term's per-document (column, count) sequence --
-- `term.text` the concatenated term bytes it points into, and `post.list` the
-- posting lists themselves, whose documents ascend by rowid. `doc.dir` is the
-- fixed-width document directory in ascending document rowid and `doc.attr`
-- the attribute records it points into: every field a search candidate is
-- served from, packed once per document, so a page of candidates is hydrated
-- from these bytes instead of one document-row read each (ADR-0007 Decision 2). `part` is 0-based
-- and the parts of a stream concatenate to it, so a reader holds a bounded
-- window of parts rather than a whole vocabulary.
CREATE TABLE lexical_segment_parts (
    segment_id INTEGER NOT NULL REFERENCES lexical_segments(id) ON DELETE CASCADE,
    stream TEXT NOT NULL CHECK(stream IN ('term.dir','term.text','post.list','doc.dir','doc.attr')),
    part INTEGER NOT NULL CHECK(part >= 0),
    bytes BLOB NOT NULL,
    PRIMARY KEY(segment_id, stream, part)
) WITHOUT ROWID;

-- The segment set of one generation, in read order. The set is DERIVED at
-- activation from search_units.segment_id -- the distinct segments the live
-- rows of the generation's member units point at -- and recorded here so a
-- reader resolves it with one scan; nothing is rewritten to publish it, and a
-- retained generation keeps its own rows when a later generation merges the
-- same segments away. The reference is RESTRICT rather than CASCADE: a segment
-- a generation still names must not be collectable while that row exists.
CREATE TABLE generation_segments (
    generation_id INTEGER NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    segment_id INTEGER NOT NULL REFERENCES lexical_segments(id) ON DELETE RESTRICT,
    ord INTEGER NOT NULL CHECK(ord >= 0),
    -- How many of the segment's documents this generation hides. Zero is the
    -- common case and is what lets a document frequency answer from the term
    -- directory; a segment with hidden documents is counted by walking the
    -- term's posting list against the visible bitmap instead.
    hidden INTEGER NOT NULL CHECK(hidden >= 0),
    PRIMARY KEY(generation_id, ord)
) WITHOUT ROWID;
CREATE INDEX idx_generation_segments_segment ON generation_segments(segment_id, generation_id);
