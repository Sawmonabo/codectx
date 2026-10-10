# Reading available memory on macOS and Windows

Research of 2026-10-09 behind [ADR-0019](../../../adr/ADR-0019-available-memory-macos-windows.md). Code read in the
repository at commit e9c8799 (the code under `internal/` is unchanged on the branch these documents land on). Nothing in
the repository was changed.

## 1. The question in plain words

codectx decides how many heavy children (indexers, language servers, parser workers) can run at once. It adds up
what they reserve and checks the sum against one figure, the *allocation*, which comes from the machine's
available memory. Linux reports that figure. macOS and Windows report nothing in the current code, so codectx admits against
a fixed 8 GiB stand-in.

**Example.** A 16 GiB MacBook has an editor and a browser open, and about 5 GiB can be taken without the OS
compressing or swapping. Linux code reading the same state would give children
`min(5 GiB − footprint − 1 GiB, 2.5 GiB)` ≈ 2.5 GiB. The current macOS build admits up to **8 GiB** of reservations,
so macOS starts compressing and swapping: Activity Monitor shows yellow or red, and the editor stutters. The
opposite happens on a 64 GiB Mac Studio with 50 GiB free: the stand-in still allows 8 GiB, where Linux would
allow about 25 GiB, so indexing is slower than it needs to be.

The decision has three parts:
1. Which OS figure means "available" on each platform.
2. How to read it: x/sys only, cgo, or gopsutil.
3. Whether to also listen for the OS's memory-pressure signals.

## 2. Current code

| What | Where | What it does |
|---|---|---|
| Non-Linux reader | `internal/admission/machine_other.go:1,13` (`//go:build !linux`) | `ObserveMachine() Machine { return Machine{} }`, so it is always unobserved |
| Linux reader | `internal/admission/machine_linux.go:31-43,47-70` | `min(MemAvailable, cgroup headroom)`. If either one is missing, the other is used alone |
| cgroup headroom | `internal/config/machine.go:335` (`CgroupMemoryHeadroom`) | group limit minus group usage, for each ancestor |
| Stand-in | `internal/admission/machine.go:19-25` | `UnobservedAllocationBytes int64 = 8 * giB` |
| Derivation | `internal/admission/machine.go:57-66` | `Allocation`: `min(avail − base − margin, avail/2)`. It returns **0 when unobserved, meaning "unknown, no bound"** |
| One admission figure | `internal/admission/machine.go:87-92` | `SchedulingAllocation`: unobserved → `(8 GiB, false)`. Observed → `Allocation(...)` |
| Composition | `internal/app/compose.go:767-778` | reads once and warns once when unobserved |
| Re-derive before each admission | `internal/app/compose.go:210-221` | **returns early when unobserved**, so the stand-in never changes |
| Own-residency add-back | `internal/app/compose.go:164-174` | adds this process's RSS (`residency.Read`) and the children's anonymous RSS back onto available, so "the product's own growth never throttles it" (`compose.go:150-157`) |
| Own RSS reader | `internal/residency/residency.go:20,37-43` | `/proc/self/status` only. Absent everywhere else |
| Children's RSS | `internal/process/treesample_other.go:1,30` | `anonBytes()` returns `(0, false)` on every non-Linux platform |
| Idle-footprint stand-in | `internal/config/machine.go:29-44` | 32 MiB wherever `residency.Read` returns nothing |
| Status surface | `internal/model/status.go:350-362` | an unobserved allocation is left out of status, never reported as zero |
| cgo | `internal/provider/treesitter/worker/memory_linux.go:14` (`import "C"`), `.github/workflows/release.yml:148-169` | the release is built with `CGO_ENABLED: "1"` and checks that each runner is native. cgo is already required |
| x/sys | `go.mod`: `golang.org/x/sys v0.48.0` | `go doc` (GOOS=windows) lists **no** `GlobalMemoryStatusEx`, `GetProcessMemoryInfo` or `IsProcessInJob`. It does have `NewLazySystemDLL`, `QueryInformationJobObject`, `JOBOBJECT_EXTENDED_LIMIT_INFORMATION`, `JOB_OBJECT_LIMIT_JOB_MEMORY` and `CreateJobObject`/`SetInformationJobObject`/`AssignProcessToJobObject`. On GOOS=darwin, x/sys/unix has `SysctlUint32/SysctlUint64/SysctlRaw` and no Mach host calls |
| CI | `.github/workflows/ci.yml:17` (`ubuntu-latest` only), `tools-matrix.yml:47` and `release.yml:123-130` (macOS/Windows legs) | macOS and Windows test runs happen only in the platform CI legs |

