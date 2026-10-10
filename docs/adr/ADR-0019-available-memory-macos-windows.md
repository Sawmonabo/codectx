# ADR-0019: Available memory is read natively on macOS and Windows, with the product's own residency

## Status

Accepted, 2026-10-09, by the owner's ruling. The admission rule this reading feeds is
[ADR-0012](ADR-0012-native-dependence-engine.md) decision 5. The survey behind it is
[research 28](../research/28-available-memory-macos-windows.md).

## Context

- **The rule.** Every heavy child (an external indexer, a language server, a parser worker) is admitted while the sum
  of what the admitted children reserve fits one allocation, and a child larger than the whole allocation runs alone.
  The allocation is the smaller of available memory plus the product's own observed residency, less the base footprint
  and a margin, and half of that total. It is re-derived from the operating system's figure before every admission;
  adding back the product's own residency means its own growth never throttles it (ADR-0012 decision 5;
  `internal/admission/machine.go:57`, `internal/app/compose.go:150-174`, `compose.go:210`).
- **Only Linux is read.** The Linux reader takes `min(MemAvailable, cgroup headroom)`
  (`internal/admission/machine_linux.go:31`, `internal/config/machine.go:335`). Every other platform is unobserved
  (`internal/admission/machine_other.go:1-13`) and admits against a fixed 8 GiB stand-in
  (`internal/admission/machine.go:19-25`, `machine.go:87-92`). The product's own resident set is read from
  `/proc/self/status` only (`internal/residency/residency.go:20`), and children's resident memory is unread off Linux
  (`internal/process/treesample_other.go:30`).
- **Two meanings of unobserved.** `Allocation` returns 0, "unknown, no bound", for an unobserved machine
  (`machine.go:58-60`), while `SchedulingAllocation` stands 8 GiB in (`machine.go:88-89`). The 0 branch has no live
  caller: its only call runs after the observed guard (`machine.go:91`).
- **Worked example.** A 16 GiB laptop with an editor and a browser open has about 5 GiB that can be taken without
  compressing or swapping. The Linux rule on the same state gives the children about 2.5 GiB; the stand-in admits up
  to 8 GiB, and the machine starts compressing and swapping. A 64 GiB workstation with 50 GiB free gets the same 8 GiB
  where the rule gives about 25 GiB.
- **Building.** The release is built with cgo already (`.github/workflows/release.yml:148-169`); the module already
  depends on `golang.org/x/sys` (`go.mod:23`).
- **The requirement.** Never freeze the host; use what the host can give; nothing to tune.

## Decision

### 1. macOS: free plus file-backed pages, from one kernel snapshot

- Available memory is `max(free_count − speculative_count, 0) + external_page_count` from
  `host_statistics64(HOST_VM_INFO64)`, times the kernel page size (`vm_kernel_page_size`).
- That is the macOS counterpart of `MemAvailable`: free memory plus the file cache the kernel can drop without writing
  to disk. The kernel's own design note approximates available memory under pressure as free plus file-backed
  [S1][S2], and a major browser's process metrics compute exactly this [S3]. Speculative (read-ahead) pages sit inside
  the free count and are file-backed, so they are subtracted once; the clamp covers two counters read a moment apart.
- The reader is a small cgo file built under `darwin && cgo`. A build without cgo is unobserved, so a cross build or
  vet still compiles. A failed call is unobserved. The arithmetic is a function without build tags, so it is tested on
  every platform.
- The page counts are in kernel pages, so the multiplier is the kernel's page size, never the calling process's: an
  x86-64 build running under translation on a 16 KiB-page machine would otherwise read low.

**Alternatives.** Free plus inactive (and purgeable), as several runtime and system libraries compute [S4][S5]:
inactive includes anonymous pages that macOS frees only by compressing or swapping, the slowdown this rule exists to
prevent. Active plus inactive plus free plus speculative [S6]: counts active pages, the pressure engine's input, not
reclaimable memory. The kernel's memory-status percentage: an integer percent that includes active pages. The raw
`vm.page_*` sysctls without cgo: verified and safe (it reads equal or low), but the names are in no public header, so
the platform makes no stability promise; it is the fallback if cgo is ever removed from this package.

### 2. Windows: physical room capped by commit room, from one call

- Available memory is `min(ullAvailPhys, ullAvailPageFile)` from one `GlobalMemoryStatusEx` call, through a lazily
  loaded `kernel32` procedure; no new module.
- `ullAvailPhys` is "the sum of the size of the standby, free, and zero lists", memory reusable without writing to
  disk [S7], the counterpart of `MemAvailable`. `ullAvailPageFile` is the most this process can commit, the line past
  which allocations fail [S7]. Their minimum mirrors Linux's `min(MemAvailable, cgroup headroom)`; a container's
  memory limit is a job commit limit [S8], which a runtime's collector reports is already reflected in the commit figure
  [S9].
- That last claim is proved by a test on Windows (decision 6). If it fails, the reader also takes
  `JobMemoryLimit − PeakJobMemoryUsed` from `QueryInformationJobObject`, a conservative figure because the structure
  reports only the job's peak.
- A zero return is unobserved.

**Alternative.** `ullAvailPhys` alone, as a widely used system library does [S4]: it misses the job's commit limit, so
a container could be over-admitted.

### 3. The product's own residency is read on both

Without it, an observed reading counts the children's use twice, once inside the operating system's figure and again
as reservations on the ledger, and the allocation shrinks as the product's own children grow.

- **The product's resident set:** the task's own memory counters (`task_info`, `TASK_VM_INFO`) on macOS: the
  resident set, its peak and the resident anonymous pages. On Windows, the process memory counters
  (`K32GetProcessMemoryInfo`, the `kernel32` export of `GetProcessMemoryInfo`, lazily loaded): the working set and its
  peak; Windows reports no anonymous part of the working set there, so that figure stays absent.
