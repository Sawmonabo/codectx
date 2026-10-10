# 05 — The delta's rebuild rule

All file:line references are to commit 16dfef6. The harness is `source/cmd/tbdelta/main.go`.

## Why it is not "re-feed one unit"
A sealed unit with the same identity cannot be rebuilt: `BeginUnit` answers "already sealed; attach it instead" (`internal/storage/sqlite/units.go:460`). A unit's identity is `NewUnitID` over its spec, including the input hash and the dependency hash (`internal/model/ids.go:131`, `internal/model/storage.go:119`, `:154`). A changed document is therefore modelled as production sees it: an edit to that document's file.

## The edit
- Unit: the one given by `-unit`, or by default the unit with the most documents.
- Document: the first one in that unit with a non-empty body. Its body gets the suffix " tbdeltaedit"; its search key is kept.
- File: the edited document's file gets a new content hash. The blob record is copied under the new hash, a new snapshot carries that file version, and a second generation is begun over it.

## Which units are rebuilt
The filesystem and treesitter providers have no delta applier (`internal/index/coordinator.go:358`; only scip and dependence have one). A unit whose input changed is therefore rebuilt whole, as `internal/index/generation.go:1256`–`1260` does.

The rule, walked in seal order:
1. A unit is rebuilt if its inputs include the edited file, or if it depends on a unit already rebuilt. A moved dependency changes the dependency hash and so the identity, and `Activate` refuses a member whose dependency is not a member (`units.go:1630`, "member dependencies that are not members").
2. A rebuilt unit gets:
   - the edited file's input with the new hash, and a recomputed input hash;
   - its dependencies remapped to the rebuilt ids, a recomputed `DependencyHash` and a recomputed `NewUnitID`;
   - every node fact and evidence row naming the new unit id and the new content hash, with `NewEvidenceID` recomputed (`internal/model/ids.go:98`).

   The store checks the asserted content hash of nodes and evidence against the unit's inputs (`units.go:1014`, `:1030`; called for nodes at `:713` and for evidence at `:1062`).
3. Every other unit is attached unchanged with `AttachUnit` (`units.go:1480`).
4. `Activate(gen2, expectedActive=gen1)` (`units.go:1525`); compaction runs inside it.

## Smoke
On spf13/cobra, the most-document unit was `treesitter/file:command.go`. The rule rebuilt `filesystem/file:command.go` and `treesitter/file:command.go` (223 documents) and attached 100 units. Timings in ms: attach 5, begin 1, nodes 8, put 27, seal 13, rebuild wall 59, activation 26. Afterwards, the query "tbdeltaedit" returns 1 hit.

## Departures
"One unit" is two here, because production rebuilds every unit the edited file feeds.

The edited file's documents keep their search keys. In production, chunk keys include the content hash (`internal/provider/filesystem/chunk.go:288`), so a real edit would mint new keys for that file's chunks.

The new blob is synthetic. After the delta, hydrating a hit in the edited file reports no source range. The delta arm does not measure queries.
