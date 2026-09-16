package paced

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FreeInterval is the wait between two freed windows. With Window it is the
// rate at which this process gives disk space back: 8 MiB, a data sync, a
// quarter of a second.
//
// It is measured, not chosen. On a machine whose root filesystem discards
// freed blocks and whose disk is a sparse image on its host, freeing 5 GB in
// one burst left the host owing work it never reported: every disk request in
// flight about a minute later waited 64 seconds, and nothing inside the
// machine could observe or wait for it. The same 5 GB freed a window at a
// time with a data sync and this wait between windows -- 152 seconds, the
// device counting 21 GB of discards at a peak of 464 MB in one second --
// caused no wait longer than 32 milliseconds in the four minutes after. The
// host tolerates any amount freed at a pace and hangs on a burst.
//
// It is a constant of this package, not a setting: the rate is a property of
// what the disk underneath does with a discard, and an operator asked to tune
// it would have nothing to tune it against.
const FreeInterval = 250 * time.Millisecond

// toFreeDirName is the set of things awaiting freeing, inside the directory a
// caller registers.
const toFreeDirName = "to-free"

// unlabelled holds the queued removals no call site named. Their bytes are
// counted by Steps and by PendingFreeBytes, never by FreedByPurpose, which
// reports only the removals the product labels.
const unlabelled = "unlabelled"

// purposeByName recovers the Purpose a queued entry was removed for after a
// crash: the entry's parent directory is the only record of it that outlives
// the process that queued it.
var purposeByName = map[string]Purpose{
	string(AnalyzerOutput):    AnalyzerOutput,
	string(Materialization):   Materialization,
	string(LeaseReclamation):  LeaseReclamation,
	string(ScratchCollection): ScratchCollection,
}

// A reclaimer is the one place in the process that gives disk space back. A
// removal anywhere in the product renames its file or tree into a to-free set
// -- a rename frees nothing -- and returns; this frees what is in the sets at
// FreeInterval per Window, on its own goroutine, so no caller ever waits for
// the disk to take space back.
//
// The sets are on the disk, so they outlive the process: a run that crashes
// or exits with removals still queued leaves them named, and the next process
// to register the same directory resumes freeing them at the same pace.
// Nothing is ever freed faster because it is old.
type reclaimer struct {
	mu   sync.Mutex
	cond *sync.Cond
	// sets maps a registered directory to the function that resolves its
	// to-free set, called once, the first time something under that
	// directory is removed.
	sets map[string]func() (string, error)
	// resolved is the to-free set of each registered directory, once its
	// resolver has run.
	resolved map[string]string
	// idle is set when the worker has looked at every set and found nothing
	// it can free. Drain waits for it.
	idle bool
	// stuck holds the queued entries the reclaimer has tried and failed to
	// free, with the reason, keyed by the entry's path. An entry here is
	// passed over so that everything queued behind it still goes, and it is
	// dropped -- and so retried -- at the next wake. It is what keeps a
	// removal nothing can make from stopping every other removal in the
	// process, and from spending a core retrying itself.
	stuck map[string]string
	// started is set when the worker goroutine is running.
	started bool
	// spentMu guards spent, which the reclaimer's goroutine and any caller
	// freeing in place both spend.
	spentMu sync.Mutex
	// spent is the bytes freed since the last wait, at most a window.
	spent int64
	// seq names queued entries apart within one process.
	seq atomic.Int64
	// sleep is the wait between windows, replaced by tests with a clock that
	// records instead of sleeping.
	sleep func(time.Duration)
}

var reclaim = newReclaimer()

