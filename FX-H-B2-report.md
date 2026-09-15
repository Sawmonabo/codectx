# FX-H-B2 — report (branch lane/FX-H-B2, commit cb371ab; wave-h merged in first)

| Item | Status | Where |
|---|---|---|
| VF6 `context.max_seeds` key, default unlimited | DONE | internal/config/config.go:367-372 (struct), :525 (default), validate.go:94, fingerprint.go:88 |
| VF6 all 12 hard-coded 64 sites → one predicate | DONE | internal/context/seeds.go `seedsFull` :109-113; callers :143,:243,:262,:287,:333,:348,:531,:560,:603 |
| VF6 cut row names key + value | DONE | seeds.go `noteCut` :39-72 ("stopped at the context.max_seeds bound of 1 …") |
| VF6 unlimited stays page/request-bounded | DONE | seeds.go extractSeeds doc :127-134; `changedFileSeeds` now ONE page + notePageEnd cursor :315-375 |
| VF8 exclusion-pointer notice (fresh compile) | DONE | compiler.go manifestNotices :249-259 + const :292-297 |
| VF8 pointer on manifest-reuse path | DONE | compiler.go :136-144 (keyed on !ScopeComplete; count lives in the paged projection, not the header) |
| VF8 CLI plan render | DONE (already routed) | cli/context.go writePlanResult → writeManifestNotices; proof below |
| Docs: config table + fingerprint row + prose | DONE | docs/configuration.md:72, :353, :383-394 |
| Docs: sessions | DONE | docs/context-sessions.md:48-58 |
| C1 routed (1) ContextSlice.entry_ordinals boundCount → boundPage | DONE | internal/model/context.go:274-277; test extended in model/field_bounds_test.go:71-86 |
| C1 routed (2) cli prose on the old per-result cap | DONE | internal/cli/context.go:55-60 |

## Proofs
Mutation (`seedsFull` → `return n >= 64`):
```
--- FAIL: TestUnlimitedSeedDiscoveryExaminesEveryIdentityTheTaskNames (0.00s)
    seed_cut_test.go:89: unlimited discovery examined 64 of the 200 identities the task names
FAIL	github.com/Sawmonabo/codectx/internal/context	0.004s
```
Restored → `ok github.com/Sawmonabo/codectx/internal/context 0.287s`.

Real `context plan` (2-file fixture, built binary), fresh / reuse / `max_seeds = 1` excluded page:
```
notice      3 candidate(s) were excluded from this plan, each with a reason; page them with `codectx context entries <session-id> --view excluded`
notice      this plan's scope is incomplete; any excluded candidates and their reasons are paged there too -- page them with ...
      1  seed discovery stopped at the context.max_seeds bound of 1 while...
```
A project `.codectx.toml` setting the key is refused ("only the user configuration may set"), matching the `user` trust row.

Gate: `go build ./...` OK; `go vet ./internal/{context,config,cli,app,model}/...` OK; `gofmt -l .` empty; `git diff --check` clean;
`go test ./internal/{context,config,cli,app,model,workflow}/... -count=1` all ok; `go test ./internal/e2e -count=1` → `ok 3.538s`.

## Deviations / notes
- **fingerprint.go touched** (outside the literal Owns list, inside the context block of `internal/config`): `max_seeds` changes
  which entries a manifest holds, so omitting it from `ContextPolicyHash` would have reused a manifest compiled under a
  different bound. Appended last, per R2.
- **changedFileSeeds now reads one page unconditionally** (was: paged until the 64 bound). Paging it under an unlimited bound
  would materialise a whole working tree in the compile's heap; one page + `notePageEnd` with the last file id as cursor is the
  shape the lexical step already uses, and avoids unlimited reading *less* than a finite limit. Limit still caps admission
  within the page.
- **The unlimited bound, in numbers**: `model.MaxTaskBytes` = 8192 and `model.MaxPageItems` = 200, so the seed set peaks at the
  distinct identities one 8 KiB task can spell x 200 declarations per identity, plus one 200-row changed-file page. Memory is
  page-bounded as required, but `freeTerms` no longer caps at 64 terms, so a long task now makes one `resolveSymbol`
  round-trip per distinct word: compile LATENCY scales with task length, and a pathological task is stopped by
  `resources.query_timeout` firing into `ctx.Err()` (an explicit incomplete compile) instead of a silent cut at 64.
- **`changedFileSeeds` infers "changes remain" from a full page** (`len(files) == pageSize`), because `PinnedReader.ChangedFiles`
  returns no cursor. On an exact-boundary page that emits a page-end exclusion and reports the scope incomplete when the walk
  was in fact complete — conservative in the safe direction, and accepted rather than guessed around.
- **compose.go unchanged**: `contextpkg.New` already receives the whole `config.Config`, so the compiler reads
  `cfg.Context.MaxSeeds` directly — no wiring line was needed.
- **`model.MaxSeeds` (=64) deliberately left** on the request array (`internal/model/context.go:140,518`) and the `--seed` help
  text (`internal/cli/context.go:789,954`): that is a wire bound on caller-supplied input, not a discovery cap. If the
  controller wants it configurable too, it is `boundStrings("context.seeds", r.Seeds, MaxSeeds, MaxPathBytes)` → a
  caller-supplied `config.Limit` threaded like `workflow.Limits.MaxObservationReferences`.
- Reuse-path notice carries no count (the header holds none) and does not claim rows exist: a scope also goes incomplete with
  no exclusions (an unreadable evidence route), so the count-less form promises only the surface.
