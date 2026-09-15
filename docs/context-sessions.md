# Context sessions: the strict actor workflow

A context session answers one question honestly: **which bytes did this actor
actually read?** Not which files were offered, not which files were opened — the
bytes a signed receipt proves were delivered and confirmed.

Everything below is one command group, `codectx context`, driven in the order
the subcommands are registered: plan, learn what is outstanding, read it,
confirm it, review it, advance, seal, close.

## The shape of a session

```text
plan ──► status / next / entries ──► read ──► acknowledge --receipt
             ▲                                      │
             └──── include (discovered scope) ◄──────┤
                                                    ├─► acknowledge --file-review
                                                    ├─► waive --reason
                                                    └─► record (observations)
                                        advance ──► capsule / export ──► close
```

## Plan: an immutable manifest pinned to one snapshot

```sh
codectx context plan \
  --task "Add retry semantics to PaymentService.Authorize" \
  --phase sweep --actor lead-session-1 --repo .
```

`--task` is required and is stored in the actor's own words: it is part of the
manifest's identity, so the same task text over the same generation compiles the
same manifest. `--phase` is `sweep` (gather) or `verify` (confirm);
`consolidate` is not plannable and is reached only with `context advance`.

`--seed` (repeatable) names a path or symbol the plan starts selection from.
The four budget flags — `--budget` (estimated tokens per slice),
`--budget-bytes`, `--budget-files` and `--budget-slices` — bound one slice and
the whole plan. They are **caller budgets, not scale refusals**: they exist
because a plan must fit a model's window, which is why they are the one family
of settings that keeps a non-zero default, and a request may raise them above
that default as well as lower it. They stay honest because a plan that cannot
fit its scope inside those bounds says so, and because every file a budget
excludes is named in the manifest with its reason, paginated and never
summarised. Nothing is silently dropped: a truncated manifest presented as
complete is the one failure this whole surface exists to prevent.

The manifest is pinned to the generation it was compiled from. Later indexing
does not move it.

## Learn what is outstanding

```sh
codectx context status  <session-id> --actor lead-session-1
codectx context next    <session-id> --actor lead-session-1
codectx context entries <session-id> --actor lead-session-1 --view entries
```

`status` reports this actor's coverage. **Read completeness and strict
implementation readiness are separate questions**, and the answer keeps them
separate: a waived file is reported as waived, never rolled up as covered.

`next` names the single next required file this actor has not fully read — the
loop primitive for an agent that wants to be told what to do rather than diff
two lists.

`entries` pages one projection of the current manifest.

## Read: bounded, lossless chunks of the pinned source

```sh
codectx context read <session-id> <file-id> --actor lead-session-1 \
  --offset 0 --max-bytes 65536
```

Bytes come from the snapshot the session pinned, **never from the working tree
as it stands now**. Chunk boundaries are lossless: CRLF endings and a final line
without a newline survive re-assembly. A continuation passes the next offset the
previous chunk reported.

Each chunk prints a **signed receipt**. Printing it is not delivery. `read`
never confirms its own receipt — there is no service call after the bytes are
written, so a failed write fails the command with nothing confirmed, and a
successful write is still not treated as delivery on its own.

## Acknowledge: the only thing that creates coverage

```sh
codectx context acknowledge <session-id> --actor lead-session-1 \
  --receipt <token> --receipt <token>
```

Echoing the receipt back is what turns issued bytes into served coverage. It is
idempotent — replaying a receipt adds credit once. A chunk that was issued but
never confirmed (a broken pipe, a dropped connection, an agent that stopped
reading) grants **no credit at all**.

For convenience the read side can confirm the previous chunk in the same call:
`codectx context read … --confirm-receipt <token>` confirms inside the service
*before* the next chunk is serialized.

A full-file review is a different assertion and is never interchangeable with
delivery:

```sh
codectx context acknowledge <session-id> <file-id> --actor lead-session-1 --file-review
```

It is refused unless that file is already fully served for this actor. A review
attests to coverage; it never creates it.

## Include: scope discovered while reading

