# Architecture decision records

An architecture decision record captures one significant, hard-to-reverse decision at the moment it
was made, so that a reader who was not there can reconstruct the reasoning rather than re-litigate
it. Each record in this directory follows the same shape: a **status** and date at the top (Proposed,
Accepted, Superseded — a superseded record is never edited away, it is linked forward); a
**context** section stating the forces that made a decision necessary, including the measurements
that were taken before anything changed; one subsection per **decision**, each naming the
alternatives that were considered and steel-manned, why the chosen one won on the specific
constraints at hand, what was measured to support it, and the **consequences and trade-offs**
accepted; and a closing **sources** list in which every citation used in the text appears exactly
once with its URL and what it was used for. Records are numbered in order and never renumbered.

| Record | Title | Status |
|---|---|---|
| [ADR-0001](ADR-0001-scale-posture.md) | Scale posture: unlimited by default, bounded by page | Accepted, 2026-09-14; amended 2026-10-08; §2.4 per ADR-0013 |
| [ADR-0002](ADR-0002-storage-identities.md) | Storage identities: integer surrogates and interned keys | Accepted, 2026-09-14 |
| [ADR-0003](ADR-0003-storage-tier2.md) | Storage tier 2: the lexical and provenance tiers, and the gate | Partially accepted, 2026-09-15; lexical tier superseded by ADR-0013, 2026-10-08 |
| [ADR-0004](ADR-0004-wal-synchronous-mode.md) | WAL synchronous mode | Accepted, 2026-09-15 |
| [ADR-0005](ADR-0005-graph-traversal-layout.md) | Graph traversal layout: packed per-generation adjacency, surrogate walk, bitset visited set | Accepted, 2026-09-15 |
| [ADR-0006](ADR-0006-language-servers.md) | Language servers and indexers: the Python server moves to a native checker | Accepted, 2026-09-15 |
| [ADR-0007](ADR-0007-lexical-first-page.md) | Lexical first page: packed term statistics at activation, rank before hydrate, heap-served first page | Superseded by ADR-0013, 2026-10-08 |
| [ADR-0008](ADR-0008-ingestion-group.md) | Ingestion commits once per run: the ingestion group | Accepted, 2026-09-15; amended 2026-10-08 |
| [ADR-0009](ADR-0009-import-staging.md) | The dependence import stages its export the way a bulk load writes | Superseded by ADR-0012, 2026-10-08 |
| [ADR-0010](ADR-0010-engine-memory.md) | Engine memory: the heap is sized to the unit's need, and the host keeps half of what it had | Superseded by ADR-0012, 2026-10-08 |
| [ADR-0011](ADR-0011-run-ledger.md) | The run ledger: a run records what each stage cost, in its own database, read live | Accepted, 2026-09-16; amended 2026-10-08 |
| [ADR-0012](ADR-0012-native-dependence-engine.md) | Dependence is computed in process, and the hosted engine is retired against measured conditions | Accepted, 2026-09-26; amended 2026-10-08, the engine deleted; decision 9 amended 2026-10-09 by ADR-0014 |
| [ADR-0013](ADR-0013-search-index.md) | The lexical tier is an embedded search index, scored by our own BM25F | Accepted, 2026-10-08 |
| [ADR-0014](ADR-0014-stable-identity-incremental-linking.md) | A declaration's identity is its name path, and an edit relinks only what read what changed | Accepted, 2026-10-09 |
| [ADR-0015](ADR-0015-open-call-sites-and-dispatch.md) | Unbound calls are open sites with name candidates, and calls through a base class are found at query time | Accepted, 2026-10-09 |
| [ADR-0016](ADR-0016-seed-budget.md) | The context planner reads lexical matches until no unread match can change what it packs | Accepted, 2026-10-09 |
