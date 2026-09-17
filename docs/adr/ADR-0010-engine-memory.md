# ADR-0010: The engine's heap is sized to the unit's need, and the host keeps half of what it had

## Status

Accepted, 2026-09-16.

## Context

The dependence provider runs an external analysis engine, a JVM, once per unit for the parse and
once for the export, under a heap ceiling the provider chooses ([providers-dependence](../providers-dependence.md)).
Until this decision the ceiling was the unit's estimate -- 512 bytes of heap per byte of JavaScript
source, from the round-3 research -- clamped to the machine's available memory minus two gigabytes.
The first uncapped index of the 13,222-file reference repository showed what that means on a host
with 47 GiB: the 157.2 MiB JavaScript unit was given a 39.1 GiB ceiling, and the JVM grew to 17.6 GB,
twice, then 12.5, 11.1, 10.8 and 7.3 GB on the unit's parts, one JVM at a time for twenty minutes
because every reservation was the size of the machine and nothing could be scheduled beside it.
The product is meant to run beside the user's editor, browser and the agents driving it over MCP;
a run that plans to hold nearly all of the host's memory is not a run that coexists with anything.

The research already held the decisive observation: a heap ceiling is lossless everywhere the run
succeeds, and a JVM given a high ceiling grows toward it rather than to what it needs
([10-round3-empirical §4](../research/10-round3-empirical.md), observations 1 and 3: caps are
lossless where they succeed; a cap close to the live set costs time, not memory). What was missing
was the measurement for this family at this size.

## Decision

1. **The heap ceiling comes from the unit's bytes with headroom, never from the machine.** The
   JavaScript/TypeScript constant is 48 bytes of heap per byte of source (the former 512 gave the
   157 MB unit 84 GB), derived below; the other families keep their research-derived constants.
   The machine-derived allocation bounds the ceiling only when the estimate exceeds it, as before.
2. **The allocation leaves the host half of what was available when the run began.** The allocation
   the scheduler sums reservations against is the smaller of available memory minus the base
   footprint and safety margin, and half of available memory. The base footprint is itself derived
   from the machine and the configuration — this build's measured idle overhead plus the query,
   cache and queue reservations, the query slots coming from the cores — so a larger host reserves
   more for this process and offers its children less, and no core count can make the shipped
   defaults unresolvable. The share is a constant with its reason in the code, not a setting. A unit whose reservation exceeds even that runs whole at the allocation,
   as [ADR-0001](ADR-0001-scale-posture.md) and the round-3 ruling require; it is never split for
   memory ([10-round3-empirical §8](../research/10-round3-empirical.md): splitting the JavaScript
   project loses more than half of the resolved calls).
3. **Only the hard ceiling sizes a unit.** A soft ceiling under a high hard ceiling does not hold
   the JVM near its live set on the pinned engine's runtime (measured below), so the product sets
   no soft ceiling and does not rely on the collector giving memory back.
4. **Every unit discloses its reservation, its ceiling and its observed peak** in the resources
   block and in `codectx status`, so a host that is short of memory can be read from the product's
   own report rather than from the kernel's.
5. **Every heavy child is admitted against the one allocation, and nothing counts them.** The
   allocation of decision 2 is not the analysis engine's alone: engine runs, external indexers and
   language servers are all admitted while the sum of what they reserve fits it, and a child larger
   than the whole allocation runs alone rather than being refused. There is no count of concurrent
   heavy children, no per-family count and no setting for either; where the platform does not
   publish available memory the scheduler stands a conservative allocation in for the observation,
   because an admission gate with no bound is not a gate. The counts that remain anywhere in the
   product are for CPU-bound work and come from the machine's cores.

   Admission figures, from the fixture that proves the rule: a machine reporting 32 GiB available
   has an allocation of 16 GiB — `min(32 − base footprint − 1 GiB margin, 32 / 2)`, the half share
   being what binds — and admits four 4 GiB reservations at once; a fifth waits and is admitted the moment one of the four is released. The same reservation
   on a machine reporting 8 GiB has a 4 GiB allocation and one runs at a time, the first by the
   runs-alone rule rather than by the sum. Before this, `max_concurrent_heavy_analyzers = 1`
   serialised all five on both machines, and a second project's language server was refused with
   `CTX_RESOURCE_LIMIT` on a machine with room for six.

## Measurements

The 157.2 MiB (164,865,219-byte), 4,984-file JavaScript unit of the reference repository, parse
only, the whole process tree's resident memory sampled every second (cap sweep of 2026-09-16):

