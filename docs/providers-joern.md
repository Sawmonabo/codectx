# Joern deep-analysis provider

`internal/provider/joern` is the optional Section 11.6 adapter. It runs an
approved local Joern installation's built-in noninteractive tools against a
private materialization of the pinned snapshot, imports the exported graph
with every field, record and depth bounded, and publishes method-level
`calls`, `control_depends_on` and `data_flows_to` relations with
`static_analysis` evidence. It is disabled by default
(`providers.joern.enabled = false`), depends on the `filesystem` and
`treesitter` providers, and builds one workspace-scoped unit.

> **Status: unverified against a real tool.** Joern is not installed on the
> machine this adapter was written on. The argument arrays below follow the
> current Joern documentation and are marked unverified (Ruling R11-1). Run
> the smoke commands in *Declaring the profile supported* before relying on
> the provider; until then, treat it as available but not supported.

## What is run

Configuration selects the built-in profile and approves the two executables:

```toml
[providers.joern]
enabled = "auto"                 # false | true | "auto"
profile = "pinned-default"       # the only profile this build knows
timeout = "45m"                  # bounds one whole unit

[analyzers.joern-parse]
executable = "/opt/joern/joern-cli/joern-parse"
version_constraint = "4.0.*"     # exact "4.0.100" or "major.minor.*"
checksum = "<sha256 of the executable>"   # optional; pin it (see Reuse)
work_dir = "/var/tmp/codectx-joern"
memory_budget_bytes = 8589934592
disk_budget_bytes = 21474836480  # materialization + CPG
timeout = "30m"
network = "denied"
env_allowlist = ["JAVA_HOME", "PATH"]

[analyzers.joern-export]
executable = "/opt/joern/joern-cli/joern-export"
version_constraint = "4.0.*"
work_dir = "/var/tmp/codectx-joern"   # must equal joern-parse.work_dir
memory_budget_bytes = 8589934592
disk_budget_bytes = 21474836480  # exports + scratch database
timeout = "15m"
network = "denied"
env_allowlist = ["JAVA_HOME", "PATH"]
```

`joern.ProfileFromConfig` turns this into a typed `Profile`. The two analyzer
entries must have **no `args`** (the profile owns the exact argv), `network =
"denied"` and the same `work_dir`. Any other `providers.joern.profile` value
is `CTX_CONFIG_INVALID`: an unknown profile is unsupported, not guessed.

The `pinned-default` argument arrays, with the typed placeholders the
provider fills from validated private paths:

```text
probe:      <tool> --version
parse:      joern-parse  ${input_dir} --output ${cpg}
export all: joern-export ${cpg} --repr=all --format=neo4jcsv --out ${out_dir}
export pdg: joern-export ${cpg} --repr=pdg --format=graphml  --out ${out_dir}
```

`${input_dir}` is the materialization root, `${cpg}` the CPG file and
`${out_dir}` the export directory, all under one private run directory. The
`--language` frontend selector is deliberately not passed: Joern's own
detection decides, as it does for a plain `joern-parse <dir>`.

Every process runs through `internal/process`: absolute path, literal argv,
no shell, an environment consisting only of the allowlisted variables,
per-tool timeout and 10 s grace, output caps (64 KiB captured for the probe;
64 MiB discarded-but-counted for parse and export), and the profile's memory
and disk reservations. `network = "denied"` is a declaration the diagnostics
report; it is not an OS sandbox.

## Detection

`Detect` never reads the repository. For each tool it checks that the path is
a regular executable file (`CTX_PROVIDER_UNAVAILABLE` otherwise) and, when a
checksum is pinned, that the SHA-256 matches (`CTX_TRUST_REQUIRED`
otherwise). It then runs the version probe through the runner and matches the
observed version against the constraint (`CTX_TRUST_REQUIRED` on mismatch;
the runner's code when the probe fails). The probe executes code, which is
why detection needs the approved absolute path from user configuration and
`auto` never runs anything found on `PATH`.

`Descriptor()` is static: its version is always `1/pinned-default` (adapter
version and profile name). The observed tool versions are returned separately,
as `Detection.ObservedVersion = "parse=<v>,export=<v>"`; the coordinator folds
that into `UnitSpec.ProviderVersion`. Section 9.1 keys unit identity on the
exact provider version, and the installed Joern release is the provider's
semantics, so a unit built by one release is never reused for another — but
detection reports the release, it never mutates the descriptor.

## Run lifecycle

```text
<work_dir>/runs/run-<id>/
  src/mat-<id>/      snapshot.Materialize of the whole pinned view (non-deleted files)
  cpg.bin            joern-parse output
  export-all/        Neo4j CSV: nodes_<LABEL>_{header,data}.csv, edges_<TYPE>_{header,data}.csv
  export-pdg/        GraphML program dependence graphs
  scratch.db         private SQLite staging database
```

The unit's deadline (`providers.joern.timeout`) bounds the whole sequence;
each tool has its own timeout. The materialization is bounded by
`joern-parse.disk_budget_bytes`; the CPG is checked against the same budget
after parse; each export directory and the scratch database against
`joern-export.disk_budget_bytes`. The run directory is removed on every
return path (success, provider error, timeout, cancellation). A crash of the
codectx process itself can leave a `runs/run-*` directory behind; removing
orphans under `work_dir` is a `doctor` concern (Task 20).

**Accepted gap — SQLite temporary b-trees.** The scratch database is opened
with `temp_store(FILE)` so SQLite spills to disk instead of into the Go
process's heap (the recursive `REACHING_DEF` walk in `project` is the main
consumer). Those temporary files are created in the operating system's temp
directory, not under `work_dir`, so they are outside the
`joern-export.disk_budget_bytes` accounting and outside the run directory's
removal. They are unlinked by SQLite when the statement or connection ends;
the accepted consequence is that a run's peak disk use can exceed the
profile's budget by the size of one walk's working set. Ruling: accepted gap,
not worked around — the alternative (`temp_store(MEMORY)`) would move an
unbounded structure into the heap, which Section 6 forbids outright.

