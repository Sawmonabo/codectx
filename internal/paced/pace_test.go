package paced

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The requirement: the rate at which disk space is given back is a property of
// the host's disk, so the process may hand the host one window per interval
// however many of its callers are freeing at once. Four reach the pace on real
// paths -- the reclaimer's own goroutine, the file system shim shortening a
// file the engine owns, a publication trimming its staging surface, a delta
// key file reset -- and a pace kept per charger would be four times the one
// the measurement established, which is a burst by another name.
//
// Mutation: release the pace lock before waiting (take paceMu only around the
// arithmetic in charge) and four chargers wait in parallel, giving the host
// four windows in one interval.
func TestEveryChargerInTheProcessWaitsUnderOnePace(t *testing.T) {
	r := newReclaimer()
	const chargers = 4
	var wg sync.WaitGroup
	start := time.Now()
	for range chargers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.charge(Window, nil)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	if want := (chargers - 1) * FreeInterval; elapsed < want {
		t.Fatalf("%d concurrent chargers of one window each took %v; one window per %v interval takes at least %v",
			chargers, elapsed, FreeInterval, want)
	}
}

// paceShareRole selects what a child of this test binary does, and
// paceShareDir is the turn directory the two processes share. It is a
// temporary directory, never the user's cache, so the test takes no turn in
// the pace of the host it runs on.
const (
	paceShareRole = "CODECTX_PACE_SHARE_ROLE"
	paceShareDir  = "CODECTX_PACE_SHARE_DIR"
)

// The requirement: two processes of one user are two callers of one host's
// disk, and the host tolerates a rate, not a rate each. An index run and a
// query server freeing at the pace each would hand the host twice what was
// measured to be safe, which is the burst the pace exists to prevent.
//
// The test is two real processes over one turn directory: one child frees
// two windows, the other frees two beside it, and the four windows between
// them take four intervals rather than the two they would take in parallel.
// It runs in children of the test binary because the turn directory is a
// process-wide choice.
//
// Mutation: drop the turn file (make turn report nil) and each process keeps
// the pace for itself, so the four windows take two intervals.
func TestTwoProcessesShareThePace(t *testing.T) {
	switch os.Getenv(paceShareRole) {
	case "runner":
		paceShareRunner(t)
		return
	case "helper":
		paceShareHelper(t)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), paceShareRole+"=runner", paceShareDir+"="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("two processes did not share the pace: %v\n%s", err, out)
	}
}

// paceShareRunner frees two windows through the shared turn directory while a
// second process frees two beside it, and times the four.
func paceShareRunner(t *testing.T) {
	dir := os.Getenv(paceShareDir)
	UseTurnDir(dir)

	helper := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.v")
	helper.Env = append(os.Environ(), paceShareRole+"=helper", paceShareDir+"="+dir)
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	// The child's start-up is not part of what is being timed: it says when
	// it is ready and waits to be let go.
	if !waitFor(60*time.Second, func() bool { return exists(filepath.Join(dir, "ready")) }) {
		t.Fatal("the second process never reached the pace")
	}
	start := time.Now()
	if err := os.WriteFile(filepath.Join(dir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	Freed(Window)
	Freed(Window)
	if err := helper.Wait(); err != nil {
		t.Fatalf("the second process: %v", err)
	}
	elapsed := time.Since(start)
	// Four windows between two processes, one window per interval. Three
	// intervals is the bound: the two intervals the processes would take
	// freeing in parallel is what this rules out.
	if want := 3 * FreeInterval; elapsed < want {
		t.Fatalf("two processes gave the host four windows in %v; at one window per %v that takes at least %v",
			elapsed, FreeInterval, want)
	}
}

// paceShareHelper is the second process: it takes two of the four windows.
func paceShareHelper(t *testing.T) {
	dir := os.Getenv(paceShareDir)
	UseTurnDir(dir)
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !waitFor(60*time.Second, func() bool { return exists(filepath.Join(dir, "go")) }) {
		t.Fatal("the first process never let the second start")
	}
	Freed(Window)
	Freed(Window)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
