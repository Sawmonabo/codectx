# ADR-0001 — Scale posture: unlimited by default, bounded by page

- **Status:** Accepted (decisions 1–7 implemented; decisions 8 and 9 accepted and scheduled)
- **Date:** 2026-09-14
- **Supersedes:** the direction ruling of 2026-09-13 recorded in
  [`docs/research/00-synthesis.md` §8](../research/00-synthesis.md#8-direction-ruling-2026-09-13-user-adopted-reviewer-directive),
  which this record generalises from one provider to the whole product
- **Inventory and per-class tables:** [`docs/research/15-scale-posture.md`](../research/15-scale-posture.md)

---

## 1. Context

### 1.1 What was ruled

codectx must index very large enterprise repositories and monorepos, quickly and
memory-efficiently. The binding ruling has two halves, and the second is the one that makes the
first hard.

> **No default limit may refuse a repository, skip a file, fail an analysis unit, drop rows, or
> truncate an answer.** Every count or size bound is user-set only; `0` (or the spelling
> `"unlimited"`) means unlimited, and the *default* is unlimited. Exceeding a user-set limit is
> reported, never silent. A field-length bound truncates and flags; it never fails a unit.

> **"Unlimited" is never "load everything into heap."** Peak resident memory must be a function of
> page or batch size, not of repository size.

The reference host for the no-out-of-memory obligation is a 47 GiB machine indexing a
million-line monorepo.

The second half rules out the cheap reading of the first. Deleting a cap and letting the
corresponding structure grow to repository size would satisfy the letter of the ruling and
produce an out-of-memory failure, which is a refusal by another name — a worse one, because it
carries no diagnosis.

This generalises a narrower ruling already on the record for the dependence tier, which had
established that estimates schedule and serialise work rather than reject it, that only an
explicit user limit rejects work up front, and that no analysis limit is lowered and no fact
family omitted in order to fit.

### 1.2 Not every bound is a limit

A blanket "remove all the numbers" would have removed the mechanisms that keep memory bounded.
The bounds in the tree were therefore classified before anything was changed, into three classes
that stay and four that do not:

| Class | Meaning | Disposition |
|---|---|---|
| **A** | Lossless cursor pagination — a page size plus a continuation cursor | Keep. Nothing is lost; the caller follows the cursor. |
| **B** | Wire and pre-allocation security bounds — signed token and cursor bytes, field and depth caps applied before allocation, one-page response ceilings, subprocess capture caps | Keep. These bound an allocation made from untrusted input. |
| **C** | Memory admission — reservations that schedule or serialise work without rejecting it | Keep. Deferral is not refusal. |
| **D** | Scale refusal — the repository, query or unit is turned away | Remove as a default. |
| **E** | Work truncation — the walk, the ranked set or the candidate list stops early | Remove as a default; where a stop is legitimate it must be resumable. |
| **F** | A field bound that fails a unit or a request | Convert to truncate-and-flag. |
| **G** | A report drops rows | Convert to a paginated surface or an honest omitted count. |

### 1.3 The measured starting point

A full inventory of the tree found **69 bounds**: configuration keys and hard-coded constants
alike. Their distribution is what set the shape of the work. Class D — outright refusal — was the
largest single group at 18 rows, ahead of classes E, F and G at six or so apiece before counting
the rows that carry two classes at once (a bound that fails a request in one place and silently
drops rows in another). Roughly a dozen rows were already compliant and are recorded so that a
later sweep does not "fix" them.

Three findings dominated the inventory:

1. **One validator loop made the ruling unimplementable.** Around fifty configuration keys were
   rejected at `<= 0` with the message *"no zero or negative setting means unlimited"* — the exact
   inverse of the ruling. Until that loop changed, no other lane could express an unlimited
   default at all.
2. **The refusals were concentrated where a monorepo lives.** A 250 000-file workspace ceiling was
   enforced at three separate sites; a 200 000-entry directory ceiling meant one generated
   directory could kill an entire capture; a 512-unit-per-family bound refused a large monorepo's
   analysis plan outright.
3. **Silence was as common as refusal.** Several sites stopped a walk, clipped a list or dropped a
   report row with no counter, no warning and no cursor. A truncation nobody can detect is worse
   than one that is reported, because it makes "did I get everything?" unanswerable.

### 1.4 The measured storage risk

Separately, the store was measured against its own 3.5× budget (stored bytes over eligible source
bytes) and missed by an order of magnitude. The history is worth stating precisely, because the
three numbers come from three different corpora and read as contradictory otherwise:

| Measurement | Corpus | Ratio |
|---|---|---|
| First run | 10 000 generated files, 5 697 273 B (~570 B/file) | **75.00×** |
| Re-measurement, same corpus | as above | **73.86×** (420 810 752 B over 5 697 273 B) |
| Corrected reference corpus | 10 000 files, 1 052 933 lines, 86 064 203 B (~8 KiB/file) | **16.10×** (1 385 729 099 B stored; the content store alone is exactly 1.00×) |

The corpus-independent figure is **≈3.4 KB per indexed symbol** (25 710 592 B over 7 595 node
facts on a 604-file cut of the same generator). Because the cost is per identity and not per
source byte, the ratio moves with the corpus while the per-symbol number does not — which is why
the per-symbol number, not the ratio, became the primary regression metric.

The same reference-scale run recorded a second miss at the time: an indexing process-tree peak of
912.6 MiB against a 768 MiB envelope, with the cold index itself passing at 2 m 56 s against a
3 min target. Re-measured after the planner's external merge sort and the batched provider sinks
landed, the same corpus indexes in 1 m 32 s with a **145.3 MiB** process-tree peak (two cold runs,
250 ms and 50 ms sampling, within 0.1 MiB of each other), so that miss is closed.

---

## 2. Decisions

### 2.1 Unlimited by default, user-set only

**Decision.** Introduce one configuration type, `Limit`, that accepts an integer or the string
`"unlimited"`, where `0` and `"unlimited"` are one value with one stable fingerprint rendering. It
exposes `IsUnlimited()`, `Exceeded(n)` (strictly greater), `ValueOr(fallback)` for the sites that
genuinely need a finite number, and a lattice `Min` in which unlimited is the **top**, so any
finite value narrows it. The validator loop inverts from `<= 0` to `< 0`: only a negative value is
refused. Fifteen keys flip their default to unlimited. Every enforcement site calls `Exceeded` and,
on true, **reports** that a user-set limit of *N* was exceeded — never clamps, never drops
silently, never branches on `> 0` inline. Where a page-size request legitimately meets a ceiling,
the clamp is reported as *requested N, effective M* on a request-scoped collector carried on the
context rather than threaded through two dozen reader signatures — the same shape the standard
library uses for request-scoped tracing [S34].

**Alternatives considered.**

*Leave the keys as plain integers and write `if n > 0 && x > n` at every site.* This is the
smallest diff and needs no new type. It was rejected because the inventory found six separate
places that already repeated the "zero means broken" convention independently, and the audit
showed that a hand-written zero check is exactly the defect that keeps recurring: one lane found a
site reading `size <= 0` (which would have excluded every non-empty file from search) and another
reading `size > 0` (which would have skipped every manifest) — both silently wrong rather than
loudly broken. A type makes the wrong spelling fail to compile.

*Sentinel `-1` for unlimited, keeping `0` as "unset".* Rejected for the configuration surface: the
ruling's own words are that `0` means unlimited, and two spellings for the same concept
(`0`/`"unlimited"`) already stretch the fingerprint contract. The sentinel survives in exactly one
place where it is needed and is documented there: a *request* field whose `0` must keep meaning
"inherit the deployment default", so `-1` is the request-side spelling for "no ceiling".

**Why it was chosen.** Three properties decided it. The bound must round-trip through the
configuration fingerprints — every bound feeds three policy hashes, so `0` and `"unlimited"` must
hash identically or the same configuration would present two identities. The ordering rule for
layered configuration (a project file may narrow a user setting, never widen it) is only
expressible if unlimited is a lattice element rather than a magic small number. And `Exceeded`
being strictly greater removes an off-by-one class of bug from fifty call sites at once.

**What was measured.** Two proofs, both mutation-checked: reverting the fingerprint rendering to
the old integer quoting makes the "`0` and `"unlimited"` are one identity" assertion fail;
flipping the lattice so unlimited is the bottom makes the ordering assertion fail. A negative value
is still refused, and a zero *reservation* (not a bound) is still refused.

**Two finite families are kept, deliberately.**

*Context caller budgets* — default file count, byte count, estimated tokens and slice count for a
compiled context plan — keep non-zero defaults. They are not limits on what the system can see;
they are the shape of the window the answer must fit into. A context plan with no byte budget is
not a useful answer. They are admissible **only** because every excluded file is named in the
manifest with a reason, paginated and never summarised. That condition is load-bearing: the
moment exclusions become a summary, these stop being budgets and become class-E truncation. The
request may raise them, including to unlimited.

*Progress-based stall detection* — a finite `stall_timeout` per provider subprocess. The wall-clock
timeouts that failed an analysis unit (20 minutes for one provider, 45 for another) default to
`0`, meaning no wall clock at all: a 45-minute default on a monorepo unit is a scale refusal
wearing a deadline's clothes. But a wedged subprocess with no deadline hangs forever, so a
detector replaces the clock. It is not a limit under the ruling because it measures **progress,
not elapsed time**: a subprocess that is producing bytes or consuming CPU is never touched,
however long it runs. Only one that has produced nothing for the stall window is terminated, with
the reason `stalled`, reported.

**Consequences.** Every remaining bound is the operator's choice, and the diagnostic burden moves
from "why did it refuse?" to "why is it slow?". Two safety nets now carry the load that the caps
used to carry: the query deadline, which must end a *page* and mint a cursor rather than end an
answer; and memory admission, which serialises. Both are exercised by tests named for the failure
mode they protect. The stall detector's signals are bytes read and, where a process tree can be
sampled, CPU; on a platform where the tree cannot be sampled, a legitimately silent computation
now dies at the stall window where it previously survived to the wall clock. That is the accepted
cost of removing the wall clock, and it is recorded rather than worked around: a third signal —
growth of an output file — would close it.

### 2.2 Resumable traversal: a cursor for every stop

**Decision.** The graph walk mints a continuation cursor for **every** stop reason that leaves a
non-empty frontier, not only for "the page is full". Visited-node and edge budgets stop being
cumulative ceilings on a whole walk and become **per-page work budgets**. The frontier spills to
the existing per-query spool when it exceeds its byte budget, and continues on the next page
instead of stopping. The cumulative visited set leaves the heap entirely: it lives in the
continuation spool, and membership is answered **per level in one sequential sweep**, not per node.

**Alternatives considered.**

*A roaring bitmap over dense node ordinals* [S10][S11] — the plan's own first proposal, and the
standard answer for a visited set. **Rejected on evidence from the schema**: a node identity here
is a content hash, not an ordinal, so there is no dense integer id space for a bitmap to compress.
A bitmap would first require assigning dense ids, which is a storage change of its own. The
rejection is recorded because the idea is attractive enough to be re-proposed.

*A blocked Bloom filter in front of the spool.* Rejected: a filter accelerates *negatives*, and the
level-batched sweep already answers a whole level in one pass. It would add a false-positive path —
and here a node wrongly reported as already admitted is an **edge silently dropped**, the exact
class-G defect this wave exists to remove — in exchange for no bound the design did not already
have.

*Re-deriving the frontier per page from a keyset scan under the pinned generation*, avoiding a spool
altogether, as mature search systems do with a point-in-time view and `search_after` [S14],
with the page token opaque and server-minted [S13]. This remains the cheaper design where it
applies and is not foreclosed; it was not taken this wave because the spool machinery already
existed and worked for one stop reason, so generalising it was a smaller, provable change than
introducing a second continuation mechanism.

*Per-node membership probes against the spool.* Rejected on cost: the spool is forward-only, so a
per-node probe is a full scan per node. Level-batched duplicate elimination is the published shape
for exactly this problem [S31], and keeping a bounded front in RAM while the bulk lives outside it
is semi-external breadth-first search as described in the literature [S32][S7][S9]. Designs that
scan the whole edge set per iteration are the wrong family here [S8]: the frontier is small
relative to the graph, and edge I/O should be issued only for it.

**Why it was chosen.** The central defect was that the walk had a working continuation mechanism
and used it for one stop reason out of four. Every other stop returned "truncated, no
continuation" while the frontier was still non-empty — a refusal disguised as an answer. Making
the budgets per-page turns them from ceilings into work quanta, which is what a caller following a
cursor actually wants, and it is the only reading under which "unlimited" and "bounded heap" are
both true.

**What was measured.**

- Over a 500-node fixture with a 50-node visited budget and unlimited depth and edges, the union
  of the pages equals the unlimited answer, with each edge returned exactly once and a
  continuation on every budget stop. Mutation: minting a cursor only for "page full" fails the
  test with *"page 1 spent the visited budget with no continuation: the frontier is unreachable"*.
- The visited set at **200 000 nodes over 40 levels** holds a front of 50 entries — below garbage
  collection noise. Mutation (let the streamed cumulative set land in the front, which is the shape
  of the old materialisation) reports **19 774 784 bytes** held for the same walk. 19.77 MB → noise
  is the before-and-after.
- Spool peak across both frontier proofs: **9 704 KiB** for the whole test binary.
- Depth exhaustion, which previously exited the loop with `truncated = false` and no reason at all,
  now reports. Mutation: deleting the depth-limited branch fails with *"truncated = false, reason =
  ""; want a depth-limited walk to say so"*.

**Why depth exhaustion reports but mints no cursor.** The maximum depth is a component of the
traversal query hash that a cursor is bound to. A continuation minted at the depth bound could only
resume a walk already past that bound — a non-terminating chain returning no rows. Nodes at the
bound are admitted but their edges were never read, so under *this* query there is no work to
resume. The defect was the silence, and the silence is fixed. Asking for more depth is a different
query, and says so.

**Consequences.** Peak resident memory on this path is now: the resumed frontier (bounded by the
frontier byte budget) plus this page's admissions (bounded by the page item ceiling) plus one
level's probe answers plus one page of relations plus one spool record in flight. Nothing scales
with the graph. The cost side is real and stated for the verification lane: one spool sweep per
*level* replaces one per page, so a chain-shaped graph — and a call chain is exactly that — can pay
up to one sweep per page item. Two spools are transiently live per walk, since the consumed one is
released only after the fresh one is written. Two callers that expand a whole walk in one request
(impact and rollup) are still walk-sized by construction, because they rank or aggregate the whole
walk in memory; that is ranked-spool work, not frontier work, and it is open.

### 2.3 External merge for the planner, with byte-identical order

**Decision.** The index planner's whole-snapshot input list — one record per file in the snapshot,
previously sorted in heap and aliased into every whole-snapshot unit — becomes an external sort:
records fill a fixed buffer, spill as stable-sorted runs, and merge through one k-way heap into a
single file. Peak is the run buffer plus fan-in times block size, independent of file count. The
sorted result is exposed as an iterator rather than a slice.

**Alternatives considered.**

*An order-insensitive fold — XOR or sum the per-input digests — so no sort is needed at all.* This
is by far the cheapest change and removes the structure entirely. It was **rejected** because the
sort exists only to make the folded input digest deterministic, and that digest **is** unit
identity. Changing the fold changes every unit id, which silently invalidates every stored reuse
and carry decision of every prior generation: indexes would appear to work while quietly rebuilding
everything, or worse, reusing across an identity change. Byte-identical order is a compatibility
constraint, not a preference.

*Keep the in-heap sort and rely on the host having enough memory.* Rejected by the second binding
ruling. Measured, the old shape retained roughly **44 MiB** at 300 000 inputs — 88 bytes of headers
plus two 64-byte hex bodies per record — growing linearly, and aliased into every whole-snapshot
unit.

*Reuse the query continuation spool as the sort primitive.* Rejected on contract: a continuation
spool is lease-bound, expiry-stamped and generation-bound state for a paginated *answer*, with a
budget-exhaustion degradation path. A sort run is none of those things. The sort reuses the spool's
frame conventions — the same length prefix, the same oversized-record chunking, the same shared
reservation accounting — and none of its lease machinery.

**Why it was chosen.** External merge sort with an explicitly auditable bound is the settled shape
for this problem: the published contract is `maxPartitionsInRAM × ramBufferSize` [S15], i.e. a
bound the operator can compute rather than hope for. Replacement selection [S16][S33] is the
available further lever — it roughly doubles mean run length and so halves merge fan-in — and was
**not** taken: runs here are plain buffer-fill. It is recorded as the next lever if fan-in becomes
the cost. The broader pattern of building sorted output once and ingesting it cheaply, rather than
inserting in arbitrary order, is the same one storage engines use for bulk load [S6].

**What was measured.** Heap held with the run and sorter alive, after a forced collection, at ten
times the input count:

```
30 000 inputs   522 024 bytes
300 000 inputs  524 088 bytes      (+0.4 %, flat)
```

Byte identity is proved directly: 50 000 inputs with a 1024-record run buffer produce ~49 spilled
runs and a real merge; the test folds the identity digest over both the in-heap reference order and
the merged order and compares, then folds the merged run **twice** to pin re-iterability. Mutation
(invert the merge comparator) fails with *"unit input … is not after …; inputs must be added in
ascending file_id order"*.

**Consequences.** The sorted run is a file with a lifetime, so the plan gained a `Close`
obligation; both production callers take it, and forgetting it leaks one temporary file, not heap.
A run read after close returns a typed error rather than zero records — which matters, because an
empty input fold is a unit identity that would reuse forever whatever changed. With this structure
fixed, the planner's largest remaining structure is the plan's own output: roughly 120 bytes plus a
scope key per planned file unit, or 50–80 MiB at 300 000 files with two file providers active. That
is named for the verification lane rather than claimed as solved. A new I/O characteristic is also
named: each whole-snapshot unit re-reads the merged file, where it previously read memory.

### 2.4 Search: a lossless ranked set

**Decision.** The ranked result set stops refusing new distinct keys past a fixed ceiling (ten
times the page size, hard-coded). It keeps every distinct key; the tail goes to the disk-backed
spool and is served by a cursor. Spool exhaustion ends a **page** with page 1 served and the
dropped-hit count reported — never the query, page 1 included. Oversized spool records are split
across continuation frames rather than refused. The per-tier candidate ceiling on exact-match tiers
is removed and the tiers walk their whole keyset.

**Alternatives considered.**

*A fixed-size min-heap top-K and accept that the tail is lost.* A size-K heap streamed over
candidates is O(K) regardless of candidate count, so no spill is needed **for ranking**. It was
rejected as the whole answer because the ruling forbids discarding the tail of the corpus: the
requirement is not "the top K are correct", it is "nothing is dropped without saying so". The heap
remains the right shape for the ranking step; the spool is what makes the tail retrievable.

*Truncate an oversized spool record's largest field and flag it, as field bounds do.* Rejected at
this layer: the record is JSON, and truncating it produces invalid JSON — a size bound converted
into a corruption bug. Chunking across frames is the correct answer, with the assembly bound set to
the spool's own byte budget so no record can exceed what the budget already admitted.

*A new `spool_bytes` configuration key.* Rejected, twice, by two independent lanes reaching the
same conclusion: the temporary-disk budget is a single number, and a second key lets its consumers
be configured to oversubscribe the very budget the first key exists to enforce. The divisor stays a
documented derivation of the one budget. The same reasoning governs the sort-run budget, which is a
*share* of the query memory admission rather than a key of its own — a memory budget treated as a
first-class parameter and divided among workers with a floor is the shape mature search engines use
[S22]. The floor raises rather than refuses, because refusing over a performance knob is the
class-D failure this posture exists to remove.

**Why it was chosen.** The published designs for deep result retrieval make the same split: a
point-in-time view plus a cursor keyed on the last row, rather than an offset or a memory-resident
scroll state [S14], with collectors that stream candidates rather than materialise them [S30]. The
tail belongs on disk behind a cursor, not in a ceiling.

**What was measured.** A 5 000-result answer returns the full set in exactly the order an
in-memory oracle produces. Mutation (restore the 2000-key refusal) fails with *"a 5000-result
answer reported itself truncated"*. A one-byte spool store, so that the header alone exhausts it,
still serves page 1; mutation (restore the failure) fails with *"a full spool failed page 1 instead
of ending it"*. Chunking: mutation (restore the record-size refusal) fails with *"a record larger
than one frame was refused instead of chunked"*.

**The duplicate-primitive incident, and the one-primitive resolution.** Two lanes, working in
parallel on different problems — the planner's input sort and the search top-K — each independently
created a bounded external sort at the *same path* in the pagination package. Both are correct;
they differ in that the second adds a capped merge fan-in (excess runs are collapsed in groups
first, so the live set stays constant instead of growing with the input) and derives its run budget
from the query memory admission. The resolution was one primitive, not two: the planner's sorter
was extended with the capped fan-in, a collapse that removes each group's inputs as the group
completes (so peak temporary disk stays at the run set plus the one run being written), a run byte
budget derived from the query memory admission, a fold applied exactly once over the fully ordered
stream (which is what keeps the planner's digest byte-identical: runs are stable-sorted and ties break
by run index over runs written in arrival order), and a live-record high-water mark for the
structural memory assertion. The second implementation was not merged. Search's deduplication and
ranking now stream through this one primitive.

**Consequences.** The ranked set and the candidate-deduplication set are no longer heap-resident in
proportion to the answer or to the range scan: two sort passes (deduplicate and fold, then rank) feed
one page and a streamed spool tail, so peak heap on this path is the run budget plus the merge fan-in's
blocks plus one page, independent of the match count. Measured live-record high-water mark: 190
records at 20 000 candidates and 189 at 200 000; the mutation that removes the run budget raises it
to the match count (20 000 and 65 536) and the assertion fails. Two residuals are named rather than
claimed: a continuation page still materialises the whole spooled tail it was handed before
re-spooling the remainder (the fix is to read one page and stream the rest into the next spool), and
per-request sort runs are not charged to the temporary-bytes reservation, though they live under the
same spool directory and are counted as real disk by the sweeper.

### 2.5 Field bounds truncate and flag

**Decision.** A field-length bound never fails a unit. Over-long values are truncated through one
shared helper and the cut is published as its own attribute — the field name and the original
length — separate from result truncation. The ceilings keep their values; only the response to
exceeding one changed, from refuse to cut-and-flag. Where a provider's record carries an
over-limit *identifying* field, the record is dropped and counted; where the field is decorative,
the field is dropped and counted.

**Alternatives considered.**

*Raise the ceilings instead of truncating.* Rejected: there is no value that generated code cannot
exceed. A qualified name here is the package joined with every enclosing scope, so generated
protocol definitions and minified sources clear any fixed number routinely, and a raised ceiling
is the same defect with a larger constant.

*Share one truncation flag with result truncation.* Rejected explicitly. Index-time truncation is
**permanent**; result truncation is transient and fixed by following a cursor. Conflating them
makes "did I get everything?" unanswerable — which is precisely the failure the published
discussion of silent-drop versus truncate-and-index behaviour turns on [S17], and the same line of
reasoning that governs an index's immense-term rejection [S17b].

*Truncate the identifier to its first N bytes and use that as the key.* **Rejected on
correctness.** A qualified name is an identity, not a display string. Two generated symbols sharing
a 2048-byte prefix would collide into one node. The truncated value is therefore never published
as an alias: it is omitted and counted, and the node resolves under a **file-local key** instead.
The cut value is used as a key only as a last resort, when the file-local key itself does not fit.

**Why it was chosen.** This was the single highest-severity site in the tree. A declaration whose
name exceeded its ceiling was rejected as invalid provider output, and that rejection propagated
into a **dead unit — the whole file published nothing** — with no test coverage anywhere near it.
One over-long generated symbol silently erased a file from the index.

**What was measured.** A declaration with a 3 000-byte qualified name now succeeds: the stored
value is within the ceiling, the truncation attribute records the original 3 000 bytes, and the
native key is the file-local form rather than the cut prefix. Mutation (restore the two length arms
of the old guard) fails the unit with *"declaration name is empty or not UTF-8"*. On the import
path for precise-index payloads, an over-limit occurrence symbol now drops that record and counts
it while an in-bound sibling still publishes; mutation (restore the abort) fails the whole import
with a typed resource-limit error.

**The degradation surface.** Truncation is only honest if it reaches an operator. A record-size
bound had no reporting channel at all — the batching sink had no degradation counter, unlike the
structural and filesystem providers — so one was added: a counted degradation folded into the
unit's capability row before validation, carrying the bound name, the count and the largest
observed size. Two consequences are recorded as overstatements rather than hidden: the row is
marked *partial* rather than *fresh*, because a fresh row's details are cleared on the way into the
generation and state is the only channel that survives the fold; and a provider already carrying
many detail keys can have the degradation dropped by the per-row detail cap.

**Consequences.** Nothing fails a unit for the size of a field. Peak memory is unchanged —
truncation is itself what bounds the row. Three residual cuts are declared rather than silently
accepted: a joined documentation body whose parts each fit but whose join does not is still cut
without a flag; a search-side body bound writes its flag in a phase later than the attribute is
read; and one provider-side derived-row ceiling remains a hard refusal, outside the converted set
and routed onward.

### 2.6 Process governance: progress, admission, and caps that bound buffers

**Decision.** Three changes, one principle — a bound may shape work, never reject it.

1. **Stall detection replaces the wall clock** (see §2.1): a progress counter on raw bytes read,
   plus process-tree CPU where the platform allows sampling, with a watchdog that terminates only a
   tree that has made no progress for the stall window.
2. **An output cap bounds the capture buffer, not the run.** Previously, crossing the cap
   terminated the process tree and failed the unit — *a stream that crossed its limit is still a
   refusal*. Now the bytes past the cap are dropped from the capture and flagged; the run completes.
   A stream that is discarded outright needs no cap at all, and one delivered to a caller's writer
   is never truncated: that writer's blocking write **is** the backpressure.
3. **Admission waits instead of refusing.** A run that does not fit the remaining budget joins a
   FIFO queue on concurrency and bytes together, and is promoted when headroom appears.

**Alternatives considered.**

*Keep the wall-clock timeouts and simply raise them.* Rejected: any fixed number is a refusal for
some monorepo unit, and the failure it produces — a failed analysis unit — is one the ruling names
explicitly.

*A bare semaphore for admission.* Rejected as insufficient. A bare semaphore is backpressure, not
admission: the queue behind it must itself be bounded and block on enqueue, or the out-of-memory
condition simply moves into the queue. The previous code also took a concurrency slot *before*
checking the byte budget, so waiters could each hold a slot while waiting for memory that only a
running child could free, and no child could start. Concurrency and bytes are now taken together
under one lock, or not at all.

*Inherit the memory limit from the container runtime rather than admitting explicitly.* Rejected on
two documented facts. The runtime's soft memory limit counts system memory minus released heap and
needs 5–10 % headroom; set too low it thrashes and then exceeds itself anyway [S18]. And a
*subprocess's* resident memory is entirely invisible to it, so a managed analyzer's whole allocation
must be deducted from the parent budget and handed to the child as a percentage-of-RAM parameter
rather than a fixed heap [S19]. There is still no runtime-native way to derive that limit from the
container's own cgroup [S20], so the admission is explicit by necessity, not by preference. The
design that buffer-managed systems converge on — memory owned by one manager that spills adaptively
rather than failing — is the same shape [S12].

**Why it was chosen.** Head-of-line admission is the property that matters: promotion stops at the
first waiter that does not fit, so nothing behind the head consumes the headroom the head is
waiting for, and a large reservation cannot starve behind small ones. The head is always eventually
satisfiable because its reservations are individually within the whole budget — checked before
enqueue, and the one remaining refusal — and every admitted run returns everything it took.

**What was measured.** A stalled child with no wall clock is terminated with the reason `stalled`;
a child echoing every 50 ms for 20 rounds finishes untouched. Two mutations: disarming the watchdog
lets the stalled child run out the test at 30 s; making the progress probe return zero kills the
*working* child — proving the detector is progress-driven and not a wall clock under another name.
For output caps, a child emitting three times its capture bound completes and is flagged; mutations
restore the refusal and break stream delivery. For admission, mutations restore the
does-not-fit refusal and break cancellation, and the package is race-clean at three repetitions.
Removing the kill path also cut one test package's runtime from 62.4 s to 5.6 s, because two tests
no longer wait out a wall clock for a kill that no longer happens.

**Consequences.** The accepted trade-off of head-of-line admission is that headroom can sit idle
while a large reservation waits; that is the price of never starving one, and it is stated at the
function. One refusal deliberately remains: a run whose own reservation exceeds the entire
user-set budget is refused, with both numbers in the message, because no amount of waiting will
satisfy it. Reservations themselves — worker counts, batch sizes, cache sizes — still require
positive values: a zero reservation is a broken deployment, not an unlimited one, and this is
recorded so that a later sweep for "zero means unlimited" does not file it as a defect.

### 2.7 Reports drop nothing

**Decision.** No report drops a row. The capability report is published in full and read in full;
its severity fold becomes an **aggregation** that preserves scope counts rather than a truncation;
the answer-level validation that *failed* an answer carrying more than 256 capability rows is
removed; run records past their ceiling are counted and the count is published; the diagnostic
report's silent clamp is deleted; the workspace sweep's silent slice is deleted at all three sites.

**Alternatives considered.**

*Return the capability report as a cursor page, per the plan's own memory column.* Rejected on
call-site evidence: the only consumer computes health and completeness over the **whole** set, so
paging only moves the loop, and page-sizing it properly needs the status surface to fold health
incrementally and the status wire type to gain a paged completeness contract. The honest statement
is that the returned slice is now proportional to the total capability row count rather than capped
at 256 — a real change in shape, recorded as an open concern, rather than a paging claim the code
does not support.

*Keep the severity fold and accept that the loser's counts vanish.* Rejected. The fold kept only
the most severe of two rows for one provider capability, so the discarded row's scope count was
lost while the omitted counter stayed at zero — a drop that reported itself as no drop. The review
that found it rated it Minor on two checks (health can only worsen under the fold, so nothing
over-claims; and the omitted counter has exactly one consumer) and it was fixed anyway.

**Why it was chosen.** The read path was the decisive site: a bare `LIMIT 256` silently dropped the
tail of the capability report, and that report feeds the completeness field on **every** answer.
A silent drop there makes every answer's completeness claim unfounded. On the write path, the same
ceiling refused to *publish the generation at all* on a large repository, which makes the index
unusable rather than merely incomplete.

**What was measured.** 300 distinct provider/capability/scope triples publish and read back
whole. Mutation A (restore the publish refusal) fails with a typed argument error; mutation B
(restore the read `LIMIT`) fails with *"returned 256 rows, want all 300; the read path dropped the
tail"*. The aggregating fold: mutation C (keep only the winner's count) reports `scopes="300"`
where `301` is correct; mutation D (use the plain fold after the collapse) reports `scopes="1"`.
Run records: mutation E (drop the total counter) reports an omitted count of `-1000` instead of 5.

**The primary-key fold.** The summing variant is applied **only after** scope collapse. Before the
collapse, two rows on one primary key are two assertions about the same scope — a composition-time
row meeting a stored one — and summing them would double-count. These details also feed a published
binding key, so changing the fold on the ordinary path would move published identities. The plain
fold is therefore left byte-identical and the summing fold is a separate function, with a mutation
guarding the split.

**Consequences.** One report bound remains unconverted and is named: an observation-reference count
that cannot be expressed as unlimited without a configuration key that does not exist. Stripping
its enforcement without adding the key would leave an inbound request unbounded with no operator
control — strictly worse than today — so it was left, with the exact three-line change recorded.

### 2.8 Storage: identity width is the amplifier *(accepted; scheduled)*

**Decision.** The store's amplification is attributed, not guessed, and the redesign follows the
attribution: **integer surrogate identities and interned keys**, with bytes-per-indexed-symbol as
the primary regression metric. The 3.5× ratio budget is re-affirmed as a release gate **only after**
it is re-measured on the corrected reference corpus and on real repositories.

**What the measurement proves.** Per-b-tree byte attribution [S2][S2b] over the full store gives
four families at roughly 20 % each — relations 22.5 %, evidence 21.6 %, alias rows 20.2 %, nodes
20.0 % — and search at 9.5 %. One table is the clean proof. Its rows cost 117 B each for about
100 B of content; one secondary index costs 117 B/row, *as much as the table*, and a second costs
**121 B/row, more than the table**. That is 355 B/row of storage for 100 B of content.

The mechanism is confirmed from the file format specification, not inferred: an index b-tree key is
the indexed columns followed by the key of the corresponding table row, and for a table declared
without a rowid, that row key is the **whole primary key**; a primary-key column already present in
the index is not repeated [S23]. With a four-column wide primary key, each of those two indexes
therefore stores all four columns — a whole copy of the table — which reproduces the measured
numbers exactly. The rule is: **a rowid-less table with a wide primary key and any secondary index
is the amplifier by construction**, and the format's own guidance already excludes this shape
("do not store large strings or BLOBs"; average row under ~200 B at a 4 KiB page) [S3].

**Alternatives ruled out by measurement — recorded so they are not revisited.** Full-text content
duplication (the search index is already external-content, and its tuning levers [S1] are therefore
irrelevant here); per-unit JSON payloads (16 B average); an un-checkpointed write-ahead log
(24 752 B); page size and fill, including vacuum settings [S4][S5] (zero free pages at a 4 KiB
page); evidence fan-out (1.007 rows per fact); carried membership copies.

**Alternatives ruled out by call site.** Dropping either of the two name indexes — both are
load-bearing for equality and prefix-range filters. Reordering the alias table's primary key to
drop one index — four separate sweep paths filter on the leading column, so the reorder turns a
cascade into a table scan, and adding the column back as its own index re-stores the full primary
key and returns the bytes. Converting the alias table to a rowid table with a unique auto-index —
measured net-neutral, 5.07 → 4.96 MB. The index that costs more than its table **cannot be
dropped** either: an orphan sweep runs a not-exists check through it on every pass. That is
precisely why the surrogate is the only available fix.

**Why surrogates.** A 32-byte identity costs about 33 B as a record field; a small integer costs
4 B; and a single-column integer primary key costs **zero payload bytes**, because it *is* the
b-tree key, and looks up faster than any other primary key [S23][S24]. Across roughly fourteen
indexes that is an order-of-magnitude change at every reference site, with the canonical identity
stored once. Interning the wide text keys is the same move for strings: a dictionary maps string to
ordinal and only ordinals appear in the bulk structures, which is how mature text indexes have
always been built — one published figure holds 9.8 M terms in 69 MB, roughly 7 B per term amortised
[S30].

**This is evidence, not imitation.** Mature fact stores converge here independently: a leading open
fact store gives every fact a 64-bit integer that identifies it *within one database*, scopes that
id explicitly to that database, has facts reference facts by it, and deduplicates in the storage
backend [S26] — which is the surrogate design including the storage-internal-only rule, arrived at
separately. The named failure of a previous-generation index format was its identity encoding,
which "heavily relies on opaque ID numbers", with slow performance from large in-memory structures;
its replacement measured 4–5× smaller, and its own database converter stores high-cardinality leaf
data opaquely as a blob rather than as rows, with a further 50 % cut from flattening nested
messages [S28]. Another mature design stores entries once in sorted first-normal-form and leaves
query indexing to the consumer, and explicitly sanctions hashing wide signatures down to
fingerprints [S29] — a lever *not* taken here, because it would change canonical identities and
therefore determinism, and it is recorded as the next one if the redesign misses.

**Why bytes-per-symbol is the metric.** The cost is per identity, so it scales with symbols, not
source bytes: the same symbol count spread over 14× more source moves the *ratio* by roughly that
factor while the per-symbol cost does not move at all. Measured at ≈3.4 KB per indexed symbol
today; 3.5× on the original corpus would imply ≈158 B per symbol. Cross-system calibration brackets
the target without settling it: a whole-corpus positional-trigram index measures about 3–3.5× of
corpus size, but it also stores a copy of the content and is a substring index, not a fact store
[S21]; the like-for-like pair is an integer-id fact store at 0.8 GB against a normalised-SQLite
code-fact store at 5.2 GB on the same corpus — **6.5×**, the same direction and magnitude as the
gap being closed [S27]. Neither says 3.5× is right for this product. Only re-measurement can.

**Determinism and migration.** Canonical hashes exclude operational identifiers by specification,
so a rowid surrogate is not merely permitted, it is the correct shape. Surrogates are
storage-internal: the canonical identity stays on the wire, and a signed cursor must encode the
canonical identity, never a surrogate, because a surrogate is meaningful only inside one store and
one rebuild. Migration is a rebuild: the schema fingerprint changes and the existing mismatch path
already fails closed with an explicit rebuild instruction. Memory is unchanged — interning is a
**bounded cache flushed with each batch**, never a whole-repository dictionary in heap.

**Honest projection.** Every trim the measurement supports — interning, dropping a constant column,
hex text to binary, the evidence primary key — reaches ≈38×, and the surrogates take ≈80 MB more,
landing near **≈24× on the original corpus**: still far from 3.5×. Either the corrected corpus
closes most of the remainder, or 3.5× is the wrong number for a store that keeps full evidence and
provenance per fact. That is the open question the re-measurement answers.

**Why this is a separate wave.** Storage amplification is a **disk** cost; the rest of this record
is about resident memory and refusals. At today's per-symbol cost a 300 000-file repository
projects to a multi-gigabyte store — bad, but not an out-of-memory failure on the 47 GiB reference
host. Blocking the unlimited posture on a 10 000-line package rewrite would trade a measured,
reported risk for a long stall. The verification lane for this record therefore **reports** the
ratio and the per-symbol cost; the storage wave's own verification is where they gate.

### 2.9 Capsule pagination *(accepted; scheduled)*

**Decision.** The sealed context capsule's lists become **durable, per-list rows** written inside
the seal's own write transaction, with the capsule blob keeping identity and per-list counts only,
a paged read surface, and the canonical hash computed over three bounded passes of the source. This
did not land in the current wave. The current release therefore ships with the capsule's
250 000-file coverage ceiling and its record bounds **still in force** — the one known remaining
default cap, documented as such.

**Why it did not land, stated plainly.** Three separate implementation lanes reached this item and
each declined to half-land it, for three different and cumulative reasons. The first found that the
mechanism named in the plan was wrong: a continuation spool is lease-bound and expiry-stamped,
while a sealed capsule is a write-once durable artefact replayed by a later session — the
discriminating question being whether a capsule read after its lease expired still serves pages,
and through a spool it does not. The second found that stripping the count bounds *without* the
durable rows produces exactly the unbounded heap-resident capsule the second binding ruling
forbids — and, worse, that such a change would have **passed** a 300 000-file proof on a 47 GiB
host while violating the ruling. The third measured the blast radius and found the minimum
footprint spans the workflow, server, command-line and storage layers plus five test files that
carry the byte-identity and replay evidence, which must be rewritten in the same commit or the gate
is red on a half-landed capsule.

Two design facts were established on the way and are load-bearing for the scheduled work. The
store opens a single-connection writer pool and a separate reader pool under write-ahead logging,
so a read issued from inside the write callback takes a *reader* connection and cannot deadlock —
which means the rows can be written inside the seal's own transaction, keeping the seal genuinely
write-once and removing a clobber race entirely. And the hash cannot be computed in two passes as
first proposed: the digest emits each list's length *before* its elements, and the digest itself is
a validated field of the sealed record, so the rows cannot be written before the hash exists.
Three bounded passes over the source — count, hash, then write — are byte-identical to today's
digest. Because no stored golden digest existed anywhere in the tree, two reference digests were
computed and pinned so the change can be proved rather than asserted.

**Consequences.** Shipping with this cap is a deliberate, recorded exception to the ruling, not an
oversight. The scheduled shape is one interface lane (schema rows, the writer and reader inside the
seal transaction, the counts on the record, page validation), then two parallel fill-in lanes, then
one integration lane that runs the three proofs: the large seal-and-page with union equal to the
whole, the restored-refusal mutation, and the single-pass-hash mutation.

---

## 3. Consequences

### 3.1 What bounds peak memory now

| Path | Bound |
|---|---|
| Workspace capture | One directory level's entries; the walk retains only the current path's directory chain |
| Index planning | Sort run buffer + fan-in × block size; the plan's own unit list remains and is named |
| Graph traversal | Resumed frontier (frontier byte budget) + this page's admissions (page items) + one level's probes + one page of relations + one spool record |
| Providers | One batch (batch records / batch bytes) + the per-sink pool reservation; truncation bounds each row |
| Search | Distinct results in the answer, plus the deduplication set for a range scan — **both open** |
| Process tree | Fixed capture buffers; admission reservations taken together under one lock |
| Storage writes | One batch; interning is a bounded cache flushed per batch |

### 3.2 The safety nets, and how they are tested

Four mechanisms now carry the load the caps used to carry, and each is exercised by a test named
for the failure mode it protects, not for the mechanism:

- **The query deadline** is the only remaining stop on an unbounded walk, and it must end a *page*.
  A deadline that returns "truncated" with no cursor is a class-E defect by definition.
- **Memory admission** serialises and defers; only an explicit non-zero user ceiling rejects
  anything, and then with both numbers in the message.
- **Disk-backed spools** replace every heap-resident whole-repository set, bounded by the temporary
  byte budget less the free-disk reserve, and their exhaustion is a reported, resumable page end.
- **The stall detector** is the hang detector that removing the wall clock required.

The test rewrite rule applied throughout: a test asserting *"N is rejected because it exceeds the
cap"* becomes *"N is accepted, and the report says a user-set limit of M was exceeded"*. A test
asserting only the numeric value of a default is deleted with that default. No test is added that
duplicates an existing assertion.

### 3.3 What is still open

- **The capsule's coverage ceiling** is the one known remaining default cap (§2.9).
- **One reference-scale miss** stands: storage at **16.10× against 3.5×**, the storage wave's to
  close (§2.8). The indexing peak, 912.6 MiB when first measured, re-measured at 145.3 MiB against
  the 768 MiB envelope after the planner's external merge and the batched provider sinks landed.
- **Search heap**: the ranked set and the deduplication set now stream through the one external sort
  primitive (§2.4), so neither is proportional to the match count. What remains on this path is the
  exact-tier candidate set, which holds one node identity per distinct exact candidate and is
  therefore still proportional to a short prefix query's range scan.
- **Two whole-walk callers** — impact and rollup — still expand a walk in one request (§2.2).
- Smaller residuals are recorded at their sites: an undisclosed cut on a joined documentation body,
  and a clamp notice that is recorded but still has no reader on the context and adjacency query
  paths -- neither of them builds the answer metadata the notice would travel on, so carrying it
  needs a field on the context manifest and an installation at the graph engine's own metadata
  sites. The provider-side derived-row refusal is closed. The observation-reference count is a
  user-set bound at the service boundary but the wire contract still refuses more than 64 references
  on a single observation, so the aggregate path is unlimited and the single-observation path is
  not.
- **The traversal reads that unlimited defaults have now unbounded in heap**: the repository map's
  containment read accumulates one page of containers' children in one slice, and the shortest-path
  walk holds its settled set, distances and read edges for the walk. The finite defaults used to
  bound all four; the page bound now covers only the containers, not the children. Each needs the
  same treatment the ranked set got -- a keyset-paged containment read and a spilled frontier --
  and until then their peak is a function of one container's fan-out rather than of a page.

### 3.4 What verification on real repositories must show

The verification pass runs fully offline with an isolated cache and configuration, cold, over four
real repositories of 13 223, 6 270, 5 432 and 4 019 tracked files, plus one synthetic 300 000-file
corpus whose only purpose is to prove that the removed workspace ceiling no longer refuses: it must
index to completion. For each it reports wall clock, sampled process-**tree** peak resident memory,
the storage ratio and bytes per indexed symbol. The gate is: no out-of-memory failure on the 47 GiB
host, and a peak bounded by page and batch size rather than by file count. The two storage numbers
are **reported here and gated in the storage wave**.

---

## 4. Sources consulted

Every source cited above appears here; every source here is cited above.

[S1] https://www.sqlite.org/fts5.html — full-text `detail=`, `content=`, `contentless_delete` and
`prefix=` storage costs; the levers ruled out once the search index proved to be external-content
(§2.8).

[S2] https://www.sqlite.org/dbstat.html — per-b-tree byte attribution in aggregate mode; the
`SUM(pgsize) GROUP BY name` method behind every per-table number in §2.8.
[S2b] https://sqlite.org/sqlanalyze.html — the packaged space report built on the same view.

[S3] https://www.sqlite.org/withoutrowid.html — single-b-tree tables, the row-size rule of thumb
and the explicit warning against wide keys; the guidance the amplifying table shape violates
(§2.8).

[S4] https://sqlite.org/pragma.html — `page_size`, `auto_vacuum`, `incremental_vacuum`,
`journal_size_limit`; the page-geometry levers measured and ruled out (§2.8).

[S5] https://phiresky.github.io/blog/2020/sqlite-performance-tuning/ — write-ahead logging with
relaxed synchronous mode and deferred index creation, benchmarked; the tuning family ruled out
against a 24 752-byte log (§2.8).

[S6] https://rocksdb.org/blog/2017/02/17/bulkoad-ingest-sst-file.html — build sorted output once,
then ingest it cheaply; the pattern (not the engine) behind the planner's run-and-merge shape
(§2.3).

[S7] https://www.usenix.org/system/files/conference/fast15/fast15-paper-zheng.pdf — semi-external
breadth-first search with vertex state in memory and selective frontier edge I/O; the traversal
family chosen (§2.2).

[S8] https://arxiv.org/abs/1907.03335 — why full-edge-scan graph processing designs are the wrong
family for frontier-bounded search; the alternative steel-manned and rejected (§2.2).

[S9] https://arxiv.org/pdf/2507.12925 — minimum-memory semi-external breadth-first search;
corroborates the bounded-front design (§2.2).

[S10] https://arxiv.org/pdf/1402.6407 — roaring bitmaps, per-chunk container selection; the visited-set
alternative considered (§2.2).

[S11] https://github.com/RoaringBitmap/roaring — the implementation with zero-copy deserialisation
that made the bitmap attractive; rejected here for want of a dense id space (§2.2).

[S12] https://duckdb.org/2024/07/09/memory-management — buffer-manager-owned memory with adaptive
spilling through the same code path in and out of core; the admission shape (§2.6).

[S13] https://google.aip.dev/158 — page tokens must be opaque and server-minted, with request
arguments constant across pages; the cursor contract (§2.2).

[S14] https://www.elastic.co/docs/reference/elasticsearch/rest-apis/paginate-search-results —
keyset continuation plus a point-in-time view, and why offset and scroll pagination cost memory;
the deep-retrieval evidence behind §2.2 and §2.4.

[S15] https://lucene.apache.org/core/8_5_2/core/org/apache/lucene/util/OfflineSorter.html — an
explicit external-sort bound of partitions-in-RAM × buffer size; the auditable bound adopted in
§2.3 and §2.4.

[S16] https://dx.doi.org/10.1145/276304.276346 — Larson & Graefe, replacement selection roughly
doubles average run size; the fan-in lever **not** taken (§2.3).

[S17] https://www.elastic.co/docs/reference/elasticsearch/mapping-reference/ignore-above and
https://github.com/elastic/elasticsearch/issues/60329 — silent drop versus truncate-and-index, and
why the two truncation flags must stay distinct (§2.5).
[S17b] https://issues.apache.org/jira/browse/LUCENE-10013 — the immense-term rejection, the
rejecting shape this product moved away from.

[S18] https://go.dev/doc/gc-guide — the soft memory limit counts system memory minus released heap,
thrashes when set too low, and wants 5–10 % headroom; why the runtime limit is not the admission
(§2.6).

[S19] https://www.baeldung.com/java-jvm-parameters-rampercentage and
https://developers.redhat.com/articles/2022/04/19/java-17-whats-new-openjdks-container-awareness —
percentage-of-RAM heap sizing and off-heap overhead; how a managed analyzer subprocess's allocation
is handed down (§2.6).

[S20] https://github.com/KimMachineGun/automemlimit and https://github.com/golang/go/issues/75164 —
cgroup-aware memory detection is still third-party; the runtime has no cgroup-derived limit (§2.6).

[S21] https://sourcegraph.com/blog/zoekt-memory-optimizations-for-sourcegraph-cloud and
https://github.com/sourcegraph/zoekt/blob/main/doc/design.md — memory-mapped shards off-heap and
the ~3–3.5× corpus-size figure for a content-storing substring index; one bracket on the storage
gate (§2.8).

[S22] https://fulmicoton.com/posts/behold-tantivy-part2/ — a memory budget as a first-class
parameter, divided per worker with a floor; the "share, not a new key" reasoning in §2.4.

[S23] https://www.sqlite.org/fileformat2.html — §2.5 an index key is the indexed columns followed by
the table row key, which for a rowid-less table is the **whole primary key**; §2.5.1 a primary-key
column already in the index is not repeated; §2.1 serial types, giving a 32-byte blob ≈ 33 B and a
small integer 4 B. The primary-source confirmation of the §2.8 mechanism.

[S24] https://www.sqlite.org/lang_createtable.html — a single-column integer primary key aliases the
row id: zero payload bytes, and roughly twice the lookup speed of any other primary key (§2.8).

[S25] https://glean.software/blog/incremental/ — fact-to-owner sets as compressed integer sets plus
an interval map, at +7 % database size and 2–3 % index-time overhead; the published analogue for
the second-order evidence change held in reserve (§2.8).

[S26] https://glean.software/docs/angle/advanced/, https://glean.software/docs/introduction/ and
https://engineering.fb.com/2024/12/19/developer-tools/glean-open-source-code-indexing/ — 64-bit
database-local fact identifiers, facts referencing facts by identifier, storage-backend
deduplication; independent convergence on the surrogate design (§2.8).

[S27] https://simonmar.github.io/posts/2025-05-22-Glean-Haskell.html — the same corpus in an
integer-identifier fact store at 0.8 GB against a normalised-SQLite code-fact store at 5.2 GB
(6.5×); the like-for-like bracket on the storage gate (§2.8).

[S28] https://sourcegraph.com/blog/announcing-scip,
https://raw.githubusercontent.com/sourcegraph/scip/main/scip.proto and
https://raw.githubusercontent.com/sourcegraph/scip/main/docs/CLI.md — opaque-identifier graph
encoding named as the predecessor format's failure, a 4–5× size cut, −50 % from flattening nested
messages, and high-cardinality leaves stored opaquely rather than as rows (§2.8).

[S29] https://kythe.io/docs/kythe-storage.html, https://kythe.io/docs/schema/ and
https://raw.githubusercontent.com/kythe/kythe/master/kythe/proto/serving.proto — entries stored once
in sorted first-normal-form with query indexing left to the consumer, structured names, sanctioned
signature hash-truncation, and paged edge sets (§2.8).

[S30] https://github.com/quickwit-oss/tantivy/blob/main/ARCHITECTURE.md and
https://blog.mikemccandless.com/2010/12/using-finite-state-transducers-in.html — a term-to-ordinal
dictionary with ordinal-only postings, holding 9.8 M terms in 69 MB (~7 B/term); the interning
evidence in §2.8 and the streaming-collector evidence in §2.4.

[S31] Munagala, K. and Ranade, A., *I/O-complexity of graph algorithms*, SODA 1999 —
level-batched duplicate elimination in external breadth-first search; why membership is answered
per level rather than per node (§2.2).

[S32] Mehlhorn, K. and Meyer, U., *External-memory breadth-first search with sublinear I/O*, ESA
2002 — a bounded front in memory with the bulk outside it; the semi-external shape adopted (§2.2).

[S33] Knuth, D. E., *The Art of Computer Programming*, vol. 3, §5.4.1 — replacement selection and
bounded merge order; the classical statement of the lever recorded but not taken (§2.3).

[S34] https://pkg.go.dev/net/http/httptrace#WithClientTrace — a request-scoped observation collector
carried on the context rather than through every function signature; the precedent for reporting a
clamp without changing two dozen reader signatures (§2.1).

---

*The measurements quoted in this record are drawn from the implementation and verification reports
of 2026-09-13 and 2026-09-14, each of which carries the mutation proof for the assertion it
supports.*