func newReclaimer() *reclaimer {
	r := &reclaimer{
		sets:     map[string]func() (string, error){},
		resolved: map[string]string{},
		stuck:    map[string]string{},
		sleep:    time.Sleep,
	}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// RegisterToFree names a directory whose removals are freed off the caller's
// path, and the function that resolves the directory holding that set. The
// resolver runs at most once, at the first removal under dir, and nothing
// touches the disk before that: a directory a caller may yet remove must not
// grow a pool behind its back. It is separate from dir because the set belongs
// to whichever pool has claimed exclusive use of it, and claiming is what the
// resolver does.
//
// A removal of a path under no registered directory is freed where it lies, at
// the same pace: the pace is the protection, and a caller outside any
// registered tree -- the toolchain store, a standalone tool -- has no set to
// rename into and no filesystem guarantee that a rename to one would work.
func RegisterToFree(dir string, set func() (string, error)) {
	dir = filepath.Clean(dir)
	reclaim.mu.Lock()
	defer reclaim.mu.Unlock()
	if _, ok := reclaim.sets[dir]; ok {
		return
	}
	reclaim.sets[dir] = set
}

// AdoptSet hands the reclaimer a to-free set a pool has just claimed, and is
// the startup collection pass. A set is a directory, so a run that exited or
// died with removals still queued left them named in it; the process that
// claims that pool next announces the set here and the reclaimer frees what is
// already in it at the same pace as everything else. Nothing is freed faster
// for being old.
func AdoptSet(dir, set string) {
	dir = filepath.Clean(dir)
	reclaim.mu.Lock()
	defer reclaim.mu.Unlock()
	if reclaim.resolved[dir] == set {
		return
	}
	reclaim.resolved[dir] = set
	reclaim.wakeLocked()
	reclaim.startLocked()
}

// ToFreeDir is the to-free set inside a registered directory. A pool that has
// claimed exclusive use of a directory builds its resolver from it.
func ToFreeDir(dir string) string { return filepath.Join(dir, toFreeDirName) }

// IsToFreeDir reports whether name is the to-free set's own directory name.
// Anything that enumerates a directory a pool serves uses it to keep the
// space awaiting freeing out of the space the pool is holding: the two are
// reported separately, and summing one into the other would count it twice.
func IsToFreeDir(name string) bool { return name == toFreeDirName }

// setFor resolves the to-free set that serves path, running the registered
// directory's resolver on first use. It reports false when no registered
// directory contains path.
func (r *reclaimer) setFor(path string) (string, bool) {
	r.mu.Lock()
	best := ""
	for dir := range r.sets {
		// A strict ancestor only: a set inside the path being removed would
		// be renamed into itself, and resolving it would build a pool inside
		// a tree that is on its way out.
		if under(path, dir) && dir != path && len(dir) > len(best) {
			best = dir
		}
	}
	if best == "" {
		r.mu.Unlock()
		return "", false
	}
	if set, ok := r.resolved[best]; ok {
		r.mu.Unlock()
		return set, true
	}
	resolver := r.sets[best]
	r.mu.Unlock()
	// Resolving claims the pool, which takes that pool's own lock, so it runs
	// outside this one.
	set, err := resolver()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// A pool that cannot be claimed cannot hold a queued removal; the
		// removal falls back to freeing where it lies, at the same pace.
		return "", false
	}
	r.resolved[best] = set
	// A set resolved for the first time may already hold the removals of a
	// process that died with them queued. Waking the worker is how they are
	// resumed.
	r.wakeLocked()
	r.startLocked()
	return set, true
}

// under reports whether path is dir or lies inside it.
func under(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// queue renames path into the to-free set that serves it and returns true.
// The rename frees nothing and takes the time of a directory entry, so the
// caller goes on with its work while the reclaimer gives the space back. It
// reports false when there is no set to rename into, or when the rename
// cannot be made -- a different filesystem, most often -- and the caller
// frees the path itself.
func queue(p Purpose, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	set, ok := reclaim.setFor(filepath.Clean(abs))
	if !ok {
		return false
	}
	name := unlabelled
	if p != "" {
		name = string(p)
	}
	dir := filepath.Join(set, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	dst := filepath.Join(dir, strconv.FormatInt(reclaim.seq.Add(1), 10)+"."+strconv.Itoa(os.Getpid()))
	if err := os.Rename(abs, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removing what is not there is what the caller asked for.
			return true
		}
		return false
	}
	reclaim.wake()
	return true
}

// freeInPlace gives one path back where it lies, at the pace, for a caller no
// to-free set serves. It is what a standalone tool and the toolchain store --
// neither of which has a pool to rename into, and neither of which is on an
// index run's path -- do instead of queueing.
func freeInPlace(p Purpose, path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	return reclaim.freeTree(path, p, dir)
}

// wake tells the worker there is something to free.
func (r *reclaimer) wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakeLocked()
	r.startLocked()
}

func (r *reclaimer) wakeLocked() {
	r.idle = false
	// A wake is new work, and the entry that could not be freed a moment ago
	// may be freeable now -- the process holding it open has exited, the
	// mount is writable again, the directory above it has been made
	// writable. Retrying it here is what the reclaimer does instead of
	// retrying it in a loop: once per wake, never in a spin.
	clear(r.stuck)
	r.cond.Broadcast()
}

// startLocked starts the worker the first time a set is used, so a process
// that never removes anything never grows a goroutine.
func (r *reclaimer) startLocked() {
	if r.started {
		return
	}
	r.started = true
	go r.work()
}