The coordinator must declare every non-deleted file of the snapshot as the
unit's input: the provider materializes `FileSelection{}` and binds facts to
those files. Any other `ScopeKey` than `workspace` is `CTX_ARGUMENT_INVALID`.

## Import

**Neo4j CSV.** Files are read in sorted name order (every `edges_*` file
therefore precedes every `nodes_*` file: forward references are the norm, not
an edge case). A `boundedRecordReader` sits between the file and
`encoding/csv`: it tracks quote state and fails with `CTX_RESOURCE_LIMIT`
(`Details["limit"] = "max_export_record_bytes"`) as soon as more than 64 KiB
have been read since the last record boundary, before the csv reader has
grown a buffer to hold them. Headers are bounded at 64 columns; the field
count of every row must match its header. Role columns are `:ID`, `:LABEL`,
`:START_ID`, `:END_ID`, `:TYPE`; property columns are `NAME:type`.

**GraphML.** `encoding/xml` is fed through a `boundedTokenReader` that
implements `io.ByteReader`, so the decoder reads it directly and the bytes
consumed since the last token are the token being decoded; 64 KiB fails the
same way. Depth is capped at 16, a data value at 64 KiB across split
CharData, entities are not resolved. `<key attr.name>` declarations map data
keys to attribute names; the edge label is `labelE` (or the element's `label`
attribute); `DDG` and `TYPE: variable` spellings normalize to `REACHING_DEF`
with the variable.

Both importers stage into the scratch database: METHOD and CALL nodes with
the attributes the projection needs, and edges of the four mapped labels,
deduplicated on `(src, dst, label, variable)`. The CSV export is read first
and wins on node attributes (`INSERT OR IGNORE`); the PDG export contributes
the CDG and REACHING_DEF edges. A Joern version whose `--repr=all` export
omits dataflow edges therefore still yields dependence facts, and one whose
PDG node ids differ from the CSV ids contributes nothing rather than
something wrong. Scratch growth is checked against the disk budget every
4096 rows.

**Dangling edges.** An edge whose endpoints the export never defined is a
loss, and the run is reported `partial` with
`CTX_SOURCE_BINDING_UNVERIFIED`. Only the edges the projection actually
consumes are counted: a `CALL` edge with either end missing, and a `CONTAINS`
edge **whose destination is a staged `CALL` node** and whose source is
missing. A real export contains many `CONTAINS` edges from other sources
(`FILE CONTAINS METHOD`, `TYPE_DECL CONTAINS METHOD`); those are structure
this profile never reads, and counting them would mark every run partial.

### Label vocabulary

| Joern | Handling |
|---|---|
| `CALL` edge (site → METHOD) | `calls` from the METHOD that `CONTAINS` the site to the target |
| `CDG` edge between two anchored sites | `control_depends_on` dependent target → controlling target |
| `REACHING_DEF` walk (depth ≤ 8) between two anchored sites | `data_flows_to` source target → destination target |
| `CONTAINS` | METHOD → call site (filename inheritance and calls attribution) |
| every other label in the CPG schema list in `scratch.go` | known, ignored |
| anything else | counted; reported as capability `unsupported_label:<LABEL>` = `unavailable` (32 distinct by name, then `unsupported_labels:untracked`) |