**Two rules for an unobserved host.** `Allocation` returns 0 when the machine is unobserved, meaning "unknown, no
bound" (`machine.go:58-60`), while `SchedulingAllocation` stands 8 GiB in for the same case (`machine.go:88-89`).
`rg '\.Allocation\('` finds one caller, `machine.go:91`, which runs only after the `Observed` guard, so no live sizing
consumer reads the 0 branch. Unobserved hosts are still handled in two places with two meanings; the design collapses
that into one rule.

**A gap that decides part of the answer.** If the readers report available memory on macOS or Windows but not the
product's own residency, `compose.go:168-174` adds back nothing. The children's actual use is then subtracted
twice: once inside the OS figure, and again as reservations on the ledger. Each re-derivation shrinks the
allocation as codectx's own children grow, which is exactly what the Linux path was built to avoid. This errs
toward fewer children, so it is safe for the host, but it breaks the stated rule. Residency readers are
therefore part of this decision, not a later step.

## 3. The correct signal on each OS

### 3.1 What "available" should mean

On Linux codectx uses `MemAvailable`: memory a new workload can take without swapping. That is free memory plus
file cache the kernel can drop. The macOS and Windows readers should mean the same thing.

Total memory is the wrong basis. Several tools use it, but only with a user-set fraction:
- Bazel's `HOST_RAM` is the *total* physical memory (`LocalHostComputeResources.getMemoryMb` →
  `getTotalPhysicalMemorySize`), and the user scales it (`--local_ram_resources=HOST_RAM*.5`).
- The Go runtime detects nothing. `GOMEMLIMIT` defaults to `math.MaxInt64` (`go doc runtime`). The GC guide says
  not to set a limit for "a CLI tool or a desktop application".

Neither is a model for "never freeze the host".

### 3.2 macOS

The kernel's counters (xnu, `osfmk/kern/host.c:808-872`) feed `host_statistics64(HOST_VM_INFO64)`:
- `free_count = vm_page_free_count + speculative_count` (host.c:818). Speculative pages are file read-ahead
  counted inside "free" (`mach/vm_statistics.h:158-163`).
- `external_page_count` = file-backed pageable pages (host.c:871; header comment "file-backed (non-swap)").
- `internal_page_count` = anonymous pages. `compressor_page_count` = pages holding compressed data.

How mature projects compute "available" from these:

