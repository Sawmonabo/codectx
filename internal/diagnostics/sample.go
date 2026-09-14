package diagnostics

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ProcessCounter reports how many children a process runner currently has
// running. It is the one method of *process.Runner this package needs, stated
// as an interface so a sampler can be exercised without starting a child.
//
// A sampler handed no counter reports the figure as absent, not as zero: an
// application whose runners were never wired is not an application running no
// subprocesses.
type ProcessCounter interface {
	LiveSubprocesses() int64
}

// HostSamplerOptions are the host locations and counters a sampler reads. Every
// one is optional: an absent dependency makes its metric absent, never zero.
type HostSamplerOptions struct {
	// CASDir is the content-addressed store directory (snapshot.CASDir). The
	// path is supplied rather than derived here so the data-directory layout
	// stays owned by internal/snapshot alone.
	CASDir string
	// TempDirs are the directories whose bytes count against
	// resources.max_temp_bytes: staging, materializations and query spools.
	TempDirs []string
	// Processes are the runners whose live children are counted. All of them
	// are summed, because a report that named only one would describe part of
	// the process budget as the whole of it.
	Processes []ProcessCounter
}

// maxWalkedEntries bounds one directory measurement. Section 6 requires a
// finite bound on every traversal, and a content-addressed store is the one
// directory in this application that grows with the repository. A walk that
// reaches the bound reports the figure as absent rather than partial, because a
// CAS size that silently stops at 200k objects is a wrong measurement, not an
// incomplete one.
const maxWalkedEntries = 200000

// HostSampler is the Sampler implementation that reads this host.
//
// Everything the host can be asked in one pass is asked in one pass: the parent
// resident set, the base parser workers and the native analyzer children are
// summed from a single /proc sweep, so the three figures describe one instant.
// Section 22 forbids the alternative -- adding per-process historical peaks
// reached at different moments describes a moment that never existed.
//
// The rule inherited from the L0 slice: a metric that cannot be measured stays
// nil. It is never defaulted to zero, because a zero resident set reads as a
// process using no memory, and Section 23 requires an unavailable figure be
// reported as unavailable.
type HostSampler struct {
	opts HostSamplerOptions

	// The readers below are fields rather than direct calls so every
	// unmeasurable path is provable on a host that can measure. Otherwise the
	// invariant that guards darwin and windows could only be tested there.

	// parentRSS reads this process's resident set size, or nil where the host
	// exposes no such figure.
	parentRSS func() *uint64
	// peakParentRSS reads this process's high-water resident set size.
	peakParentRSS func() *uint64
	// treeRSS sums this process's descendants in one sweep, split into the
	// base parser workers (this same executable re-executed under its worker
	// subcommand) and every other child. Both are nil on a host with no
	// process accounting, which is the darwin and windows case.
	treeRSS func() (base, native *uint64)
	// dirBytes sums a directory subtree, or reports absent.
	dirBytes func(string) *uint64
}

// NewHostSampler builds the sampler over the real host readers.
func NewHostSampler(opts HostSamplerOptions) *HostSampler {
	return &HostSampler{
		opts:          opts,
		parentRSS:     parentRSSBytes,
		peakParentRSS: peakParentRSSBytes,
		treeRSS:       descendantRSSBytes,
		dirBytes:      directoryBytes,
	}
}

// Sample reports what this host can measure. It returns no error for a metric
// it cannot read: the nil field is the answer, and an error would make an
// unmeasurable figure fail a call that is otherwise complete.
//
// The fields left nil here and not by absence of a reading are the ones no
// process-wide source exists for at this commit: pending watch events belong to
// a live watcher in this process only, and the unit reuse and parse counts are
// per-index-run figures on model.IndexResult, not running totals. Reporting
// them as zero would state that nothing is pending and nothing was parsed.
func (h *HostSampler) Sample(_ context.Context) (model.ResourceReport, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Sys is the total the runtime has obtained from the operating system,
	// which is the Go-managed figure Section 23 names -- not HeapAlloc, which
	// omits stacks, spans and the allocator's own bookkeeping.
	goManaged := ms.Sys

	base, native := h.treeRSS()
	report := model.ResourceReport{
		ParentRSSBytes:     h.parentRSS(),
		PeakParentRSSBytes: h.peakParentRSS(),
		GoManagedBytes:     &goManaged,
		BaseWorkerRSSBytes: base,
		NativeWorkerBytes:  native,
		LiveSubprocesses:   h.liveSubprocesses(),
	}
	if h.opts.CASDir != "" {
		report.CASBytes = h.dirBytes(h.opts.CASDir)
	}
	report.TempBytes = h.tempBytes()
	return report, nil
}

