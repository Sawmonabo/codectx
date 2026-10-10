# Available memory on macOS and Windows: which figure means "can be taken without swapping"

Research note for the codectx product owner, behind
[ADR-0019](../adr/ADR-0019-available-memory-macos-windows.md). Research date 2026-10-09. The full findings, the options
table and every source are in [raw/memory/findings.md](raw/memory/findings.md); sources are cited here as [n] from §5.

## 1. The question

Admission sums what heavy children reserve against one allocation derived from the machine's available memory
([ADR-0012](../adr/ADR-0012-native-dependence-engine.md) decision 5). Linux reports `MemAvailable` and cgroup headroom;
macOS and Windows report nothing in the current code, so admission uses a fixed 8 GiB stand-in, which over-admits on a
small, busy laptop and under-uses a large workstation. Three questions: which operating-system figure means the same as
`MemAvailable` on each platform; how to read it (the system call package only, cgo, or a third-party library); and
whether to also listen for memory-pressure signals.

## 2. What the field does

**What "available" means.** `MemAvailable` is memory a new workload can take without swapping: free memory plus file
cache the kernel can drop. Total memory is the wrong basis: a build tool scales total memory by a user fraction, and
the Go runtime sets no limit by default and advises none for desktop tools [1][2].

**macOS.** The kernel fills `host_statistics64(HOST_VM_INFO64)` with free (speculative read-ahead included),
file-backed, anonymous and compressor page counts [3]. Projects compute "available" from them differently:

| Formula | Who | Problem |
|---|---|---|
| free − speculative + file-backed | a major browser's process metrics [4]; the kernel's design note, "free + file_backed" under pressure [5] | none: it matches `MemAvailable` |
| free + inactive | a widely used Go system library [6] | inactive includes anonymous pages, freed only by compressing or swapping |
| free + inactive + purgeable | a cross-platform event library; a Java runtime [7][8] | the same |
| active + inactive + free + speculative | a Rust system library [9] | counts active pages |

The kernel's memory-status percentage and its pressure level are policy inputs, not byte counts [3][5]. The same
counters are exported as read-only sysctls, readable without cgo, but declared in no public header [3]. The page
counts are kernel pages; the per-process page size differs under translation [3].

**Windows.** `GlobalMemoryStatusEx`'s `ullAvailPhys` is "the sum of the size of the standby, free, and zero lists",
memory reusable without writing to disk; `ullAvailPageFile` is the most the process can commit [10]. A container's
memory limit is a job commit limit [11], and a runtime's collector notes that "the commit limit is already reflected by
default when you run in a job" [12]. A container orchestrator's node agent reads the same call [13]. The job query
reports only the job's peak usage, not its current usage [11].

**Pressure signals.** macOS offers a dispatch source and a masked sysctl; Windows offers memory resource notifications
with a gap where neither is signalled [5][14].

## 3. Findings

1. **macOS:** `max(free − speculative, 0) + file-backed`, times the kernel page size, from one `host_statistics64`
   call through cgo. The release is already built with cgo, and the product's own current resident set on macOS needs
   cgo anyway, because the system call package exposes only a peak (`Getrusage`'s `Maxrss`).
2. **Windows:** `min(ullAvailPhys, ullAvailPageFile)` from one call, the counterpart of Linux's
   `min(MemAvailable, cgroup headroom)`. Whether the commit figure carries a job's limit rests on one runtime's comment,
   so a Windows test must prove it; the fallback is the job limit less its peak usage.
3. **Residency is part of the decision:** without the product's own resident set and its children's on these
   platforms, an observed reading counts the children's use twice and the product throttles itself.
4. **One rule for an unreadable host:** the stand-in only where no reader exists or a call fails.
5. **Pressure signals are redundant** with a reading before every admission, and CI cannot provoke them; deferred.
6. **No third-party library:** the surveyed one adds five modules and its macOS formula counts anonymous inactive pages,
   the over-admission the decision exists to stop.

| Option | macOS reading | Windows reading | Matches `MemAvailable` | New dependency |
|---|---|---|---|---|
| Keep the stand-in | none | none | no | none |
| Sysctls only | free + file-backed (raw counters) | min of the two | yes | none; macOS names unpublished |
| **cgo kernel call on macOS, system DLL on Windows** | **free − speculative + file-backed** | **min of the two** | **yes** | **none** |
| Third-party library | free + inactive | physical only | no | five modules |
| Add pressure signals | a level that zeroes the allocation | the same | n/a | none |

## 4. Not tested hands-on

No reader was exercised on macOS or Windows; the findings rest on the kernel, runtime and library sources and the
platform documentation, read during the research. The claim that a job's commit limit shows in `ullAvailPageFile`, and
whether speculative pages are counted inside the file-backed sysctl, are unverified and named as such.

## 5. Sources

1. *LocalHostComputeResources.java*, https://raw.githubusercontent.com/bazelbuild/bazel/master/src/main/java/com/google/devtools/build/lib/actions/LocalHostComputeResources.java
2. *A Guide to the Go Garbage Collector*, https://go.dev/doc/gc-guide
3. *host.c*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/kern/host.c; *vm_unix.c*,
   https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/vm/vm_unix.c; *kern_mib.c*,
   https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/bsd/kern/kern_mib.c; *vm_pageout.c*,
   https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/osfmk/vm/vm_pageout.c
4. *process_metrics_apple.mm*, https://chromium.googlesource.com/chromium/src/+/main/base/process/process_metrics_apple.mm
5. *memorystatus.md*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/doc/vm/memorystatus.md;
   *memorystatus_notify.md*, https://raw.githubusercontent.com/apple-oss-distributions/xnu/main/doc/vm/memorystatus_notify.md
6. *mem_darwin.go*, https://raw.githubusercontent.com/shirou/gopsutil/master/mem/mem_darwin.go
7. *darwin.c*, https://raw.githubusercontent.com/libuv/libuv/v1.x/src/unix/darwin.c
8. *os_bsd.cpp*, https://raw.githubusercontent.com/openjdk/jdk/master/src/hotspot/os/bsd/os_bsd.cpp
9. *system.rs*, https://raw.githubusercontent.com/GuillaumeGomez/sysinfo/main/src/unix/apple/system.rs
10. *MEMORYSTATUSEX*, https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/ns-sysinfoapi-memorystatusex
11. *Implementing resource controls*, https://learn.microsoft.com/en-us/virtualization/windowscontainers/manage-containers/resource-controls;
    *JOBOBJECT_EXTENDED_LIMIT_INFORMATION*, https://learn.microsoft.com/en-us/windows/win32/api/winnt/ns-winnt-jobobject_extended_limit_information
12. *gcenv.windows.cpp*, https://raw.githubusercontent.com/dotnet/runtime/main/src/coreclr/gc/windows/gcenv.windows.cpp
13. *perfcounter_nodestats_windows.go*, https://raw.githubusercontent.com/kubernetes/kubernetes/master/pkg/kubelet/winstats/perfcounter_nodestats_windows.go
14. *CreateMemoryResourceNotification*, https://learn.microsoft.com/en-us/windows/win32/api/memoryapi/nf-memoryapi-creatememoryresourcenotification