```sh
codectx context include <session-id> --actor lead-session-1 \
  --seed internal/payments/retry.go --expected-version 5
```

Adding scope bumps the session's scope version. That version is what the
observation and transition guards below check, so scope that grew after an actor
formed a conclusion cannot be passed off as scope they saw.

## Waive: an auditable exception, not a shortcut

```sh
codectx context waive <session-id> <file-id> --actor lead-session-1 \
  --reason "generated protobuf stubs; contract reviewed at the .proto"
```

`--reason` is required and is stored verbatim in the capsule. A waiver does not
make the file covered — it makes it *waived*, and every readiness answer keeps
saying so.

## Record: what the actor concluded

```sh
codectx context record <session-id> --actor lead-session-1 \
  --kind accept_fact --expected-scope 3 \
  --relation <relation-id> --node <node-id> \
  --note "Authorize calls the retry wrapper, not the transport directly"
```

`--kind` is `accept_fact`, `reject_fact`, `contradiction`, `unresolved` or
`scope_review`. `--expected-scope` is the scope version the observation is about
as `context status` last reported it; if scope moved since, the observation is
refused rather than silently attached to a scope the actor never saw. Cited
relations, nodes and sources are recorded with it, and `--note` is stored
verbatim as part of the observation's identity.

## Advance: guarded, version-checked transitions

```sh
codectx context advance <session-id> verify --actor lead-session-1 --expected-version 7
```

Targets are `verify`, `consolidate` and `complete`. Each transition is guarded
by the session's state and by `--expected-version`: a session that moved under
you fails the transition instead of applying it to a state you did not read.

The guards are what make the readiness claim mean something — `complete` is
reachable only when every required file has confirmed coverage or a recorded
waiver.

## Capsule, export and close

```sh
codectx context capsule <session-id> --actor lead-session-1 --view accepted_facts
codectx context export  <session-id> --actor lead-session-1 --output ../session-7.json
codectx context close   <session-id> --actor lead-session-1 --expected-version 9
```

The capsule is the deterministic completion record: what was planned, what was
served, what was reviewed, what was waived and with which reason, and every
observation with its citations. `capsule` pages one projection of it; `export`
writes the whole sealed capsule to a file **outside the repository**, and never
overwrites an existing file. Sealing is the one place in this build where a
built-in cap can still refuse work: a session carrying coverage or waivers for
more than 250 000 files, or a capsule list of more than 1 000 records, is
refused with `CTX_RESOURCE_LIMIT` naming the list and the bound rather than
sealed with a truncated list. See
[the one remaining default cap](configuration.md#the-one-remaining-default-cap).

`close` takes the final version-checked transition.

## What this does and does not prove

- **It proves delivery to an actor**, by a receipt signed with this workspace's
  key. It does not prove comprehension, and it does not prove the actor was who
  it claimed to be against anyone who can reach that key — see
  [SECURITY.md](../SECURITY.md).
- **Receipts are per actor and per source hash.** There is no receipt sharing
  between actors and none across a changed file.
- **There is no waiver shortcut to readiness.** With no waiver, readiness is
  strict; with one, the waiver and its reason travel with the answer.
- **Disabling the read gate changes the reason, not the attestation.** With
  `context.strict_read_gate = false` an unfinished read no longer shuts the
  gate at that precondition, and the rest are still evaluated and reported —
  but nothing confirmed the coverage, so the session is reported neither
  `ready_for_implementation` nor `strict_gate_satisfied`, the sealed capsule
  records the strict gate false, and the answer's `guarantee_limit` begins
  `strict_read_gate=disabled` instead of naming files the configuration
  excused. Toggling
  the key also changes the context-policy fingerprint, so context answers
  compiled under the other setting are recompiled rather than reused.
- **Coverage is about the pinned snapshot**, so a session's claims stay true
  even after the repository moves on.

The configuration that bounds all of this — `[context]` budgets and `[coverage]`
receipt settings — is in [configuration.md](configuration.md). The graph facts
the planner ranks over are the same ones [queries.md](queries.md) describes.