An *anchored* site is a CALL node whose `METHOD_FULL_NAME` is not an
`<operator>.` synthetic and that has a `CALL` edge to a METHOD; a site with
several targets anchors to the smallest id, deterministically. The
REACHING_DEF walk starts at an anchored site, passes only through
non-anchored nodes (operator calls, identifiers, locals) and stops at the
first anchored site with a distinct target. It is **intraprocedural def-use
reachability**: `data_flows_to` from `helper` to `sink` means a value produced
by a call to `helper` reaches an argument of a call to `sink` inside one
method. Evidence detail says so (`joern:REACHING_DEF intraprocedural var=x`).
It is never a proven source-to-sink flow: no sanitizer, no interprocedural
propagation, no semantic model.

**Gaps (reported, not papered over):** `reads`/`writes` are not published.
Joern has no READ or WRITE edge; deriving field and variable accesses from
`<operator>.assignment` / `<operator>.fieldAccess` argument shapes is
frontend-specific and would be a guess. `model.RelationKind` has every kind
the profile needs (`calls`, `control_depends_on`, `data_flows_to`); no
vocabulary gap.

## Identity, evidence and source binding

Only METHOD nodes are published, and only those that are an endpoint of an
emitted relation. A method's `FILENAME` (relative to the materialization, or
absolute under it) must be a file of the snapshot; a method whose path is not
(`src/missing.go` in the fixture) is **dropped together with every relation
touching it** and the run's capabilities are reported `partial` with
`CTX_SOURCE_BINDING_UNVERIFIED`. A fact bound to bytes that are not its
source is worse than no fact. `IS_EXTERNAL` methods (library targets) have no
location by design.