// work frees one queued entry at a time, for the life of the process, and
// sleeps between the ones that empty a window. There is one of it: the pace
// is the process's, so two entries never free two windows at once.
func (r *reclaimer) work() {
	for {
		entry, purpose, ok := r.nextEntry()
		if !ok {
			r.mu.Lock()
			r.idle = true
			r.cond.Broadcast()
			for r.idle {
				r.cond.Wait()
			}
			r.mu.Unlock()
			continue
		}
		// A failure to free one entry must not stop the reclaimer: the
		// remaining entries are other callers' space. It stays named in the
		// set, is recorded with its reason so an operator can read what is
		// stuck and why, and is passed over until the next wake.
		if err := r.freeEntry(entry, purpose); err != nil {
			r.mu.Lock()
			r.stuck[entry] = err.Error()
			r.mu.Unlock()
		}
	}
}

// nextEntry names one queued removal the reclaimer has not already failed on,
// in the order the sets and their purposes read. It reports false when every
// set holds nothing but entries this pass could not free, which is when the
// worker has nothing left to do and goes to sleep.
func (r *reclaimer) nextEntry() (string, Purpose, bool) {
	r.mu.Lock()
	sets := make([]string, 0, len(r.resolved))
	for _, set := range r.resolved {
		sets = append(sets, set)
	}
	stuck := make(map[string]bool, len(r.stuck))
	for path := range r.stuck {
		stuck[path] = true
	}
	r.mu.Unlock()
	sort.Strings(sets)
	for _, set := range sets {
		purposes, err := os.ReadDir(set)
		if err != nil {
			continue
		}
		for _, pe := range purposes {
			if !pe.IsDir() {
				continue
			}
			entries, err := os.ReadDir(filepath.Join(set, pe.Name()))
			if err != nil {
				continue
			}
			for _, e := range entries {
				path := filepath.Join(set, pe.Name(), e.Name())
				if stuck[path] {
					continue
				}
				return path, purposeByName[pe.Name()], true
			}
		}
	}
	return "", "", false
}

// freeEntry gives one queued file or tree back to the filesystem at the pace,
// attributing every byte it releases to the purpose the removal named.
func (r *reclaimer) freeEntry(path string, p Purpose) error {
	set, err := os.Open(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return err
	}
	defer set.Close()
	return r.freeTree(path, p, set)
}

// freeTree frees one path, depth first: a directory's children are freed
// before the directory itself, so nothing is unlinked while it still holds
// space.
//
// A tree removal is never all-or-nothing. One child that cannot be freed --
// a device error, a read-only mount, a file another process is executing --
// does not leave the rest of the tree behind; the walk goes on, every child
// that can go goes, and the first failure is reported once the walk is over.
// Leaving the whole tree because of one file is the leak a removal exists to
// prevent.
func (r *reclaimer) freeTree(path string, p Purpose, set *os.File) error {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		var first error
		for _, e := range entries {
			if err := r.freeTree(filepath.Join(path, e.Name()), p, set); err != nil && first == nil {
				first = err
			}
		}
		if err := os.Remove(path); err != nil && first == nil {
			first = err
		}
		return first
	}
	if !st.Mode().IsRegular() {
		// A symbolic link, a socket or a device holds no blocks of its own.
		return os.Remove(path)
	}
	return r.freeFile(path, st, p, set)
}

// freeFile empties one regular file a window at a time and then unlinks it.
// A file that could not be emptied is unlinked all the same -- the shrink is
// best effort, the unlink is not -- and its bytes are charged to the pace
// whichever of the two released them.
// Every byte it releases is charged to the pace, whether it went by
// truncation or with the unlink: a thousand small files freed at once cost
// the filesystem what one large file of the same bytes costs, and the
// measurement that set FreeInterval counted bytes, not calls.
//
// The bytes an unlink will release are charged BEFORE the unlink, never
// after. Emptying a file needs write on the file and unlinking it needs write
// only on the directory, so a file the process may remove but may not
// truncate reaches the unlink at its full length -- every published blob is
// one of those, being made read-only at publication, and nothing bounds a
// blob's size. Charging after the unlink would hand the host that whole
// length in one act and wait for it afterwards, which is the burst the pace
// exists to prevent: the wait has to come first, so the length is given back
// over its own size's worth of windows and the unlink is the last thing that
// happens.
//
// Attribution goes the other way: the counter is credited only once the name
// is actually gone, so what an operator reads as given back is never bytes
// that are still on the disk.
func (r *reclaimer) freeFile(path string, st fs.FileInfo, p Purpose, set *os.File) error {
	size := st.Size()
	var shrinkErr error
	switch {
	case !shrinkable(st):
		// Another name reaches this object, so unlinking this one gives
		// nothing back: the object and its blocks stay. There is nothing to
		// pace and nothing to attribute.
		size = 0
	case size > Window:
		size, shrinkErr = r.empty(path, size, p, set)
	}
	r.charge(size, set)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	attribute(p, size)
	return shrinkErr
}

