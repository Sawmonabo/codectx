# 15. Scale posture — the bound inventory

Date 2026-09-14. This page is the inventory behind
[ADR-0001 — Scale posture](../adr/ADR-0001-scale-posture.md), which carries the decisions, the
alternatives considered and the sources. Read the record first; this page exists so the counts it
quotes can be checked.

## The classification

Every count or size bound in the tree was classified before anything was changed. Three classes
stay, because each bounds an allocation without losing anything; four do not, because each turns a
large repository into a worse answer.

| Class | Meaning | Disposition |
|---|---|---|
| A | Lossless cursor pagination: a page size plus a continuation cursor | Keep |
| B | Wire and pre-allocation bounds applied to untrusted input before allocating | Keep |
| C | Memory admission that schedules or serialises work without rejecting it | Keep |
| D | Scale refusal: the repository, query or unit is turned away | Remove as a default |
| E | Work truncation: a walk, ranked set or candidate list stops early | Remove as a default; a legitimate stop must be resumable |
| F | A field bound that fails a unit or a request | Truncate and flag instead |
| G | A report drops rows | Paginate, or publish an honest omitted count |

## What the inventory found

**69 bounds** across configuration keys and hard-coded constants.

| Class | Rows | Character of the group |
|---|---|---|
| D — refusal | 18 | The largest group, and concentrated where a monorepo lives: a workspace file ceiling enforced at three separate sites, a directory-entry ceiling that let one generated directory kill a capture, a per-family analysis-unit ceiling that refused a monorepo's plan outright, and provider wall-clock timeouts that failed an analysis unit |
| E — truncation | ~6 | Graph depth, visited nodes and edges stopping a walk with no continuation; a ranked-set ceiling discarding the tail of the corpus; per-tier candidate ceilings |
| F — unit failure | ~6 | Eleven field-byte ceilings that returned an error rather than cutting, one of which turned an over-long generated symbol into a file that published nothing |
| G — row drop | ~6 | A capability report read behind a bare `LIMIT`, run records stopping at a ceiling with no counter, a diagnostic clamp, silent list slices |
| Mixed | several | Rows that carry two classes because the same constant refuses in one place and drops silently in another |
| A / B / C, already compliant | ~12 | Recorded so a later sweep does not "fix" a bound that is doing its job |

Three findings set the shape of the work:

1. **One validator loop made the ruling unimplementable.** Around fifty configuration keys were
   rejected at `<= 0` with the message *"no zero or negative setting means unlimited"* — the exact
   inverse of the ruling. Nothing else could be expressed until it changed.
2. **Refusal clustered at monorepo scale**, not at the edges.
3. **Silence was as common as refusal.** A truncation nobody can detect is worse than one that is
   reported: it makes "did I get everything?" unanswerable.

## The storage measurement

Measured separately, against the 3.5× budget of stored bytes over eligible source bytes. The three
ratios come from two different corpora and must be read with their corpus attached.

| Measurement | Corpus | Result |
|---|---|---|
| First run | 10 000 generated files, 5 697 273 B (~570 B/file) | 75.00× |
| Re-measurement | same corpus | 73.86× (420 810 752 B over 5 697 273 B) |
| Corrected reference corpus | 10 000 files, 1 052 933 lines, 86 064 203 B (~8 KiB/file) | 16.10×; the content store alone exactly 1.00× |
| Per indexed symbol | 604-file cut of the same generator | ≈3.4 KB (25 710 592 B over 7 595 node facts) |

Per-table attribution of the full store:

| Family | Bytes | Share |
|---|---|---|
| Relations (identity table, its two auto-indexes, two lookup indexes, facts) | ~94.5 MB | 22.5 % |
| Evidence (and its four indexes) | 90.7 MB | 21.6 % |
| Native aliases (and its two indexes) | 84.8 MB | 20.2 % |
| Nodes (facts, identity table, auto-indexes, name and path indexes) | ~83.9 MB | 20.0 % |
| Search (units, full-text shadow tables, index) | ~40 MB | 9.5 % |

The amplifier is **identity width**, not source volume: on the alias table, rows cost 117 B each
for about 100 B of content, one secondary index costs the same 117 B/row and a second costs
121 B/row — more than the table it indexes. Full-text content duplication, per-unit JSON payloads,
an un-checkpointed log, page size and fill, evidence fan-out and carried membership were each
measured and ruled out.

The same reference-scale run recorded the other open miss: an indexing process-tree peak of
**912.6 MiB against a 768 MiB envelope**, with the cold index itself passing at 2 m 56 s against a
3 min target.

## Where each decision landed

See [ADR-0001](../adr/ADR-0001-scale-posture.md) §2 for the reasoning and §3 for what is still
open. In outline: the configuration `Limit` type with unlimited defaults (§2.1); a resumable
traversal with a cursor for every stop and the visited set off the heap (§2.2); external merge in
the planner with byte-identical order (§2.3); a lossless ranked set with a spooled tail (§2.4);
field bounds that truncate and flag (§2.5); progress-based stall detection and waiting admission
(§2.6); reports that drop nothing (§2.7); the storage redesign, accepted and scheduled (§2.8); and
capsule pagination, accepted and scheduled, which is the one known remaining default cap (§2.9).
