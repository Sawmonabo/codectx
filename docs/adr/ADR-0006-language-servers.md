# ADR-0006 — Language servers and indexers: the Python server moves to a native checker

**Status:** Accepted, 2026-09-15 · **Informs:** the toolchain lock (`internal/toolchain/tools.lock.json`),
`docs/toolchain.md`, `docs/providers-lsp.md`, `docs/providers-scip.md` · **Input:** research note
[19](../research/19-language-server-and-indexer-matrix.md) and, for the indexers, notes
[08](../research/08-scip-empirical-six-indexers.md) and [12](../research/12-incremental-scip-lsp.md).

## Context

The toolchain pins one SCIP indexer and one language server per language. The indexers were
chosen from a measured comparison (note 08 compared all six on one fixture for roles, determinism
and range encoding; note 12 measured whole-unit wall clock, memory and the smallest sound re-index
granularity). The servers were not: each was pinned as the mainstream reference implementation,
the Python server was inherited from the Python indexer, which vendors it, and no record explains
any server row. The owner asked, on 2026-09-15, whether the choice had been researched and why a
heavier server had been preferred over a faster native one. Note 19 is that comparison, and its
decisive measurement is reproduced here.

**What the overlay asks a server for.** Seven requests only: definition, type definition,
implementation, references with declarations, document symbols, workspace symbols and call
hierarchy. Four properties of the session shape decide fitness [19 §1]: every session is cold
(a private materialisation of a pinned snapshot, started lazily, stopped after an idle window);
documents never change; ranges must validate against the pinned bytes with the negotiated
position encoding; and the server's reported version is provenance for the overlay's input
digest. A server that needs a background-indexing window before `references` is complete is wrong
for a first question, not merely slow.

**Measured, cold session, one `references` request** on an 11 267-file Python repository for a
class present in 104 files [19 §2]:

| server | settle | latency | results | server peak RSS | implementation request |
|---|---|---|---|---|---|
| pinned Python server (TypeScript-on-Node) | cold | 1.43 s | **2** | 267 MB | not advertised |
| pinned Python server | after 15 s | 0.23 s | 138 | 290 MB | not advertised |
| native Rust checker (`ty` 0.0.81) | **cold** | **0.21 s** | **136** | **168 MB** | advertised and answered |
| second native checker (`pyrefly` 1.3.1) | cold | 0.20 s | **2** | 487 MB | advertised |

The ordering replicated on a 2 379-file repository. The pinned server's `references` walks only the
opened file's import closure until background workspace indexing widens it, so a cold session
answers a fraction of the set; the native checker answers the whole set at once. On the
TypeScript side the pinned server and the native port answered identically, the port about seven
times faster on the first request, but the port's binary is published only on a preview channel
with a development tag [19 §3]. For Go, Rust, Java and C/C++ there is no credible second server;
every SCIP indexer is at its newest upstream release and no alternative emitter exists for any
of the nine languages [19 §4].

**Rulings that bind this decision.** Context quality decides first: a faster server that cannot
answer whole-workspace references loses. The product works completely under defaults with nothing
to tune. codectx is greenfield, so a lock entry is replaced, not migrated.

## Decision 1 — Python: the server becomes the native checker; the indexer stays

The Python language-server entry of the lock is replaced by `ty`, pinned to an exact upstream
release with the digests upstream publishes for all six platforms, run directly (no managed
runtime), started with its server subcommand, with the same root markers and environment
allowlist as before. `scip-python` stays exactly as pinned: it is the only SCIP emitter for Python,
and canonical Python facts are unchanged by this decision.

**Alternatives considered.**

1. *Keep the pinned server and make the overlay wait for its workspace index before the first
   `references`.* Steel-man: the gap is a readiness bug, not a capability gap; after the settle
   window the pinned server answers 138 in 0.23 s, and this change is cheaper. Rejected: it turns
   every cold Python query on an 11 000-file repository into a wait of fifteen seconds or more,
   which is exactly the tuning-free, immediate answer the product promises; the native checker is
   complete at 0.21 s with no wait, at 40 % less memory, and it answers the implementation request
   the pinned server never advertised.
2. *The pinned server's community fork.* Rejected: same engine, same cold-session gap; everything
   it adds is outside the seven requests [19 §3].
3. *The second native checker.* Steel-man: it has shipped a stable 1.0, is a large company's
   default, and reports the highest typing-specification conformance of the four. Rejected on the
   measurement: it has the pinned server's cold-session gap (2 of 136) at two to three times the
   memory, so it is worse than the chosen checker and barely better than the status quo.