// empty truncates one regular file towards zero a window at a time, syncing
// its data and charging the pace after each window, and reports the length
// the unlink that follows will still have to give back. A file the process
// may unlink but may not truncate is left whole, at its full length, for the
// unlink to free: pacing is how a removal is performed, never whether it is
// allowed, and its full length is what the unlink then owes the pace.
//
// A file another name still reaches is left whole too, and owes nothing: the
// object outlives this name, so the unlink gives no blocks back and a wait
// for them would be a wait for nothing. See shrinkable.
func (r *reclaimer) empty(path string, size int64, p Purpose, set *os.File) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return size, nil
	default:
		return size, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return size, err
	}
	if !shrinkable(st) {
		return 0, nil
	}
	cur := size
	for cur > 0 {
		next := max(cur-Window, 0)
		if err := f.Truncate(next); err != nil {
			return cur, err
		}
		if err := syncData(f); err != nil {
			return cur, err
		}
		steps.Add(1)
		attribute(p, cur-next)
		r.charge(cur-next, set)
		cur = next
	}
	return 0, nil
}

// charge spends the bytes just released against the pace and waits whenever a
// window of them has been spent. Freed extents reach the device when the
// filesystem journals the change, so a wait with nothing synced would be a
// wait for nothing: the truncation path has already synced the file's data,
// and for an unlink the directory the entry left is synced here, which is the
// journal commit that carries the freed extents with it.
//
// The budget is the process's, not one removal's: a caller freeing in place
// and the reclaimer's own goroutine spend the same window, so two of them
// never hand the disk two windows at once.
func (r *reclaimer) charge(n int64, set *os.File) {
	r.spentMu.Lock()
	r.spent += n
	waits := r.spent / Window
	r.spent -= waits * Window
	r.spentMu.Unlock()
	for ; waits > 0; waits-- {
		if set != nil {
			_ = set.Sync()
		}
		r.sleep(FreeInterval)
	}
}

// A Stuck is one queued removal the reclaimer tried to make and could not,
// and the reason it could not. The space it holds is still counted by
// PendingFreeBytes, so a figure that stops going down is never left without
// an explanation beside it.
type Stuck struct {
	// Entry names the queued removal inside its to-free set: the purpose it
	// was removed for and the name the rename gave it. The set's own path is
	// left off, because what an operator needs is which removal is stuck, not
	// where this process happens to keep its scratch.
	Entry string
	// Reason is what the filesystem said.
	Reason string
}

// Drain frees everything queued and returns when nothing is left that can be
// freed, reporting what could not be. It is what an operator's request to
// give the space back waits on; nothing on a run's path calls it.
//
// It returns rather than waiting for the impossible: a queued removal the
// filesystem refuses -- a directory the process may not write, a file a
// device error will not release -- would otherwise hold the request open for
// the life of the process while everything behind it went unfreed.
func Drain() []Stuck {
	reclaim.mu.Lock()
	if reclaim.started {
		for !reclaim.idle {
			reclaim.cond.Wait()
		}
	}
	reclaim.mu.Unlock()
	return StuckFrees()
}

// StuckFrees is every queued removal the reclaimer has tried and failed to
// make since its last wake, with the reason. It is disclosed beside
// PendingFreeBytes: that figure alone says space is waiting, and this says
// which of it is waiting on something that will not resolve itself.
func StuckFrees() []Stuck {
	reclaim.mu.Lock()
	out := make([]Stuck, 0, len(reclaim.stuck))
	for path, reason := range reclaim.stuck {
		out = append(out, Stuck{Entry: entryName(path), Reason: reason})
	}
	reclaim.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Entry < out[j].Entry })
	return out
}

// entryName is a queued entry's purpose directory and name, which is all of
// its path that means anything outside this process.
func entryName(path string) string {
	return filepath.Join(filepath.Base(filepath.Dir(path)), filepath.Base(path))
}

// PendingFreeBytes is the disk held by everything renamed into a to-free set
// and not yet given back: space this process has finished with, still
// occupied because giving it back faster than the pace is what stalls the
// host. It is reported beside the space the pools hold, and never inside it.
//
// A set that cannot be read leaves the figure absent rather than short, so
// the number never reads as "less is pending than there is".
func PendingFreeBytes() (int64, error) {
	reclaim.mu.Lock()
	sets := make([]string, 0, len(reclaim.resolved))
	for _, set := range reclaim.resolved {
		sets = append(sets, set)
	}
	reclaim.mu.Unlock()
	var total int64
	for _, set := range sets {
		n, err := TreeBytes(set)
		if err != nil {
			return 0, fmt.Errorf("the space awaiting freeing under %s: %w", filepath.Base(set), err)
		}
		total += n
	}
	return total, nil
}

// TreeBytes is the length of every regular file under dir, excluding any
// to-free set inside it. A directory that is not there holds nothing.
func TreeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() && path != dir && IsToFreeDir(d.Name()) {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