- **Each child's resident memory:** `proc_pid_rusage` for every process in the child's process group on macOS; on
  Windows, the job each child already runs in (`internal/process/process_windows.go`), its process list, and each
  member's memory counters, each member confirmed in the job through the handle opened on it.
- **Which figure is added back for a child.** What a child holds of its own is its anonymous resident memory, and
  neither platform reports exactly that for another process. macOS reports the physical footprint (also counting
  compressed pages that are no longer resident) and the resident set (also counting the shared executable and library
  pages, which stay in the available figure); Windows reports the working set (also counting shared pages) and the
  private commit (also counting committed pages never touched). The anonymous resident part is within both, so the
  smaller of the two is added back: adding back more than the child holds would widen the allocation past what the
  host has.
- A figure a platform does not report stays absent, never zero (`internal/residency/residency.go`). Where the
  readers report, the idle-footprint stand-in (`internal/config/machine.go:29`) stops applying.
- Only the children's anonymous resident memory is sampled on these platforms; the running tree's resident peak and
  its transferred bytes are reported unavailable there.

### 4. One rule for a host that cannot be read

`Allocation` takes only an observed reading; its unobserved branch is removed. `SchedulingAllocation` is the single
place an unobserved machine becomes the 8 GiB stand-in, and it is reported as unobserved, never as a measured figure.
With decisions 1 and 2, the stand-in remains only for other platforms and for a failed reading.

### 5. Pressure notifications are not used

The operating systems' memory-pressure signals (a dispatch source on macOS, a resource notification on Windows) add
little here: the allocation is re-derived before every admission, and the ledger never takes back what it admitted, so
a signal could change only the next admission, which the fresh reading already covers. Should one be wanted, it plugs
in where the allocation is re-derived (`internal/app/compose.go:210`): a warning or critical level sets the allocation
to zero, and the existing rule that a child larger than the allocation runs alone does the rest.

### 6. Proof

- **Observed (macOS and Windows):** the reader is observed, and `0 < available ≤ total`, where total is the
  platform's physical memory.
- **Arithmetic (every platform):** the macOS figure on fake snapshots, with speculative above free clamping to 0, a
  16 KiB page multiplying correctly, and an overflow reported as unobserved; the Windows minimum on fake structures,
  with a zero or failed call unobserved.
- **One stand-in rule:** a forced unobserved machine gives the 8 GiB stand-in, unobserved, and re-derivation keeps it;
  no other caller can turn an unobserved reading into an allocation (mutation: restore `Allocation`'s 0 branch).
- **Residency (macOS and Windows):** the readers report a resident set for the test process, and a child that
  touches N MiB lowers the allocation by much less than N.
- **Job limit (Windows):** after asserting that its own reading exceeds 512 MiB, the test re-runs itself inside a job
  limited to 512 MiB of job memory, and the child reads at most 512 MiB available. This proves or disproves that the
  commit figure carries the job's limit.

The observed, residency and job-limit tests run on the macOS and Windows CI legs; the arithmetic and one-rule tests run
everywhere.

## Consequences

- On macOS and Windows the product admits children against what the host can give, as on Linux: fewer on a small,
  busy laptop, more on a large workstation.
- The product's own growth no longer throttles it on those platforms.
- macOS admission depends on cgo, which the release already requires; a build without cgo falls back to the stand-in
  and says so.
- Platform defects in the readers surface in the macOS and Windows CI legs, not on Linux.

## Sources

- [S1] *memorystatus.md*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/doc/vm/memorystatus.md —
  available memory under pressure approximated as free plus file-backed.
- [S2] *host.c*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/kern/host.c; and
  *vm_statistics.h*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/mach/vm_statistics.h —
  how `free_count`, `speculative_count` and `external_page_count` are filled.
- [S3] *process_metrics_apple.mm*, https://chromium.googlesource.com/chromium/src/+/main/base/process/process_metrics_apple.mm
  — `GetAvailablePhysicalMemory`: free less speculative, plus file-backed.
- [S4] *mem_darwin.go* and *mem_windows.go*, https://raw.githubusercontent.com/shirou/gopsutil/master/mem/mem_darwin.go,
  https://raw.githubusercontent.com/shirou/gopsutil/master/mem/mem_windows.go — free plus inactive; physical only.
- [S5] *darwin.c*, https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/darwin.c; and *os_bsd.cpp*,
  https://raw.githubusercontent.com/openjdk/jdk/master/src/hotspot/os/bsd/os_bsd.cpp — free plus inactive plus
  purgeable.
- [S6] *system.rs*, https://raw.githubusercontent.com/GuillaumeGomez/sysinfo/main/src/unix/apple/system.rs — active
  plus inactive plus free plus speculative.
- [S7] *MEMORYSTATUSEX*, https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
  — `ullAvailPhys` and `ullAvailPageFile`.
- [S8] *Implementing resource controls*, https://learn.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/resource-controls;
  and *JOBOBJECT_EXTENDED_LIMIT_INFORMATION*, https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_extended_limit_information
  — a container's memory limit as a job commit limit; only the peak job usage is reported.
- [S9] *gcenv.windows.cpp*, https://raw.githubusercontent.com/dotnet/runtime/main/src/coreclr/gc/windows/gcenv.windows.cpp
  — "The commit limit is already reflected by default when you run in a job".
- [research 28](../research/28-available-memory-macos-windows.md) and
  [raw/memory](../research/raw/memory/findings.md) — the survey, the options and the full source list.