| Project | Formula | Read |
|---|---|---|
| Chromium (`base/process/process_metrics_apple.mm`, `GetAvailablePhysicalMemory`) | `(free_count − speculative_count) + external_page_count`, with free clamped to 0 when the two snapshots disagree | "free memory plus memory that can be reclaimed without writing to disk, which on macOS is the file-backed cache" |
| xnu design note (`doc/vm/memorystatus.md`) | "can generally be approximated to `free + file_backed` for a system under pressure" | same as Chromium |
| gopsutil v4 (`mem/mem_darwin.go`) | `free + inactive` | inactive includes anonymous pages, which must be compressed or swapped to reclaim |
| libuv `uv_get_available_memory` (`src/unix/darwin.c`) | `free + inactive + purgeable` (and `uv_get_free_memory` = `free_count` only) | same caveat |
| OpenJDK (`os_bsd.cpp`, `os::Bsd::available_memory`) | `free + inactive + purgeable`, with the comment that "free_count is just a lowerbound" | same caveat |
| Rust sysinfo (`src/unix/apple/system.rs`) | `active + inactive + free + speculative` (xnu's `AVAILABLE_NON_COMPRESSED_MEMORY`) | counts *active* pages, which is the pressure engine's input, not reclaimable memory |

**Decision for macOS: Chromium's formula, `free − speculative + external`, times the kernel page size.** It is
the one that matches `MemAvailable` (free plus droppable file cache). Subtracting speculative avoids counting
read-ahead pages twice, since they sit in both `free_count` and the file-backed count. Adding `inactive` would
count anonymous pages that macOS can only free by compressing or swapping, and compressing or swapping is the
slowdown codectx must not cause. Purgeable pages are left out on purpose: the xnu note says they are trimmed
quickly under pressure, so they are gone by the time the figure matters.

Signals that look right but are not:
- **`kern.memorystatus_level`.** On macOS it is `AVAILABLE_NON_COMPRESSED_MEMORY × 100 / total_pages`
  (`osfmk/vm/vm_pageout.c:4738`; `vm_page.h:1528,1549`). That is an integer percent that *includes active pages*,
  so it is not a reading of available memory.
- **`vm.memory_pressure`, the jetsam `memorystatus_available_pages`.** These are kernel policy inputs.
  `memorystatus_notify.md` says the warning and critical levels come from thresholds on
  `AVAILABLE_NON_COMPRESSED_MEMORY` relative to the compressor's compaction and swap points.
- **Activity Monitor's "memory pressure".** Apple says it "graphically represents how efficiently your memory is
  serving your processing needs", from free memory, swap rate, wired memory and cached files. It shows state; it
  is not a byte count.

**Two ways to read it, both checked against the sources:**
- **cgo plus Mach.** `host_statistics64(mach_host_self(), HOST_VM_INFO64, …)` is a public, documented Mach call
  (Apple's Kernel documentation lists `host_statistics64`). Chromium, sysinfo and the JDK use it. It reads every
  count in one call. The page size is `vm_kernel_page_size` (or `host_page_size`).
- **No cgo: x/sys/unix sysctls.** xnu exports the raw counters read-only in release kernels:
  - `vm.page_free_count`, `vm.page_speculative_count`, `vm.page_pageable_external_count` and
    `vm.page_pageable_internal_count` (`bsd/vm/vm_unix.c:2566-2574`, `CTLFLAG_RD`, outside the
    `DEBUG || DEVELOPMENT` guards).
  - `vm.page_purgeable_count` (`vm_unix.c:2464`) and `vm.pagesize`, which is the kernel's `page_size`
    (`vm_unix.c:2437`).

  The raw `vm_page_free_count` already excludes speculative pages (host.c:818 adds them), so the sysctl form is
  `page_free_count + page_pageable_external_count`, with **no** subtraction. This form skips the per-CPU local
  queue pages that host.c:871 adds. It also depends on speculative pages being counted inside
  `pageable_external`, which I did not find a line to prove. So it reads equal to or lower than Chromium's
  figure, never higher, which is the safe direction. The catch: these
  names are `OID_AUTO` entries declared in no public header, so Apple makes no stability promise.
- **Page-size trap (either path).** `hw.pagesize` is the *calling task's* map page size (`bsd/kern/kern_mib.c`,
  `sysctl_pagesize` → `vm_map_page_size(get_task_map(current_task()))`). An amd64 build running under Rosetta
  on Apple Silicon, *if* its task map uses 4 KiB pages, would multiply 16 KiB kernel page counts by 4 KiB and
  read 4× low. `kern_mib.c` does not mention translated processes, so that case is unverified. Either way, the
  page counts are in kernel pages: use `vm.pagesize` (the kernel's `page_size`) or `vm_kernel_page_size`, never
  `hw.pagesize` or `os.Getpagesize()`. gopsutil also resolves `vm_kernel_page_size` by `Dlsym`
  (`mem/mem_darwin.go`) rather than using the process page size.
- **Total, for sanity checks.** `hw.memsize` (`kern_mib.c`, `SYSCTL_QUAD … &max_mem_actual`), as in gopsutil,
  libuv, the JDK and Chromium.

Containers: macOS has no cgroups, and Docker Desktop runs Linux in a VM, where the Linux reader applies. No
container limit applies on the macOS side.

### 3.3 Windows

- **Physical.** `GlobalMemoryStatusEx` → `ullAvailPhys`, "the amount of physical memory that can be immediately
  reused without having to write its contents to disk first. It is the sum of the size of the standby, free, and
  zero lists" (MEMORYSTATUSEX docs). Standby is cached file memory that has not been modified, so this is the
  direct equivalent of `MemAvailable`. gopsutil, sysinfo, libuv (`uv_get_free_memory` and
  `uv_get_available_memory`) and the JDK (`os::win32::available_memory`) all use it. Chromium describes its
  Windows `avail_phys` field with the same MSDN wording (`process_metrics.h`); I did not read its Windows
  `GetAvailablePhysicalMemory` body.
- **Commit.** `ullAvailPageFile` is "the maximum amount of memory the current process can commit … equal to or
  smaller than the system-wide available commit value". Commit is the line past which allocations *fail*.
  Kubernetes' Windows eviction uses commit headroom, `CommitLimit − CommitTotal` from `GetPerformanceInfo`
  (`pkg/kubelet/eviction/memory_threshold_notifier_windows.go:73-80`;
  `server/stats/summary_sys_containers_windows.go:118-122`).
- **Job objects (Windows containers, and any parent that puts codectx in a job).** Process-isolated Windows
  containers enforce `--memory` with `JOB_OBJECT_LIMIT_JOB_MEMORY` on the container's job (Microsoft,
  "Implementing resource controls"). That is a *commit* limit (JOBOBJECT_EXTENDED_LIMIT_INFORMATION docs).
  .NET's GC comments: "The commit limit is already reflected by default when you run in a job but the physical
  memory load is not" (`src/coreclr/gc/windows/gcenv.windows.cpp:285-286`), and it then takes the job limit as
  its physical ceiling (`:288-300`).
  - `ullAvailPageFile` therefore already carries the job's commit headroom. That claim rests on .NET's comment;
    Microsoft's own page does not say it, so a test must prove it on Windows.
  - `QueryInformationJobObject(NULL, JobObjectExtendedLimitInformation, …)` reads "the job associated with the
    calling process … If the job is nested, the immediate job". The structure has only *peak* job usage
    (`PeakJobMemoryUsed`), not current usage. `JobMemoryLimit − PeakJobMemoryUsed` is therefore a conservative
    fallback if the proof fails.
- **Decision for Windows:** `min(ullAvailPhys, ullAvailPageFile)`, from one `GlobalMemoryStatusEx` call through
  `windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")`. That is how kubelet
  (`pkg/kubelet/winstats/perfcounter_nodestats_windows.go:80-82`) and gopsutil (`mem/mem_windows.go`) do it.
  This mirrors Linux's `min(MemAvailable, cgroup headroom)`: physical room on the host, capped by what this
  process's job and the system can still commit.
  - With a per-process limit (`JOB_OBJECT_LIMIT_PROCESS_MEMORY`), `ullAvailPageFile` describes codectx's own
    process, while each child has a cap of its own. The min then reads low for the children's total, which is
    the safe direction.

### 3.4 Pressure signals

- **macOS, push.** `dispatch_source_create(DISPATCH_SOURCE_TYPE_MEMORYPRESSURE, …)` with WARN/CRITICAL masks
  (`memorystatus_notify.md`). This needs libdispatch, so cgo or purego. The kqueue filter beneath it
  (`EVFILT_MEMORYSTATUS`) is in `event_private.h` only.
- **macOS, poll.** `kern.memorystatus_vm_pressure_level` (`bsd/kern/kern_memorystatus_notify.c:1885-1910`). It
  skips the privilege check on macOS (`#if !XNU_TARGET_OS_OSX` guards it) and returns
  `NOTE_MEMORYSTATUS_PRESSURE_NORMAL/WARN/CRITICAL` = 1/2/4. It is `CTLFLAG_MASKED`: hidden from `sysctl -a`
  but readable by name, and it works from x/sys/unix without cgo.
- **Windows.** `CreateMemoryResourceNotification(Low/HighMemoryResourceNotification)` plus
  `QueryMemoryResourceNotification` or a wait. It is a system-wide event, and the docs note "a range of memory
  availability where neither … is signaled". The calls are not in x/sys/windows, so they would also go through
  a lazy kernel32 proc.
- **What they add here:** little. codectx already reads memory before *every* admission (`compose.go:210-221`),
  and the ledger never takes back what it admitted (`admission.go:496-503`). A push signal could only change the
  next admission, and the fresh reading already covers that. The one extra bit is the OS's view of compressor
  and swap state, which free plus file-backed does not show.

## 4. Options

| | (a) Keep the stand-in, fix the rule | (b1) Native, x/sys only | (b2) Native, cgo Mach on macOS + x/sys on Windows | (c) gopsutil v4 | (d) add pressure signals (on top of b) |
|---|---|---|---|---|---|
| macOS reading | none, 8 GiB | `vm.page_free_count + vm.page_pageable_external_count` × `vm.pagesize` (sysctl) | `host_statistics64`: `free − speculative + external` × `vm_kernel_page_size` | `free + inactive` (counts anonymous pages) | `kern.memorystatus_vm_pressure_level` poll: warn/critical → allocation 0 |
| Windows reading | none, 8 GiB | `min(ullAvailPhys, ullAvailPageFile)` through a lazy kernel32 proc | same as b1 | `ullAvailPhys` only (no commit or job term) | `QueryMemoryResourceNotification(Low)` → allocation 0 |
| Matches Linux `MemAvailable` | no | yes | yes | no on macOS; Windows lacks the job/commit term | n/a |
| New dependency | none | none | none (cgo already required, `release.yml:148-169`) | gopsutil v4 + `ebitengine/purego`, `yusufpapurcu/wmi`, `go-ole`, `tklauser/go-sysconf`, `numcpus`, … (its `go.mod`) | none |
| API stability | n/a | macOS sysctl names are `OID_AUTO`, not in public headers | public Mach API, used by Chromium, sysinfo, the JDK, libuv | inherits gopsutil's choices | macOS sysctl is masked and undocumented. Windows API is documented |
| Residency (own + children) | not needed | needs its own readers. x/sys/unix (darwin) has no `task_info`, `proc_pidinfo` or `proc_pid_rusage` wrapper, and `Getrusage` returns only `Maxrss`, a peak (`go doc`), so macOS current RSS needs cgo (or purego) anyway | same Mach/cgo file can host `task_info`/`proc_pid_rusage` | gopsutil `process` could give RSS, but polls by pid | n/a |
| Host safety | over-admits on small Macs and PCs; under-uses big ones | right | right | over-admits on macOS (counts anonymous inactive pages) | adds an OS-judged brake |
| Testable | Linux now | pure arithmetic on Linux now; reading on macOS/Windows CI | same | gopsutil's own tests | needs real pressure; hard to provoke in CI |

## 5. Recommendation

**Adopt (b2), plus (a)'s one rule for an unreadable host and residency readers. Defer (d) and record where it would plug in.**

1. **macOS:** `host_statistics64(HOST_VM_INFO64)` through a small cgo file:
   `max(free_count − speculative_count, 0) + external_page_count`, times `vm_kernel_page_size`. The reasons:
   - it is the public API that Chromium, sysinfo and the JDK use;
   - it reads one consistent snapshot;
   - it matches what `MemAvailable` means;
   - cgo costs nothing new, because the product already ships with `CGO_ENABLED=1`, and x/sys/unix has no
     current-RSS call on darwin (only `Getrusage`'s peak `Maxrss`), so the macOS residency reader needs Mach
     or libproc through cgo anyway.

   Build tags: `darwin && cgo` for the reader, and `darwin && !cgo` falls back to unobserved, so a `CGO_ENABLED=0`
   vet or cross build still compiles. If no cgo is wanted in `internal/admission`, b1's sysctl form is
   verified (§3.2) and is the right fallback; keep the `vm.pagesize` rule either way.
2. **Windows:** `min(ullAvailPhys, ullAvailPageFile)` from one `GlobalMemoryStatusEx` call through
   `windows.NewLazySystemDLL`. This takes no new module. A test must prove that `ullAvailPageFile` reflects
   a job memory limit. If it does not, add `JobMemoryLimit − PeakJobMemoryUsed` from
   `QueryInformationJobObject(0, JobObjectExtendedLimitInformation)`, which is already in x/sys/windows.
3. **One rule for an unreadable host.** Remove the unobserved branch from `Machine.Allocation`. It should
   take only an observed reading, and `SchedulingAllocation` stays the single place an unobserved machine becomes
   `(UnobservedAllocationBytes, false)`. After b2, that is only FreeBSD/other platforms and a failed call. The
   comments at `machine.go:48-56` and `machine_other.go:5-12` change with it.
4. **Residency, required for parity.** Without it, an observed macOS or Windows reading makes codectx
   throttle itself (§2). It needs:
   - own RSS: macOS `task_info`, Windows `GetProcessMemoryInfo` through a lazy psapi proc;
   - children's resident memory: macOS `proc_pid_rusage` for each child pid; Windows, the per-child job codectx
     already creates (`internal/process/process_windows.go:20-40`) → `JobObjectBasicProcessIdList` → each
     process's memory counters.

   The prior research names the same per-process calls (`docs/research/raw/native-engine-need/01-need-derived-memory.md:105-110,327`).
   The idle-footprint stand-in (`config/machine.go:29-44`) then stops applying on these platforms automatically.
5. **Defer (d).** It is redundant with the reading before every admission (§3.4), and CI cannot exercise it.
   The plug-in point if wanted later: in `ledgerAllocation.rederive` (`compose.go:210`), a WARN/CRITICAL level
   sets the allocation to 0. That reuses the existing "zero = run alone" rule (`admission.go:120-131`), so no new
   mechanism is needed.

Not chosen:
- **(c) gopsutil.** It brings five modules and its macOS formula counts anonymous inactive pages, which is the
  over-admission this decision exists to stop.
- **(a) alone.** It leaves the default macOS and Windows experience at a fixed 8 GiB that is either unsafe or
  wasteful.

## 6. What the design changes

- **macOS reader.** `internal/admission/machine_darwin.go` (`darwin && cgo`):
  - `host_statistics64(HOST_VM_INFO64)` → `max(free − speculative, 0) + external`, times `vm_kernel_page_size`;
  - unobserved on a non-`KERN_SUCCESS` result.

  `machine_other.go` becomes `!linux && !windows && !(darwin && cgo)`. The arithmetic lives in a function
  without build tags (for example `darwinAvailable(free, spec, external, pageSize uint64) (int64, bool)`), so
  Linux CI tests it.
- **Windows reader.** `internal/admission/machine_windows.go`:
  - lazy `kernel32!GlobalMemoryStatusEx` with `dwLength` set → `min(ullAvailPhys, ullAvailPageFile)`;
  - unobserved on a zero return.

  Same split into a pure combine function. Job-object fallback only if the job-limit test fails.
- **One rule.** `Machine.Allocation` loses its unobserved branch. `SchedulingAllocation` is the only place
  the stand-in appears. Update the doc comments at `machine.go:19-25,48-56` and `machine_other.go`.
- **Residency** (required):
  - `internal/residency` gains darwin (`task_info`) and windows (`GetProcessMemoryInfo`) readers for `Resident`;
  - `internal/process` gains darwin and windows `anonBytes` sources, replacing `treesample_other.go:30` for
    those two platforms (macOS `proc_pid_rusage` for each child; Windows per-child job → process list → memory
    counters).
  - Figures a platform does not report stay `nil`, never 0 (`residency.go:7-8`).
- **Docs.** The design record and the admission docs name Linux, macOS and Windows as observed and say the stand-in
  is only for other platforms and failed calls.

**Tests:**
- **Observed (macOS and Windows, platform CI):** `ObserveMachine()` returns `Observed == true` and
  `0 < AvailableBytes ≤ total`, where total is `hw.memsize` or `ullTotalPhys`.
- **Arithmetic (Linux):** the darwin arithmetic on a fake snapshot:
  - speculative > free clamps to 0;
  - a 16 KiB page multiplies correctly;
  - overflow saturates or reports unobserved.

  The Windows combine on fake structs: the min of the two, and zero or failure → unobserved.
- **One stand-in rule (Linux):** a forced unobserved `Machine{}` gives `SchedulingAllocation` = `(8 GiB, false)`, and
  `rederive` leaves the ledger at the stand-in. No other caller can turn an unobserved reading into an
  allocation (mutation: put back `Allocation`'s 0 branch and call it from a test helper, and the build or test
  fails).
- **Residency (macOS and Windows, platform CI):** the residency readers return non-nil `Resident` for the test process.
  After a child is started that allocates and touches N MiB, `memoryAllocation` with the child's residency
  added back drops by much less than N (the Linux "own growth does not throttle" property).
- **Job limit (Windows, platform CI):** the parent first asserts that its own reading is greater than 512 MiB, so the
  test cannot pass by accident on a runner short of memory. It then re-runs its own binary inside a new job with
  `JOB_OBJECT_LIMIT_JOB_MEMORY` = 512 MiB (`CreateJobObject`/`SetInformationJobObject`/`AssignProcessToJobObject`,
  all in x/sys/windows), and the child asserts `AvailableBytes ≤ 512 MiB`. This proves or disproves the .NET
  claim that the job commit limit shows in `ullAvailPageFile`.

**Caveat.** `ci.yml` runs on Ubuntu only (`ci.yml:17`). The observed, residency and job-limit tests run only in the
platform CI legs, so platform defects surface there. The arithmetic and one-rule tests run on Linux.

## 7. Sources

Web (read Oct 9, 2026):
- gopsutil `mem/mem_darwin.go`: https://raw.githubusercontent.com/shirou/gopsutil/master/mem/mem_darwin.go
- gopsutil `mem/mem_windows.go`: https://raw.githubusercontent.com/shirou/gopsutil/master/mem/mem_windows.go
- gopsutil `internal/common/common_darwin.go` (purego `Dlopen`, `host_statistics` binding): https://raw.githubusercontent.com/shirou/gopsutil/master/internal/common/common_darwin.go
- gopsutil `go.mod`: https://raw.githubusercontent.com/shirou/gopsutil/master/go.mod
- Rust sysinfo, Apple `system.rs`: https://raw.githubusercontent.com/GuillaumeGomez/sysinfo/main/src/unix/apple/system.rs
- Rust sysinfo, Windows `system.rs`: https://raw.githubusercontent.com/GuillaumeGomez/sysinfo/main/src/windows/system.rs
- Chromium `base/process/process_metrics_apple.mm` (`GetSystemMemoryInfo`, `GetAvailablePhysicalMemory`): https://chromium.googlesource.com/chromium/src/+/main/base/process/process_metrics_apple.mm
- Chromium `base/process/process_metrics.h` (`avail_phys` comment): https://chromium.googlesource.com/chromium/src/+/main/base/process/process_metrics.h
- Chromium `base/system/sys_info_mac.mm`: https://chromium.googlesource.com/chromium/src/+/main/base/system/sys_info_mac.mm
- libuv `src/unix/darwin.c`: https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/darwin.c
- libuv `src/win/util.c`: https://raw.githubusercontent.com/libuv/libuv/v1.x/src/win/util.c
- OpenJDK `os_bsd.cpp`: https://raw.githubusercontent.com/openjdk/jdk/master/src/hotspot/os/bsd/os_bsd.cpp
- OpenJDK `os_windows.cpp`: https://raw.githubusercontent.com/openjdk/jdk/master/src/hotspot/os/windows/os_windows.cpp
- .NET runtime GC `gcenv.windows.cpp`: https://raw.githubusercontent.com/dotnet/runtime/main/src/coreclr/gc/windows/gcenv.windows.cpp
- Kubernetes kubelet `winstats/perfcounter_nodestats_windows.go`: https://raw.githubusercontent.com/kubernetes/kubernetes/master/pkg/kubelet/winstats/perfcounter_nodestats_windows.go
- Kubernetes kubelet `eviction/memory_threshold_notifier_windows.go`: https://raw.githubusercontent.com/kubernetes/kubernetes/master/pkg/kubelet/eviction/memory_threshold_notifier_windows.go
- Kubernetes kubelet `server/stats/summary_sys_containers_windows.go`: https://raw.githubusercontent.com/kubernetes/kubernetes/master/pkg/kubelet/server/stats/summary_sys_containers_windows.go
- Bazel `LocalHostComputeResources.java`: https://raw.githubusercontent.com/bazelbuild/bazel/master/src/main/java/com/google/devtools/build/lib/actions/LocalHostComputeResources.java
- Bazel `ResourceConverter.java` (`HOST_RAM`): https://raw.githubusercontent.com/bazelbuild/bazel/master/src/main/java/com/google/devtools/build/lib/util/ResourceConverter.java
- Bazel user manual 8.8.1 (`--local_ram_resources`): https://raw.githubusercontent.com/bazelbuild/bazel/master/docs/versions/8.8.1/docs/user-manual.mdx
- Go, "A Guide to the Go Garbage Collector": https://go.dev/doc/gc-guide
- xnu `doc/vm/memorystatus_notify.md`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/doc/vm/memorystatus_notify.md
- xnu `doc/vm/memorystatus.md`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/doc/vm/memorystatus.md
- xnu `osfmk/kern/host.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/kern/host.c
- xnu `osfmk/mach/vm_statistics.h`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/mach/vm_statistics.h
- xnu `bsd/vm/vm_unix.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/vm/vm_unix.c
- xnu `bsd/kern/kern_mib.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/kern/kern_mib.c
- xnu `bsd/kern/kern_memorystatus.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/kern/kern_memorystatus.c
- xnu `bsd/kern/kern_memorystatus_notify.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/kern/kern_memorystatus_notify.c
- xnu `osfmk/vm/vm_pageout.c`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/vm/vm_pageout.c
- xnu `osfmk/vm/vm_page.h`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/vm/vm_page.h
- xnu `bsd/sys/event_private.h`: https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/sys/event_private.h
- Apple, "host_statistics64" (Kernel docs; only the page title rendered): https://developer.apple.com/documentation/kernel/1502863-host_statistics64
- Apple, "View memory usage in Activity Monitor on Mac": https://support.apple.com/guide/activity-monitor/view-memory-usage-actmntr1004/mac
- Microsoft, MEMORYSTATUSEX: https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
- Microsoft, CreateMemoryResourceNotification: https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification
- Microsoft, QueryInformationJobObject: https://learn.microsoft.com/en-us/windows/win32/api/jobapi2/nf-jobapi2-queryinformationjobobject
- Microsoft, JOBOBJECT_EXTENDED_LIMIT_INFORMATION: https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_extended_limit_information
- Microsoft, JOBOBJECT_LIMIT_VIOLATION_INFORMATION: https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_limit_violation_information
- Microsoft, "Implementing resource controls" (Windows containers): https://learn.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/resource-controls

Local: the files and lines in §2. Also:
- `go doc` lookups of `golang.org/x/sys/windows` (GOOS=windows) and `golang.org/x/sys/unix` (GOOS=darwin) at v0.48.0;
- `go doc runtime` (GOMEMLIMIT default);
- `docs/adr/ADR-0010-engine-memory.md:34-55`;
- `docs/research/raw/native-engine-need/01-need-derived-memory.md:18,105-110,327-337,383-384`.
