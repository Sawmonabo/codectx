package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A machine held to part of its cores must be read as that part. Mutations
// this fails on:
//   - CPUs returning cpuCount() alone, ignoring the quota: a process held to
//     two cores of a sixteen-core host would run sixteen parser workers and
//     reserve sixteen workers' memory on a machine that can run two, which is
//     how a container freezes the host it is capped on.
//   - CPUs taking the quota without the ok flag: an unread quota would become
//     a machine with no cores, which is an unavailable measurement published
//     as a zero.
//   - CPUs taking the quota rather than the smaller of the two: a host whose
//     affinity is narrower than its quota would be over-read.
func TestCPUsIsTheSmallerOfAffinityAndQuota(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cpus          int
		quota         int
		quotaObserved bool
		want          int
	}{
		{"quota binds", 16, 2, true, 2},
		{"affinity binds", 2, 16, true, 2},
		{"no quota stated is not a quota of zero", 16, 0, false, 16},
		{"neither is ever below one", 0, 0, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpuCount = func() int { return tc.cpus }
			cpuQuota = func() (int, bool) { return tc.quota, tc.quotaObserved }
			t.Cleanup(func() { cpuCount, cpuQuota = runtime.NumCPU, observedCPUQuota })
			if got := CPUs(); got != tc.want {
				t.Fatalf("CPUs() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A quota stated on an ANCESTOR group binds this process, and that is the
// case the product actually meets: a container and a transient service both
// put the process in a child group whose own cpu.max states no limit.
// Mutation: read only the process's own group (drop cgroupAncestors) and this
// fails, reporting the whole host's cores to a two-core container.
func TestCPUQuotaReadsTheWholeHierarchy(t *testing.T) {
	root := t.TempDir()
	group := filepath.Join(root, "user.slice", "session.scope")
	if err := os.MkdirAll(group, 0o700); err != nil {
		t.Fatal(err)
	}
	// The limit is on user.slice; the group the process is in states none.
	write(t, filepath.Join(root, "user.slice", "cpu.max"), "200000 100000\n")
	write(t, filepath.Join(group, "cpu.max"), "max 100000\n")
	proc := filepath.Join(root, "procself")
	write(t, proc, "0::/user.slice/session.scope\n")

	cgroupRoot, procCgroupPath = root, proc
	t.Cleanup(func() { cgroupRoot, procCgroupPath = "/sys/fs/cgroup", "/proc/self/cgroup" })

	cores, ok := observedCPUQuota()
	if !ok || cores != 2 {
		t.Fatalf("observedCPUQuota() = %d, %v; want 2, true", cores, ok)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
