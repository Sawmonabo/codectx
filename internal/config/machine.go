package config

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/residency"
)

// idleFootprintBytes is what this process holds before it reserves anything:
// the Go runtime, the resolved configuration and the bounded buffers a command
// needs to answer at all. Everything else in the footprint is a reservation
// the configuration states, which is why this is the only part of it that has
// to be observed.
//
// It is this process's own resident set, read once, at its first use, with
// residency.Read. For a loaded configuration that first use is validation
// at load, before the store is opened, so the reading is the idle process and
// does not count again the page caches the reservations below declare. Where
// the platform reports no resident set, UnobservedIdleFootprintBytes stands in
// for the reading.
var idleFootprintBytes = sync.OnceValue(func() int64 {
	if resident := residency.Read().Resident; resident != nil {
		return int64(*resident)
	}
	return UnobservedIdleFootprintBytes
})

// UnobservedIdleFootprintBytes stands in for the idle term where the platform
// reports no resident set for this process. It is not an observation of this
// process: it is the measurement of the idle process recorded in
// docs/adr/ADR-0010-engine-memory.md -- `codectx status` over a freshly
// indexed five-file, 432-byte fixture peaked at 26,584 / 27,052 / 26,732 KiB
// of resident set over three samples ("Maximum resident set size",
// /usr/bin/time -v), so 26.4 MiB at the worst of the three, rounded up to the
// next binary step for run-to-run variation.
const UnobservedIdleFootprintBytes int64 = 32 * (1 << 20)

// BaseFootprint is what this process holds for itself on this machine under
// configuration c: its idle resident set above, plus the reservations it
// makes up front -- one query slot's memory per core, the parsed-graph cache, the
// indexing queue, and every page cache it opens: the store writer
// connection's (storage.writer_cache_kib), every reader connection's
// (storage.reader_cache_kib) in both reader pools of every store handle
// (ReadConnections, PostingConnections, storeHandles), one lexical staging database's per unit a generation
// builds at once (LexicalStageCacheKiB, BuildWorkers), each store handle's
// query tokenizer (TokenizerCacheKiB) and the run ledger's
// (ledger.FootprintBytes).
//
// It is derived from the machine and the configuration rather than compared
// against a figure. How many queries run at once comes from the cores, so a
// host with more cores has a larger base footprint and leaves its children a
// smaller allocation, and no core count can make the shipped defaults
// unresolvable.
func BaseFootprint(c Config) int64 {
	total, err := baseFootprintFor(c)
	if err != nil {
		// Validation refuses a configuration whose base-footprint arithmetic
		// does not fit 64-bit range, so a loaded configuration never reaches
		// this. Saturating is the safe direction for anything else:
		// over-stating the footprint only leaves the children less.
		return math.MaxInt64
	}
	return total
}

// storeHandles is how many store handles this process can hold open at once:
// the writer's, and in the serving composition a second, query-only handle
// beside it. Counting the serving case for every composition over-states the
// others by one handle's pools, which only leaves their children less.
const storeHandles = 2

// ReadConnections is how many connections a store handle's short-read pool
// holds: storage.read_connections when it is set, and otherwise one for each
// goroutine that can read the pool at once. Those are the units a generation
// builds at once (BuildWorkers), each reading its unit state and its
// dependencies' aliases there, and the tool calls that run at once
// (QuerySlots), each issuing its short reads there. The coordinator holds the
// unit count under the provider pool's ceiling on live sinks, which can only
// lower it, so the pool is never smaller than the goroutines that read it.
func ReadConnections(c Config) int {
	if c.Storage.ReadConnections > 0 {
		return c.Storage.ReadConnections
	}
	return BuildWorkers(c) + QuerySlots()
}

// PostingConnections is how many connections a store handle's posting-stream
// pool holds: storage.read_connections when it is set, and otherwise one per
// tool call that runs at once (QuerySlots). Only a query's candidate walk
// holds a posting session, and it holds one for the whole walk, so no unit
// build ever reads this pool.
func PostingConnections(c Config) int {
	if c.Storage.ReadConnections > 0 {
		return c.Storage.ReadConnections
	}
	return QuerySlots()
}

// LexicalStageCacheKiB is the page cache of one building unit's lexical
// staging database, which the store opens per unit that publishes search
// rows. It is the buffer the engine sorts the seal-time read in, so a unit
// whose vocabulary fits it is ordered without a spill file, and it is an
// internal layout figure, not a user limit: no count of terms or documents is
// refused because of it, and a larger unit spills under <data_dir>/tmp. It is
// stated here, beside the footprint that counts it, because the store reads
// its configuration from this package.
const LexicalStageCacheKiB = 16 << 10