| heap ceiling | wall | peak tree RSS | outcome |
|---|---|---|---|
| 2 GiB | 1:08 | 4.57 GB | fails closed, heap exhausted; no graph |
| 4 GiB | 3:38 | 5.44 GB | exits 1 on a deterministic pass crash, having written an 86,247,860-byte graph; the product classes that as an engine failure and recovers by subdividing the unit |
| 8 GiB | 3:47 | 9.67 GB | as 4 GiB: exit 1 on the same pass, same graph size |
| 16 GiB | 4:53 | 14.32 GB | as 4 GiB, slower |
| none (default) | 3:41 | 10.70 GB | as 4 GiB |

No ceiling from 4 GiB up completes this unit: every one of them reaches the same deterministic
pass crash, at the same point, having written the same graph, and its export then dies on the
partial graph at every ceiling. The unit is recovered by subdivision, not by memory — the
reference run's own ledger records exactly that outcome (`parse … scope=pkg:javascript:app …
exit_code=1 failure_class=engine`, then `span 23 dependence pkg:javascript:app … subdivided`).
What the sweep measures is therefore cost, not completion, and the rows stay comparable to each
other because every run above 2 GiB stopped at the same place: the 4 GiB ceiling reaches it at the
speed of no ceiling with half the memory, and 16 GiB is slower than 4 because the collector has
more to walk. The 2 GiB row is the one that differs in kind — it stops earlier, on heap
exhaustion, which is why its wall time is shorter and why it is the one row where the heap is
known to have been filled.

The constant 52 = ⌊2 × 4 GiB ÷ 164,865,219 B⌋ = ⌊52.10⌋, twice the smallest ceiling that ran at
full speed, the research's headroom rule (§4, observation 3). 52 × 164,865,219 = 8,572,991,388 B =
7.98 GiB, which is 1.996× the 4 GiB ceiling — the multiple the rule claims is the multiple the
code produces. The 48 this replaces was reachable only by dividing binary GiB by decimal MB
(4 GiB × 2 ÷ 157 MB = 48.5); it yielded 7,913,530,512 B = 7.37 GiB, 1.84× the ceiling, not twice.

Fact identity had to be measured elsewhere, for the reason the table above gives: the whole unit
exports nothing at any ceiling, so there is no pair of exports to compare. It was measured on the
unit's 348-file, 2.79 MiB sub-project, which does complete, parse then export at each ceiling:
every ceiling from 768 MiB to none produced a 195 MB export with identical counts -- 24,507
methods, 176,698 call edges, 81,040 control-dependence edges, 1,226,336 reaching-definition edges.
The ceiling changes cost, never facts. Peak parse RSS 1.11 GB at 768 MiB, 1.17 at 1 GiB, 1.74 at
2 GiB, 2.68 at 4 GiB, 3.65 at 8 GiB, 4.44 GB with none: the JVM takes what it is allowed.

This process's own idle overhead, the part of the base footprint that is not a stated reservation:
`codectx status` over a freshly indexed five-file, 432-byte fixture, peak resident set of the
command process ("Maximum resident set size", `/usr/bin/time -v`), three samples: **26,584 /
27,052 / 26,732 KiB**, i.e. 26.4 MiB at the worst of the three. The shipped constant is 32 MiB, the
worst sample rounded up to the next binary step. The 1 GiB that preceded it was never measured; it
is 38× the measurement, and every byte of that over-statement was taken from the children.

Soft ceiling: an 8 GiB hard ceiling with a 1 GiB soft ceiling peaked at 3.82 GB (3.65 without
the soft ceiling; 1.17 with a real 1 GiB hard ceiling); a periodic collection interval did not
move it. The soft ceiling is not honoured by the collector in use.

## Alternatives considered

- **Keep the machine-derived ceiling and let the collector return memory.** Measured not to
  happen: the soft ceiling and the periodic collection left the peak where the hard ceiling put it.
- **Split large units for memory.** Rejected by the round-3 measurement: control and data
  dependence survive a split, call resolution does not (46% of resolved calls kept), and the product
  would publish a degraded graph to save memory the ceiling already saves.
- **A user setting for the host share.** Rejected: the user should tune nothing; the share is a
  property of coexistence, stated once with its reason.

## Consequences

The reference repository's dependence phase can schedule units beside each other again, since a
reservation is now gigabytes rather than the machine; the host keeps at least half of what it had
for everything else; a unit larger than that still runs whole and says so. The constants are
measured on one engine version and one host and are re-measured when either changes; the
disclosure in decision 4 is what makes a drift visible.

## Sources

- [10-round3-empirical.md §4, §4a, §8, §10](../research/10-round3-empirical.md) (ceilings lossless
  where they succeed; a near-live-set ceiling costs time; the JavaScript project must not be split;
  the per-frontend memory model).
- [00-synthesis.md §8](../research/00-synthesis.md) (no default memory ceiling as a *rejection*
  of work; reservations schedule; subdivision never for memory).
- [ADR-0001](ADR-0001-scale-posture.md) (the scale posture the allocation rule sits under).
- Measurement record: the cap sweep of 2026-09-16 on the reference repository, reproduced in the tables above.
