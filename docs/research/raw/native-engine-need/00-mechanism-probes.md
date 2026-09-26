# 00 — Mechanism probes on this host (self-verification only)

Date 2026-09-26. Kernel `6.18.33.2-microsoft-standard-WSL2`, 16 CPUs, 47.0 GiB total, 11.4 GiB available at probe
time. Each probe ran under `ulimit -v 2000000` in a scratch directory under the user cache, touched at most 300 MiB,
and was deleted after the research closed. No product, benchmark or repository was run.

## P1. `clear_refs` value 5 resets `VmHWM`

A Python process allocated and touched 300 MiB, freed it, wrote `5` to `/proc/self/clear_refs`, then touched 50 MiB.

```
start                {'VmHWM': '9716 kB',   'VmRSS': '9716 kB'}
after 300MiB touch   {'VmHWM': '317176 kB', 'VmRSS': '317176 kB'}
after free           {'VmHWM': '317076 kB', 'VmRSS': '9972 kB'}
after clear_refs=5   {'VmHWM': '9972 kB',   'VmRSS': '9972 kB'}
after 50MiB touch    {'VmHWM': '61176 kB',  'VmRSS': '61176 kB'}
```

Result: writing `5` resets the peak resident set to the current resident set, and the next peak is the peak of what
followed. A process can therefore measure its own per-file peak exactly: reset before the file, read `VmHWM` after.
The mechanism is documented in the kernel's procfs documentation (`Documentation/filesystems/proc.rst`, the
`clear_refs` table: "5 — reset the peak resident set size ("high water mark") to the process's current value").

## P2. Cost of each observation

1,000 reads each, same process:

```
/proc/self/status        6.85 ms total  -> 6.9 µs per read
/proc/self/smaps_rollup  183.2 ms total -> 183 µs per read
```

`smaps_rollup` is 27× the cost of `status` and walks every mapping; it adds `Pss`, `Private_Dirty` and
`Anonymous`, which separate shared grammar text from the file's own memory. Sample of its output at 61 MB resident:
`Rss 61308 kB, Pss 56613 kB, Private_Dirty 54440 kB, Shared_Clean 6764 kB, Anonymous 54440 kB`.

## P3. Can an unprivileged process create a child cgroup here?

```
$ cat /proc/self/cgroup                              -> 0::/init.scope
$ ls -ld /sys/fs/cgroup/init.scope                   -> drwxr-xr-x root root
$ mkdir /sys/fs/cgroup/init.scope/probe              -> mkdir: Permission denied
$ mkdir /sys/fs/cgroup/probe                         -> mkdir: Permission denied
$ ls -ld /sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service
                                                     -> drwxr-xr-x+ <the user> (delegated)
$ cat .../user@1000.service/cgroup.subtree_control   -> cpu memory pids
```

Result: a process started from an ordinary login shell on this host sits in `init.scope`, owned by root, and cannot
create a child. The user's systemd manager does hold a delegated subtree with the memory controller enabled, reachable
only by asking that manager for a scope (`systemd-run --user --scope`) — a host where no user manager runs (containers,
many CI runners, macOS, Windows) has no such subtree. The design therefore cannot depend on a cgroup.

## P4. `memory.peak` in a delegated user scope, and what a reset means

Inside `systemd-run --user --scope`:

```
memory.peak at start                  3334144
after touching 80 MiB                88723456
echo reset > memory.peak (fresh open) reset_ok, re-read 88723456   (unchanged)
```

Second run, keeping the file descriptor that wrote the reset:

```
peak before reset                 87535616
same fd after writing "reset"      3747840
fresh open after the reset        87535616
same fd after touching 20 MiB     24616960
```

Result: on this kernel `memory.peak` accepts a reset, and the reset is local to the file descriptor that wrote it —
other readers keep the lifetime peak. A per-file peak through a cgroup needs one open descriptor held across files
and a scope per worker; it counts page cache charged to the group as well as anonymous memory. It measures the same
quantity P1 measures, at the price of the delegation P3 shows is not always available.