// TokenizerCacheKiB is the page cache of a store handle's query tokenizer: one
// in-memory connection that holds a single query text, at most
// model.MaxQueryTextBytes, for the length of one call and nothing between
// calls. An in-memory database cannot spill, so the figure bounds the pages it
// keeps after a call rather than what one call may use; it is stated for the
// same reason LexicalStageCacheKiB is.
const TokenizerCacheKiB = 2 << 10

// BuildWorkers is how many units one generation builds at once:
// index.workers when it is set, and otherwise one per core this process may
// run on. The coordinator also holds the count under the provider pool's
// structural ceiling on live sinks, which can only lower it.
func BuildWorkers(c Config) int {
	if c.Index.Workers > 0 {
		return c.Index.Workers
	}
	return CPUs()
}

// baseFootprintFor is BaseFootprint with the arithmetic's range failure
// carried, so validation and the figure itself come from one implementation
// rather than two that drift.
func baseFootprintFor(c Config) (int64, error) {
	concurrent, err := mulNoOverflow("query slots * resources.query_memory_bytes",
		int64(QuerySlots()), c.Resources.QueryMemoryBytes)
	if err != nil {
		return 0, err
	}
	writerCache, err := mulNoOverflow("storage.writer_cache_kib * 1024",
		int64(c.Storage.WriterCacheKiB), 1<<10)
	if err != nil {
		return 0, err
	}
	handleConnections, err := addNoOverflow("short-read + posting-stream connections",
		int64(ReadConnections(c)), int64(PostingConnections(c)))
	if err != nil {
		return 0, err
	}
	readerConnections, err := mulNoOverflow("store handles * reader connections",
		storeHandles, handleConnections)
	if err != nil {
		return 0, err
	}
	readerCacheBytes, err := mulNoOverflow("storage.reader_cache_kib * 1024",
		int64(c.Storage.ReaderCacheKiB), 1<<10)
	if err != nil {
		return 0, err
	}
	readerCaches, err := mulNoOverflow("reader connections * storage.reader_cache_kib",
		readerConnections, readerCacheBytes)
	if err != nil {
		return 0, err
	}
	stageCaches, err := mulNoOverflow("index.workers (or cores) * lexical staging cache",
		int64(BuildWorkers(c)), LexicalStageCacheKiB<<10)
	if err != nil {
		return 0, err
	}
	return addNoOverflow("base footprint of this process",
		idleFootprintBytes(), concurrent, c.Resources.CacheBytes, c.Index.QueueBytes, writerCache, readerCaches,
		stageCaches, storeHandles*TokenizerCacheKiB<<10, ledger.FootprintBytes)
}

// How much CPU-bound work runs at once is a property of the machine, not a
// number anyone types. Memory-bound work is admitted against the machine's one
// memory allocation; the counts here are the other half of that rule, for work
// whose scarce resource is a core rather than a byte.
//
// None of these ever refuses anything. A structural parse that finds no free
// worker waits for one, and a query that finds no free slot queues: a gate
// derived from the hardware would otherwise turn a busy moment on a small
// machine into a refusal the caller cannot act on.

// CPUs is how many cores this process may run on: the smaller of the
// affinity the process was started with and the CPU quota its control group
// is held to. It is never below one because neither input is --
// runtime.NumCPU reports at least one, and a quota is rounded to at least one
// core (smallestQuota).
//
// Both are read because they are different limits. runtime.NumCPU honours
// affinity, which is what a CPU set restricts; a container CPU limit is
// normally not a set but a bandwidth quota, and a process held to two cores of
// a sixteen-core host by a quota still reads sixteen from affinity alone. It
// would then run sixteen parser workers and reserve sixteen workers' memory on
// a machine that can run two.
//
// A quota is an observation that can be absent, and an absent one is not a
// quota of zero: no control-group filesystem, a group that states no bandwidth
// limit, and a file this process may not read all mean the group states no
// quota, and the affinity count then stands alone.
func CPUs() int {
	n := cpuCount()
	if quota, ok := cpuQuota(); ok && quota < n {
		n = quota
	}
	return n
}

// cpuCount is the machine observation CPUs reads, indirected so a test can
// resolve a configuration for a core count this host does not have.
var cpuCount = runtime.NumCPU