Ranges are verified against the pinned bytes read through
`req.Content.Open`, one file at a time, files up to 4 MiB (larger files keep
file-bound facts without ranges). A METHOD spans whole lines
`LINE_NUMBER..LINE_NUMBER_END`. A CALL site is the exact bytes of its `CODE`
at `LINE_NUMBER`/`COLUMN_NUMBER` when the source holds that text there
(columns are tried one-based, then zero-based: the byte match is the proof,
not the frontend's convention), else its whole line; coordinates that do not
land on the file yield no range. All positions go through `source.Cursor`.

Resolution goes through `req.Resolver` with this candidate:

| Field | Located method | External method |
|---|---|---|
| `ScopeKey` | `"file:" + path` | `provider.ScopeWorkspace` |
| `NativeKey` | `joern:method:<FULL_NAME>` | same |
| `StrongKey` | `decl:<NAME>@<path>:<LINE_NUMBER>-<LINE_NUMBER_END>` | none |
| `Kind` | `function` (an alias match adopts the stored kind) | `function` |
| `Language` | the snapshot file's language | normalized `META_DATA.LANGUAGE` |
| `FileID`, `ContentHash`, `Range` | the pinned file and verified line span | none |

The strong key is the **declaration-location contract**, fixed by controller
ruling (R11-3, fix round 1). The exact strings are:

```text
scope key:  "file:" + <root-relative slash path>
strong key: "decl:" + <identifier token as written>
            + "@" + <root-relative slash path>
            + ":" + <start line> + "-" + <end line>
```

Lines are **one-based and the span is inclusive**. The identifier is the
token the producing tool saw, byte for byte: no case folding, no
unqualifying, no normalization of any kind. For Joern it is the METHOD's
`NAME` attribute (a METHOD with no `NAME` publishes no strong key, since it
has no identifier token); the path is `FILENAME` normalized to a
root-relative slash path; the lines are `LINE_NUMBER` and `LINE_NUMBER_END`
(falling back to `LINE_NUMBER` when the end is absent or smaller). A key that
would exceed `MaxNativeKeyBytes` is omitted rather than truncated — a
truncated key would collide. `emit.go`'s `declarationKey` is the single
producer of this string.

A structural provider that aliases its declarations under that
`(scope key, strong key)` pair makes the Joern method resolve to the same
node with basis `native_key`. Without a hit the resolver
mints a Joern-local identity from the file and range (`source_location`) or,
for external methods, from the qualified name (`structural_key`); an
unresolved basis marks metadata `{"resolution":"unresolved"}`. Ambiguous
alternatives become `may_refer_to` edges. Every method publishes one alias
`(ScopeKey, NativeKey) → NodeID` so later units resolve the same Joern
symbol without rescanning.

Evidence: precision `static_analysis`; `NativeKey` is the Joern
`FULL_NAME` of the method (node evidence) or of the call target (relation
evidence); `Detail` is `joern:METHOD`, `joern:METHOD external`, `joern:CALL`,
`joern:CDG` or `joern:REACHING_DEF intraprocedural var=<v>`. Occurrences
with identical evidence identity collapse; distinct ranges stay distinct; a
relation carries at most `MaxEvidencePerFact` rows (the overflow is counted
and reported `partial` with `CTX_RESOURCE_LIMIT`).

Emission order honours the sink contract: every node fact, then every alias,
then every relation, each phase streamed from ordered scratch queries in
pages of 512. `providertest.Conform` runs the provider with the sink flushed
after every `Put`, so a violation would fail deterministically.

## Failure handling

| Condition | Outcome |
|---|---|
| executable missing / not executable | detection `unavailable`, `CTX_PROVIDER_UNAVAILABLE` |
| checksum or version not the approved one | detection `unavailable`, `CTX_TRUST_REQUIRED` (a `true` enablement makes it `failed`) |
| unknown `providers.joern.profile`, analyzer with `args`, `network != denied` | `CTX_CONFIG_INVALID` at profile construction |
| tool exits non-zero | runner's `CTX_PROVIDER_UNAVAILABLE`; unit failed |
| tool timeout / unit deadline | `timed_out`; unit failed |
| record, token, depth, file-count or disk bound exceeded | `CTX_RESOURCE_LIMIT` naming the limit; unit failed |
| malformed CSV/XML, unknown header role, empty ids | `CTX_PROVIDER_OUTPUT_INVALID`; unit failed |

"Unit failed" is `provider.RunUnit`'s doing: on any non-succeeded outcome it
deletes every row the unit wrote; this provider adds nothing to that and
emits facts only after both exports imported and projected cleanly, so a
failed export never reaches the sink at all. Process metrics (exit code,
duration, stdout/stderr byte counts, timed-out/canceled/truncated/signaled
flags, memory and disk reservations) are recorded per tool as a structured
`slog` entry under `component=provider.joern`, separate from the unit's
`RecordsEmitted`/`BytesProcessed` (facts and export bytes). Child output is
never logged.

## Reuse

`AnalysisConfigHash` already folds the analyzer executables, version
constraints, checksums and environment allowlist; `Detection.ObservedVersion`
carries the tool versions the coordinator folds into `UnitSpec.ProviderVersion`.
Pin `checksum` too: with a wildcard
constraint, two patch releases that print the same `--version` line would
otherwise share unit identity.

## Declaring the profile supported

A maintainer with a real installation runs, from a scratch directory:

```sh
JOERN=/opt/joern/joern-cli
$JOERN/joern-parse  --version
$JOERN/joern-export --version
$JOERN/joern-parse  ./src --output ./cpg.bin
$JOERN/joern-export ./cpg.bin --repr=all --format=neo4jcsv --out ./export-all
$JOERN/joern-export ./cpg.bin --repr=pdg --format=graphml  --out ./export-pdg
ls export-all | head; ls export-pdg | head
```

and confirms: (1) both tools accept `--version` and print a version token
the constraint matches; (2) `--output` is the CPG flag of `joern-parse`; (3)
`export-all` holds `nodes_*_header.csv` / `nodes_*_data.csv` /
`edges_*_header.csv` / `edges_*_data.csv` (or single files with a header
row) whose METHOD rows carry `FILENAME` relative to the input directory,
whose CALL rows carry `METHOD_FULL_NAME`, `LINE_NUMBER`, `COLUMN_NUMBER`,
`CODE`, and whose edge files include `CALL`, `CONTAINS`, `CDG` and
`REACHING_DEF`; (4) the GraphML node ids equal the CSV `:ID` values and edge
labels are `CDG` / `DDG`; (5) the run's capabilities are `fresh`, not
`partial` — **`CONTAINS` sources other than `METHOD` are expected** (`FILE`
and `TYPE_DECL` contain methods), so a `partial` run with
`CTX_SOURCE_BINDING_UNVERIFIED` means real dangling `CALL` edges or unmatched
source paths, never ordinary `CONTAINS` structure. Any difference is a one-line change to
`PinnedDefault()` or the label tables in `scratch.go`; only then may the
docs drop the unverified marker. The checked-in fixture under
`internal/provider/joern/testdata` mirrors this expected layout.

An optional CI job (not added to `.github/` by this task) would install a
pinned Joern release and run the same five commands against
`internal/provider/joern/testdata`'s source before running `go test
./internal/provider/joern/...`:

```yaml
joern-smoke:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-java@v4
      with: { distribution: temurin, java-version: "21" }
    - run: curl -sSL https://github.com/joernio/joern/releases/download/v4.0.100/joern-install.sh | bash -s -- --version=v4.0.100 --install-dir=$HOME/joern --without-plugins
    - run: |
        set -e; J=$HOME/joern/joern-cli
        $J/joern-parse --version && $J/joern-export --version
        $J/joern-parse internal/provider/joern/testdata --output cpg.bin
        $J/joern-export cpg.bin --repr=all --format=neo4jcsv --out export-all
        $J/joern-export cpg.bin --repr=pdg --format=graphml --out export-pdg
```
