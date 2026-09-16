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
| [ADR-0001](ADR-0001-scale-posture.md) | Scale posture: unlimited by default, bounded by page | Accepted, 2026-09-14 |
| [ADR-0002](ADR-0002-storage-identities.md) | Storage identities: integer surrogates and interned keys | Accepted, 2026-09-14 |
| [ADR-0003](ADR-0003-storage-tier2.md) | Storage tier 2: the lexical and provenance tiers, and the gate | Proposed, 2026-09-15 |
| [ADR-0004](ADR-0004-wal-synchronous-mode.md) | WAL synchronous mode | Accepted, 2026-09-15 |
| [ADR-0005](ADR-0005-graph-traversal-layout.md) | Graph traversal layout: packed per-generation adjacency, surrogate walk, bitset visited set | Accepted, 2026-09-15 |
| [ADR-0006](ADR-0006-language-servers.md) | Language servers and indexers: the Python server moves to a native checker | Accepted, 2026-09-15 |
| [ADR-0007](ADR-0007-lexical-first-page.md) | Lexical first page: packed term statistics at activation, rank before hydrate, heap-served first page | Accepted, 2026-09-15 |
| [ADR-0008](ADR-0008-ingestion-group.md) | Ingestion commits once per run: the ingestion group | Accepted, 2026-09-15 |
| [ADR-0010](ADR-0010-engine-memory.md) | Engine memory: the heap is sized to the unit's need, and the host keeps half of what it had | Accepted, 2026-09-16 |