// cpuQuota is the whole cores this process's control group hierarchy allows,
// and false where no group in it states a bandwidth limit. It is indirected
// for the same reason cpuCount is: a test cannot put this host under a quota.
var cpuQuota = observedCPUQuota

// cgroupRoot is where the control-group filesystem is mounted, and
// procCgroupPath is where a process reads the groups it is in. A build on a
// platform that has neither simply finds nothing there, which is the same
// answer as a host that states no limit, so the reading needs no platform
// split of its own. They are variables so a test can state a hierarchy this
// host is not in.
var (
	cgroupRoot     = "/sys/fs/cgroup"
	procCgroupPath = "/proc/self/cgroup"
)

// observedCPUQuota reads the CPU bandwidth limit of every group this process
// is in, unified hierarchy first and then the legacy cpu controller, and
// returns the smallest. Every group in the hierarchy enforces its own limit,
// so a quota stated on an ancestor binds this process just as one stated on
// its own group does -- and the common container and service case states it on
// the ancestor, where a reading of the process's own group alone finds
// nothing.
//
// Fractions round DOWN, never below one core: the count decides how many
// workers run and how much memory they reserve, and rounding a half core up
// would reserve a worker the quota cannot run.
func observedCPUQuota() (int, bool) {
	if cores, ok := unifiedCPUQuota(); ok {
		return cores, true
	}
	return legacyCPUQuota()
}

// unifiedCPUQuota reads cpu.max ("<quota|max> <period>", in microseconds) from
// this process's unified group and each of its ancestors.
func unifiedCPUQuota() (int, bool) {
	path, ok := cgroupPathFor("")
	if !ok {
		return 0, false
	}
	best, found := 0, false
	for _, dir := range cgroupAncestors(filepath.Join(cgroupRoot, path)) {
		fields := strings.Fields(readCgroupFile(filepath.Join(dir, "cpu.max")))
		if len(fields) != 2 || fields[0] == "max" {
			continue
		}
		quota, qerr := strconv.ParseInt(fields[0], 10, 64)
		period, perr := strconv.ParseInt(fields[1], 10, 64)
		if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
			continue
		}
		best, found = smallestQuota(best, found, quota, period)
	}
	return best, found
}

// legacyCPUQuota reads cpu.cfs_quota_us and cpu.cfs_period_us from the legacy
// cpu controller's group and each of its ancestors. A quota of -1 is the
// controller's spelling of "no limit".
func legacyCPUQuota() (int, bool) {
	path, ok := cgroupPathFor("cpu")
	if !ok {
		return 0, false
	}
	best, found := 0, false
	for _, mount := range []string{"cpu,cpuacct", "cpu"} {
		for _, dir := range cgroupAncestors(filepath.Join(cgroupRoot, mount, path)) {
			quota, qerr := strconv.ParseInt(readCgroupFile(filepath.Join(dir, "cpu.cfs_quota_us")), 10, 64)
			period, perr := strconv.ParseInt(readCgroupFile(filepath.Join(dir, "cpu.cfs_period_us")), 10, 64)
			if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
				continue
			}
			best, found = smallestQuota(best, found, quota, period)
		}
	}
	return best, found
}

// smallestQuota folds one group's quota and period into the smallest whole
// core count seen so far.
func smallestQuota(best int, found bool, quota, period int64) (int, bool) {
	cores := int(quota / period)
	if cores < 1 {
		cores = 1
	}
	if !found || cores < best {
		return cores, true
	}
	return best, found
}

// CgroupMemoryHeadroom is how much more memory this process's control groups
// let it take: the limit less the usage of every group it is in, unified
// hierarchy first and then the legacy memory controller, smallest over the
// process's own group and every ancestor. Every group enforces its own limit,
// so a limit on an ancestor binds exactly as one on the process's own group
// does, and a container is normally limited on the ancestor.
//
// false means no group in the hierarchy states a limit -- no control-group
// filesystem, "max", or a file this process may not read -- which is not a
// headroom of zero. A group already at or above its limit is a real reading
// of zero. A group whose limit is readable and whose usage is not is read at
// its whole limit: the headroom cannot be more than that.
func CgroupMemoryHeadroom() (int64, bool) {
	if headroom, ok := unifiedMemoryHeadroom(); ok {
		return headroom, true
	}
	return legacyMemoryHeadroom()
}

