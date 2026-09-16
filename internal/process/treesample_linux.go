//go:build linux

package process

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// treeSampled reports that this platform can observe a running process tree's
// resident memory. Where it is false the runner records the figure as
// unavailable rather than as zero, which Section 22 requires: a missing
// measurement that reads as "used no memory" is worse than no measurement.
const treeSampled = true

// maxSampledProcesses bounds one sweep of /proc. A sweep is O(processes on the
// machine), and Section 6 requires a finite bound on every traversal; 4096 is
// far above the process count of any machine this runs an analysis on, so the
// bound truncates a pathological /proc rather than a real tree.
const maxSampledProcesses = 4096

// treeSampler keeps the peak of the summed resident set size over a process
// group, sampled while the group runs.
//
// The peak is sampled concurrently and never computed by adding per-process
// historical high-water marks: those peaks occur at different instants, and
// summing them describes a moment that never existed (Section 22). The figure
// this produces is the tree sum, which is strictly above what /usr/bin/time
// reports, because that is the largest single process and not the tree
// (docs/research/10-round3-empirical.md §1).
type treeSampler struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
	// sample is written only by the sampling goroutine and read only after done
	// is closed, so the join in stopSampling is what publishes it.
	sample treeSample
	// cpu is the tree's summed user+system time in clock ticks as of the last
	// sweep. Unlike peak it is read while the run is still going, by the stall
	// watchdog, so it is atomic: a tree that produces no output but is burning
	// CPU is working, not wedged.
	cpu atomic.Int64
}

// startTreeSampler begins sampling the process group pgid every interval. The
// group is the runner's own: the child is a group leader, so its process ID is
// the group ID, and a descendant that re-parents away from it stays in the
// group. Membership by group, not by parent, is also exactly what the runner
// terminates.
func startTreeSampler(pgid int, interval time.Duration) *treeSampler {
	s := &treeSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// The sweep comes first so that a child which exits inside the
			// first interval is still measured at least once.
			sums := sumGroup(pgid)
			if sums.rss > s.sample.peakBytes {
				s.sample.peakBytes = sums.rss
			}
			if sums.ioSeen {
				// The byte counters are kept from the last sweep that still
				// found the tree. Sweeps after it exits observe nothing, and
				// letting those overwrite the totals with zero would erase the
				// measurement at exactly the moment it is wanted.
				s.sample.readBytes = sums.read
				s.sample.writeBytes = sums.write
				s.sample.ioSampled = true
			}
			s.cpu.Store(sums.ticks)
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

// stopSampling ends the sweep, joins the goroutine and returns what the sweeps
// observed. It is idempotent, and it never returns before the goroutine has
// finished: no goroutine this package starts outlives the run that started it.
func (s *treeSampler) stopSampling() treeSample {
	if s == nil {
		return treeSample{}
	}
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return s.sample
}

// cpuTicks reports the tree's summed user+system time as of the last sweep, or
// zero on a platform or a sweep that could not observe it. A caller uses it
// only as a change signal, never as a duration.
func (s *treeSampler) cpuTicks() int64 {
	if s == nil {
		return 0
	}
	return s.cpu.Load()
}

// groupSums is one sweep's totals over a process group.
type groupSums struct {
	rss   int64
	ticks int64
	read  int64
	write int64
	// ioSeen reports that at least one member had readable byte counters, so a
	// sweep that found the tree is distinguishable from one that found nothing
	// and from one whose members all denied their counters.
	ioSeen bool
}

// sumGroup adds the resident set size, the consumed CPU ticks and the
// transferred bytes of every live process in the group. Processes that exit
// mid-sweep are simply absent from the sums; an unreadable /proc yields an
// empty sweep rather than aborting the run.
func sumGroup(pgid int) groupSums {
	var sums groupSums
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return sums
	}
	pageSize := int64(os.Getpagesize())
	var scanned int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if scanned++; scanned > maxSampledProcesses {
			break
		}
		group, pages, cpu, ok := statGroup(pid)
		if !ok || group != pgid {
			continue
		}
		sums.rss += pages * pageSize
		sums.ticks += cpu
		// Only a member of the group is opened a second time, so the extra
		// read costs the size of the tree and not the size of /proc.
		if read, write, ok := ioBytes(pid); ok {
			sums.read += read
			sums.write += write
			sums.ioSeen = true
		}
	}
	return sums
}

// ioBytes reads one process's transferred byte counters from /proc/<pid>/io.
//
// The figures are the counts taken where the process called the kernel, not
// the block-layer totals that stand beside them in the same file: a child
// whose output is still in the page cache, or whose work directory is a memory
// filesystem, has transferred every byte it wrote and moved none of them to a
// disk, and the block-layer counters would report that work as having cost
// nothing.
//
// An entry that cannot be read -- the process exited mid-sweep, or the kernel
// denies the counters to this user -- contributes nothing and is not an error,
// which is the posture the sweep already takes for an unreadable stat.
func ioBytes(pid int) (read, write int64, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
	if err != nil {
		return 0, 0, false
	}
	var found int
	for line := range strings.Lines(string(raw)) {
		name, value, cut := strings.Cut(strings.TrimSpace(line), ": ")
		if !cut {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch name {
		case "rchar":
			read, found = n, found+1
		case "wchar":
			write, found = n, found+1
		}
	}
	// Both counters or neither: a half-read file is an unreadable one, so a
	// missing figure is never summed in as a zero.
	return read, write, found == 2
}

// statGroup reads one process's group ID, resident pages and consumed CPU
// ticks from /proc/<pid>/stat.
func statGroup(pid int) (pgid int, rssPages, cpuTicks int64, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, 0, false
	}
	// Field 2 is the executable name in parentheses and may itself contain
	// spaces and parentheses, so the numbered fields are read from after its
	// last ')'. Splitting the whole line on spaces misreads every field after
	// it for a process whose name contains one.
	close := bytes.LastIndexByte(raw, ')')
	if close < 0 || close+2 >= len(raw) {
		return 0, 0, 0, false
	}
	fields := strings.Fields(string(raw[close+2:]))
	// fields[0] is field 3 (state), so field N is at index N-3: the process
	// group is field 5, user time field 14, system time field 15, and the
	// resident set size, in pages, field 24.
	const pgrpIndex, utimeIndex, stimeIndex, rssIndex = 2, 11, 12, 21
	if len(fields) <= rssIndex {
		return 0, 0, 0, false
	}
	group, err := strconv.Atoi(fields[pgrpIndex])
	if err != nil {
		return 0, 0, 0, false
	}
	pages, err := strconv.ParseInt(fields[rssIndex], 10, 64)
	if err != nil || pages < 0 {
		return 0, 0, 0, false
	}
	// CPU time is advisory: a tree whose ticks cannot be parsed simply
	// contributes no CPU progress, and the byte counters still speak for it.
	utime, _ := strconv.ParseInt(fields[utimeIndex], 10, 64)
	stime, _ := strconv.ParseInt(fields[stimeIndex], 10, 64)
	return group, pages, max(utime, 0) + max(stime, 0), true
}