// liveSubprocesses sums every wired runner's live children. No runner wired is
// no measurement, which is not the same answer as no children.
func (h *HostSampler) liveSubprocesses() *int64 {
	if len(h.opts.Processes) == 0 {
		return nil
	}
	var total int64
	for _, p := range h.opts.Processes {
		if p == nil {
			continue
		}
		total += p.LiveSubprocesses()
	}
	return &total
}

// tempBytes sums the temporary directories. One unreadable directory makes the
// whole figure absent: a total that silently omits a subtree understates
// pressure against resources.max_temp_bytes, which is the one direction that
// matters.
func (h *HostSampler) tempBytes() *uint64 {
	if len(h.opts.TempDirs) == 0 {
		return nil
	}
	var total uint64
	for _, dir := range h.opts.TempDirs {
		if dir == "" {
			continue
		}
		n := h.dirBytes(dir)
		if n == nil {
			return nil
		}
		total += *n
	}
	return &total
}

// directoryBytes sums the apparent size of every regular file under dir. A
// directory that does not exist yet is legitimately zero bytes; any other
// failure, and a subtree larger than the traversal bound, is absent.
func directoryBytes(dir string) *uint64 {
	var total uint64
	var seen int
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if seen++; seen > maxWalkedEntries {
			return errWalkBound
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			// The file was removed while the walk ran, which a live CAS
			// temporary directory does constantly. It contributes nothing.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if size := info.Size(); size > 0 {
			total += uint64(size)
		}
		return nil
	})
	switch {
	case err == nil:
		return &total
	case os.IsNotExist(err):
		zero := uint64(0)
		return &zero
	default:
		return nil
	}
}

// errWalkBound stops a traversal that reached maxWalkedEntries. It is never
// returned to a caller: it makes the figure absent, which is the honest report
// for a subtree this sampler refused to finish walking.
var errWalkBound = errors.New("directory traversal reached its bound")

// parentRSSBytes reads this process's resident set size from /proc/self/statm,
// whose second field is the resident page count. A host without that file --
// every non-Linux platform, and a Linux host with /proc unmounted -- has no
// portable equivalent, so the figure is absent rather than zero.
func parentRSSBytes() *uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(raw))
	const residentIndex = 1
	if len(fields) <= residentIndex {
		return nil
	}
	pages, err := strconv.ParseUint(fields[residentIndex], 10, 64)
	if err != nil {
		return nil
	}
	bytes := pages * uint64(os.Getpagesize())
	return &bytes
}

// peakParentRSSBytes reads this process's high-water resident set size from the
// VmHWM line of /proc/self/status, in kibibytes. It is the kernel's own peak,
// not a figure this application sampled, so it covers the whole life of the
// process including work that finished before any sampler started.
func peakParentRSSBytes() *uint64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "VmHWM:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil
		}
		kib, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil
		}
		bytes := kib * 1024
		return &bytes
	}
	return nil
}