// unifiedMemoryHeadroom reads memory.max and memory.current from this
// process's unified group and each of its ancestors.
func unifiedMemoryHeadroom() (int64, bool) {
	path, ok := cgroupPathFor("")
	if !ok {
		return 0, false
	}
	best, found := int64(0), false
	for _, dir := range cgroupAncestors(filepath.Join(cgroupRoot, path)) {
		best, found = smallestHeadroom(best, found,
			readCgroupFile(filepath.Join(dir, "memory.max")),
			readCgroupFile(filepath.Join(dir, "memory.current")))
	}
	return best, found
}

// legacyMemoryHeadroom reads memory.limit_in_bytes and memory.usage_in_bytes
// from the legacy memory controller's group and each of its ancestors.
func legacyMemoryHeadroom() (int64, bool) {
	path, ok := cgroupPathFor("memory")
	if !ok {
		return 0, false
	}
	best, found := int64(0), false
	for _, dir := range cgroupAncestors(filepath.Join(cgroupRoot, "memory", path)) {
		best, found = smallestHeadroom(best, found,
			readCgroupFile(filepath.Join(dir, "memory.limit_in_bytes")),
			readCgroupFile(filepath.Join(dir, "memory.usage_in_bytes")))
	}
	return best, found
}

// legacyUnlimitedMemoryBytes is where the legacy memory controller's spelling
// of "no limit" begins. It writes no word for it: an unlimited group reports
// the largest page-aligned count its counter holds, just under 2^63. No host
// has 2^62 bytes of memory, so a limit at or above it is that spelling and
// not a limit.
const legacyUnlimitedMemoryBytes int64 = 1 << 62

// smallestHeadroom folds one group's limit and usage into the smallest
// headroom seen so far. A limit that does not parse (the unified "max"), is
// not positive or is the legacy unlimited spelling states nothing.
func smallestHeadroom(best int64, found bool, limitText, usageText string) (int64, bool) {
	limit, err := strconv.ParseInt(limitText, 10, 64)
	if err != nil || limit <= 0 || limit >= legacyUnlimitedMemoryBytes {
		return best, found
	}
	headroom := limit
	if usage, uerr := strconv.ParseInt(usageText, 10, 64); uerr == nil && usage >= 0 {
		headroom = max(limit-usage, 0)
	}
	if !found || headroom < best {
		return headroom, true
	}
	return best, found
}

// cgroupPathFor is the group path /proc/self/cgroup states for a controller:
// the unified line ("0::/path") when controller is empty, otherwise the legacy
// line whose comma-separated controller list holds it.
func cgroupPathFor(controller string) (string, bool) {
	for _, line := range strings.Split(readCgroupFile(procCgroupPath), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		if controller == "" {
			if parts[1] == "" {
				return parts[2], true
			}
			continue
		}
		if slices.Contains(strings.Split(parts[1], ","), controller) {
			return parts[2], true
		}
	}
	return "", false
}

// cgroupAncestors is dir and every directory above it up to the mount root,
// so a limit stated anywhere in the hierarchy is read. The walk is bounded by
// the path's own depth, which the kernel bounds.
func cgroupAncestors(dir string) []string {
	clean := filepath.Clean(dir)
	out := []string{clean}
	for {
		parent := filepath.Dir(clean)
		if parent == clean || !strings.HasPrefix(parent, cgroupRoot) {
			return out
		}
		clean = parent
		out = append(out, clean)
	}
}

// readCgroupFile is one small, optional observation file. Absent, unreadable
// and empty are all "this states nothing", which is why the error is not
// carried: there is nothing an operator could do about a control-group file
// that is not there, and a failure here is not a failure of the run.
func readCgroupFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ParserWorkers is how many structural-parse workers run at once: one per
// core. A worker is a subprocess that parses one file at a time and spends
// essentially all of its time computing, so more than one per core only adds
// context switching, and fewer leaves the machine idle on the phase that
// dominates a cold index. The kernel is left to share the cores between them
// and the rest of the machine rather than a core being held back for it: a
// reserved core is idle in the common case, where nothing else wants it.
func ParserWorkers() int { return CPUs() }

// QuerySlots is how many tool calls run at once: one per core. A query is
// bounded work over the store rather than a subprocess, so the slot count is
// what keeps the answers from sharing one core between them; a call that finds
// no free slot waits.
func QuerySlots() int { return CPUs() }

// GraphSlots is how many of those calls may be graph traversals, half the
// cores and never below one. A traversal holds a query slot as well as this
// one and does far more work per slot than a lookup, so letting every core run
// one would leave nothing answering the cheap calls an agent interleaves with
// them. A traversal that finds no free slot waits.
func GraphSlots() int { return max(CPUs()/2, 1) }
