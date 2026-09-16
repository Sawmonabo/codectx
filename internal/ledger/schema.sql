-- The run ledger's own database. It is a separate file from the index store
-- on purpose: the store's ingestion group holds one transaction open for most
-- of a run and its activation holds an exclusive one, so rows written through
-- it would be invisible to a second process for minutes at a time. This file
-- has one writer -- the collector -- and any number of read-only readers at
-- any moment, which is what a live view requires.
CREATE TABLE ledger_meta (
    singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
    fingerprint TEXT NOT NULL
);
CREATE TABLE runs (
    run_id BLOB PRIMARY KEY CHECK(length(run_id) = 32),
    kind TEXT NOT NULL CHECK(kind IN ('index','deferred','overlay')),
    repository_id BLOB NOT NULL CHECK(length(repository_id) = 32),
    -- The generation this run produced. Null until BeginGeneration returns,
    -- and for ever on a run that failed before it: a run has a ledger from its
    -- first stage, which is earlier than any generation exists. It names a row
    -- in the index store's own file, so no foreign key can state it here.
    generation_id INTEGER CHECK(generation_id IS NULL OR generation_id > 0),
    started_at TEXT NOT NULL,
    -- Null while the run is live. It is set when the collector stops, which is
    -- also when any span still open is closed as interrupted.
    finished_at TEXT CHECK(finished_at IS NOT NULL OR outcome = 'running'),
    outcome TEXT NOT NULL CHECK(outcome IN ('running','ok','failed','subdivided','reused','skipped','interrupted')),
    file_count INTEGER NOT NULL CHECK(file_count >= 0),
    source_bytes INTEGER NOT NULL CHECK(source_bytes >= 0),
    units_planned INTEGER NOT NULL CHECK(units_planned >= 0),
    units_succeeded INTEGER NOT NULL CHECK(units_succeeded >= 0),
    units_failed INTEGER NOT NULL CHECK(units_failed >= 0),
    units_subdivided INTEGER NOT NULL CHECK(units_subdivided >= 0),
    -- Events the bounded bus refused because the collector was behind. A run
    -- never waits on its own accounting, so the loss is counted rather than
    -- prevented, and a reader that sees this above zero knows the span rows
    -- below are incomplete.
    events_dropped INTEGER NOT NULL CHECK(events_dropped >= 0),
    -- Null where the platform does not expose the process's peak resident
    -- size. Unavailable is never zero.
    process_peak_rss_bytes INTEGER CHECK(process_peak_rss_bytes IS NULL OR process_peak_rss_bytes >= 0)
);
CREATE TABLE spans (
    id INTEGER PRIMARY KEY,
    run_id BLOB NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    -- Null for a span the run opened at its top level; otherwise the span it
    -- nests under, which the caller carried in its context.
    parent_id INTEGER REFERENCES spans(id) ON DELETE CASCADE,
    -- The run's own ordinal for this span, allocated where the span was
    -- opened. It orders the rows and it is how a span's end finds the row its
    -- start inserted, without either side knowing a row id.
    seq INTEGER NOT NULL CHECK(seq >= 0),
    stage TEXT NOT NULL CHECK(length(stage) > 0),
    scope_key TEXT NOT NULL,
    provider TEXT NOT NULL,
    started_at TEXT NOT NULL,
    -- Null while the span is running, and on a span whose run ended before it
    -- did: that span never finished, and stamping a time on it would invent a
    -- wall nobody measured.
    finished_at TEXT CHECK(finished_at IS NOT NULL OR outcome IN ('running','interrupted')),
    wall_ms INTEGER CHECK(wall_ms IS NULL OR wall_ms >= 0),
    -- Null where no CPU time can be attributed to this span; cpu_unattributed
    -- then names why. 'overlapped' is in-process work that ran beside other
    -- goroutines, where the process-wide counters measure the process and not
    -- the stage; 'unsampled' is a platform that does not expose the counters.
    cpu_user_ms INTEGER CHECK(cpu_user_ms IS NULL OR cpu_user_ms >= 0),
    cpu_sys_ms INTEGER CHECK(cpu_sys_ms IS NULL OR cpu_sys_ms >= 0),
    cpu_unattributed TEXT NOT NULL CHECK(cpu_unattributed IN ('','overlapped','unsampled')),
    -- Null where nothing sampled this span's process tree, or where the span
    -- did not own one: a resident-size delta across overlapping in-process
    -- work measures the process, not the stage.
    peak_rss_bytes INTEGER CHECK(peak_rss_bytes IS NULL OR peak_rss_bytes >= 0),
    read_bytes INTEGER CHECK(read_bytes IS NULL OR read_bytes >= 0),
    write_bytes INTEGER CHECK(write_bytes IS NULL OR write_bytes >= 0),
    items_in INTEGER NOT NULL CHECK(items_in >= 0),
    items_out INTEGER NOT NULL CHECK(items_out >= 0),
    outcome TEXT NOT NULL CHECK(outcome IN ('running','ok','failed','subdivided','reused','skipped','interrupted')),
    diagnostic_code TEXT NOT NULL,
    failure_json TEXT NOT NULL
);
-- Unique so that a span's end, and a child's parent lookup, name exactly one
-- row by (run_id, seq); it is also the order a reader pages the run in.
CREATE UNIQUE INDEX idx_spans_run_seq ON spans(run_id, seq);
-- Retention deletes a run by the generation it produced, and the overlay
-- cleanup deletes by kind; both are indexed lookups rather than table scans.
CREATE INDEX idx_runs_generation ON runs(generation_id);
CREATE INDEX idx_runs_repository ON runs(repository_id, started_at);