// descendantRSSBytes sums the resident set size of every descendant of this
// process in ONE sweep of /proc, split by what the child is.
//
// Both figures come from the same sweep, which is the Section 22 requirement:
// the parent, the base workers and the native analyzers must describe one
// instant, never three readings taken as the tree changed underneath them.
//
// Classification is by the child's comm: a base parser worker is this very
// executable re-executed under its worker subcommand, so its comm equals this
// process's own. comm is truncated to 15 bytes by the kernel, and comparing two
// equally truncated values is unaffected by that. Every other descendant -- a
// Git plumbing command, a language server, a SCIP indexer, the dependence
// engine -- is native.
//
// Membership is by parent chain, not by process group, because each child is
// its own group leader (internal/process places it there) and there is no
// registry of those groups here. The honest limitation, which
// docs/operations.md records: a descendant re-parented away after its parent
// died leaves this set and stops being counted. A host with no readable /proc
// reports both figures absent.
func descendantRSSBytes() (base, native *uint64) {
	self, selfComm, ok := selfIdentity()
	if !ok {
		return nil, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	pageSize := uint64(os.Getpagesize())
	all := make(map[int]procRecord, len(entries))
	var scanned int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// The same finite bound the runner's own sweep uses: one pass is
		// O(processes on the machine), and 4096 is far above the process count
		// of any host this runs on.
		if scanned++; scanned > maxSampledProcesses {
			break
		}
		comm, parent, pages, ok := statProcess(pid)
		if !ok {
			continue
		}
		all[pid] = procRecord{parent: parent, comm: comm, bytes: pages * pageSize}
	}
	var baseTotal, nativeTotal uint64
	for pid, rec := range all {
		if pid == self || !descends(all, pid, self) {
			continue
		}
		if rec.comm == selfComm {
			baseTotal += rec.bytes
			continue
		}
		nativeTotal += rec.bytes
	}
	return &baseTotal, &nativeTotal
}

// maxAncestorHops bounds the walk up one process's parent chain. A tree this
// application builds is at most three deep (this process, a runner's child, its
// own children), so the bound truncates a cycle in a malformed /proc rather
// than a real chain.
const maxAncestorHops = 16

// maxSampledProcesses bounds one sweep of /proc, matching the bound the
// runner's own tree sampler applies for the same reason.
const maxSampledProcesses = 4096

// procRecord is one process as this sweep read it.
type procRecord struct {
	parent int
	comm   string
	bytes  uint64
}

// descends reports whether pid is below root in the parent chain of one sweep.
// A chain that leaves the sweep -- the parent exited between the two reads --
// is not a descendant, which is the same absence the re-parenting limitation
// documented above produces.
func descends(all map[int]procRecord, pid, root int) bool {
	for hops := 0; hops < maxAncestorHops; hops++ {
		rec, ok := all[pid]
		if !ok || rec.parent <= 0 {
			return false
		}
		if rec.parent == root {
			return true
		}
		pid = rec.parent
	}
	return false
}

// statProcess reads one process's comm, parent and resident pages from
// /proc/<pid>/stat.
func statProcess(pid int) (comm string, parent int, rssPages uint64, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, 0, false
	}
	return parseStat(raw)
}

// parseStat splits one /proc/<pid>/stat line. Field 2 is the executable name in
// parentheses and may itself contain spaces and parentheses, so the numbered
// fields are read from after its last ')'. Splitting the whole line on spaces
// misreads every field after it for a process whose name contains one.
func parseStat(raw []byte) (comm string, parent int, rssPages uint64, ok bool) {
	open := bytes.IndexByte(raw, '(')
	closing := bytes.LastIndexByte(raw, ')')
	if open < 0 || closing <= open || closing+2 >= len(raw) {
		return "", 0, 0, false
	}
	comm = string(raw[open+1 : closing])
	fields := strings.Fields(string(raw[closing+2:]))
	// fields[0] is field 3 (state), so field N is at index N-3: the parent
	// process is field 4 and the resident set size, in pages, is field 24.
	const ppidIndex, rssIndex = 1, 21
	if len(fields) <= rssIndex {
		return "", 0, 0, false
	}
	parent, err := strconv.Atoi(fields[ppidIndex])
	if err != nil {
		return "", 0, 0, false
	}
	pages, err := strconv.ParseUint(fields[rssIndex], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return comm, parent, pages, true
}

// selfIdentity reports this process's id and comm, or that /proc is unreadable.
func selfIdentity() (pid int, comm string, ok bool) {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, "", false
	}
	comm, _, _, ok = parseStat(raw)
	if !ok {
		return 0, "", false
	}
	return os.Getpid(), comm, true
}
