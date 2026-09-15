package main

// Env-gated profiling. `CODECTX_PPROF_DIR=<dir>` makes this process write a
// CPU profile for the whole run plus a heap and an allocation profile at exit
// into that directory. It is a diagnostics hook, not a flag: it must be
// settable for a run that is already misbehaving without changing the command
// line, and it must reach the re-executed parser workers, which inherit the
// environment but not argv. Each process names its files by role and pid, so a
// parent and its workers never collide and the split between them is visible.
//
// Nothing is written and nothing is refused when the variable is unset or the
// directory cannot be opened: a profiling hook that failed a run would be a
// worse diagnostic than no profile.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
)

// pprofEnv is the variable that turns profiling on and names the directory.
const pprofEnv = "CODECTX_PPROF_DIR"

// startProfiling begins a CPU profile when pprofEnv names a writable
// directory. The returned stop function writes the heap and allocation
// profiles and closes everything; it is safe to call when profiling never
// started.
func startProfiling(role string) func() {
	dir := os.Getenv(pprofEnv)
	if dir == "" {
		return func() {}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "codectx: %s: %v\n", pprofEnv, err)
		return func() {}
	}
	name := func(kind string) string {
		return filepath.Join(dir, fmt.Sprintf("%s-%d.%s.pprof", role, os.Getpid(), kind))
	}
	cpu, err := os.Create(name("cpu"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "codectx: %s: %v\n", pprofEnv, err)
		return func() {}
	}
	if err := pprof.StartCPUProfile(cpu); err != nil {
		fmt.Fprintf(os.Stderr, "codectx: %s: %v\n", pprofEnv, err)
		cpu.Close()
		return func() {}
	}
	return func() {
		pprof.StopCPUProfile()
		cpu.Close()
		writeProfile(name("allocs"), "allocs")
		runtime.GC()
		writeProfile(name("heap"), "heap")
	}
}

// writeProfile dumps one named runtime profile, reporting a failure to stderr
// rather than failing the run.
func writeProfile(path, which string) {
	p := pprof.Lookup(which)
	if p == nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codectx: %s: %v\n", pprofEnv, err)
		return
	}
	if err := p.WriteTo(f, 0); err != nil {
		fmt.Fprintf(os.Stderr, "codectx: %s: %v\n", pprofEnv, err)
	}
	f.Close()
}