4. *Interpreter-hosted servers.* Rejected on distribution: they need a Python runtime the lock does
   not manage, and they resolve heuristically rather than through a type checker.

**Consequences and trade-offs accepted.** The chosen checker is pre-1.0 and warns that any two
releases may differ incompatibly; the lock pins an exact version with upstream digests, so the
product never sees an unreviewed change, and the cost is a faster pin-refresh cadence for this one
entry, each refresh re-running the overlay verification table. Two answers differ by two results
(138 versus 136) between the old and new server; the implementation lane adjudicates which two
references are the disagreement before the swap lands, and records the answer in the verification
table. After the swap Python gains the implementation request, the overlay's provider version comes
from the server's own report, and the negotiated position encoding becomes UTF-8; all three feed
the overlay input digest, so previously cached Python overlay answers are correctly a different
question. The managed Node runtime stays for the TypeScript server and two indexers.

## Decision 2 — every other row is kept, with two follow-ups recorded

Go keeps `gopls` (the reference implementation, the only server in the set with a supported
persistent cache, pin current). Rust keeps `rust-analyzer` (the only server and the only SCIP
emitter; the pin is advanced to the current weekly release as part of the same change). Java keeps
`jdtls` (the only alternative has no Gradle project model and a fraction of the surface). C and
C++ keep `clangd` (its background index is content-hash incremental and persisted; the alternative's
release cadence has fallen behind [19 §3]). TypeScript and JavaScript keep
`typescript-language-server`, with a recorded trigger: re-evaluate the native port when its
language-server binary ships on a stable, digest-published channel (announced for the 7.1 line),
because it answered identically at about seven times the speed, reports its version and its
position encoding, and needs no runtime [19 §3, §5]. The wrapper alternative is rejected outright:
same engine, more wrapper, nine months without a release.

**Indexers.** All six stay. The known cost is the Python indexer, which vendors a copy of the old
server modified to analyse everything: 84.66 s and 2.9 GB for 1 316 documents on one measured
repository, the slowest indexer in the set by an order of magnitude [12 §3.1]. No SCIP emitter
exists on either native checker; building one against the chosen checker's semantic index is
recorded as the highest-value future tool change and is not a lane-sized task [19 §4].

## What the change must show

The overlay verification table in `docs/providers-lsp.md` re-run for Python with the new server:
all seven requests answered; the cold `references` count on the 11 267-file repository equal to the
settled count; the 138 versus 136 disagreement explained; `serverInfo` and `positionEncoding`
recorded; peak server RSS at or below the measured 168 MB. The lock passes the digest check against
upstream sidecars for every platform key of the new entry.

## Sources

Research note 19 carries the full bibliography; the entries below are the ones this record relies
on, each once.

- [19 §1–§6] Language servers and SCIP indexers: was the pinned set researched, and is it still right? — `docs/research/19-language-server-and-indexer-matrix.md` (the seven requests, the cold-session measurements, the candidate matrix, the switch cost).
- [08] SCIP empirical verification: six indexers, one fixture shape — `docs/research/08-scip-empirical-six-indexers.md` (why the indexer rows stand).
- [12 §3.1, §4] Incremental SCIP and LSP — `docs/research/12-incremental-scip-lsp.md` (indexer cost baselines; how each pinned server stays incremental).
- ty: An extremely fast Python type checker and language server — https://astral.sh/blog/ty (design; pre-1.0 status).
- ty releases and README — https://github.com/astral-sh/ty/releases (exact version, per-asset digests, the breaking-change warning).
- ty documentation, Language server — https://docs.astral.sh/ty/features/language-server/ (the request list; its implementation-request entry is stale against the measured behaviour).
- Pyrefly v1.0 is here — https://pyrefly.org/blog/v1.0/ (the second native checker's status and vendor figures).
- basedpyright README — https://github.com/DetachHead/basedpyright (what the fork adds).
- Announcing TypeScript 7.0 — https://devblogs.microsoft.com/typescript/announcing-typescript-7-0/ (the native port's channel and API status).
- microsoft/typescript-go, `internal/lsp/server.go` at `typescript/v7.0.2` — https://raw.githubusercontent.com/microsoft/typescript-go/typescript/v7.0.2/internal/lsp/server.go (the port's server capabilities).
- ccls, Comparison with clangd — https://github.com/MaskRay/ccls/issues/880 (the C/C++ alternative).
- scip-python: a precise Python indexer — https://sourcegraph.com/blog/scip-python (why the Python indexer vendors the old server).
