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
