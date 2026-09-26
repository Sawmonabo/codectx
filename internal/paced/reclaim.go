package paced

import (
	"encoding/binary"
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

	"github.com/Sawmonabo/codectx/internal/fslock"
)

// FreeInterval is the wait between two freed windows. With Window it is the
// rate at which disk space is given back on the host: 8 MiB, a data sync, a
// quarter of a second. It is one rate for the host, not one per charger, not
// one per process and not one per cache -- what the disk underneath does with
// a discard does not depend on how many callers asked.
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
	// stuck holds what the reclaimer has tried and could not give back, with
	// the reason, keyed by path: a queued entry whose removal failed, or a
	// directory it could not list. A record stands until the entry is freed
	// or gone, or the directory is listed, because it is the disclosure an
	// operator reads beside pending_free_bytes, and a run wakes the reclaimer
	// at every removal it queues.
	stuck map[string]string
	// tried is the skip set of this wake: what the worker has already
	// attempted since the last wake, so everything queued behind a failure
	// still goes and nothing is retried in a spin. A wake clears it, because
	// the entry that could not be freed a moment ago may be freeable now.
	tried map[string]bool
	// started is set when the worker goroutine is running.
	started bool
	// paceMu is the one place in the process a window's turn is taken. Every
	// charger holds it across its wait, so this process never hands the host
	// two windows in one interval however many callers are freeing at once:
	// the reclaimer's goroutine, the file system shim shortening a file the
	// engine owns, a publication trimming its staging surface. The rate is a
	// property of what the disk underneath does with a discard, so it is the
	// HOST's, and a pace kept per charger would be N times it.
	paceMu sync.Mutex
	// spent is the bytes freed since the last wait, at most a window, and
	// turns counts the waits taken. Both are guarded by paceMu, because
	// spending the budget is what takes a turn.
	spent int64
	turns int64
	// turnDir is the directory holding this user's one turn file on the host,
	// set by the process (see UseTurnDir). paceRoot is the outermost directory
	// this process has registered, and is where the turn file goes only when
	// no turn directory was set. Both are guarded by mu. turnFile is whichever
	// turn file is open, at turnAt, both guarded by paceMu.
	turnDir  string
	paceRoot string
	turnFile *os.File
	turnAt   string
	// seq names queued entries apart within one process.
	seq atomic.Int64
	// sleep is the wait between windows, replaced by tests with a clock that
	// records instead of sleeping.
	sleep func(time.Duration)
}

// reclaim is the process's one reclaimer. The pace is the host's, so the
// product has exactly one; a test constructs its own with newReclaimer.
var reclaim = newReclaimer()

func newReclaimer() *reclaimer {
	r := &reclaimer{
		sets:     map[string]func() (string, error){},
		resolved: map[string]string{},
		stuck:    map[string]string{},
		tried:    map[string]bool{},
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
func RegisterToFree(dir string, set func() (string, error)) { reclaim.register(dir, set) }

func (r *reclaimer) register(dir string, set func() (string, error)) {
	dir = filepath.Clean(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	// The fallback turn location is the outermost registered directory: a
	// store pools under several directories -- its data directory, the
	// continuation store's, a provider's work directory -- all inside one
	// data directory, so the ancestor of the others is the one two processes
	// over that data directory both arrive at.
	if r.paceRoot == "" || (under(r.paceRoot, dir) && dir != r.paceRoot) {
		r.paceRoot = dir
	}
	if _, ok := r.sets[dir]; ok {
		return
	}
	r.sets[dir] = set
}

// AdoptSet hands the reclaimer a to-free set a pool has just claimed, and is
// the startup collection pass. A set is a directory, so a run that exited or
// died with removals still queued left them named in it; the process that
// claims that pool next announces the set here and the reclaimer frees what is
// already in it at the same pace as everything else. Nothing is freed faster
// for being old.
func AdoptSet(dir, set string) { reclaim.adopt(dir, set) }

func (r *reclaimer) adopt(dir, set string) {
	dir = filepath.Clean(dir)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.resolved[dir] == set {
		return
	}
	r.resolved[dir] = set
	r.wakeLocked()
	r.startLocked()
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
// directory's resolver on first use, and answers the registered directory
// and its set. It reports false when no registered directory contains path.
func (r *reclaimer) setFor(path string) (string, string, bool) {
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
		return "", "", false
	}
	if set, ok := r.resolved[best]; ok {
		r.mu.Unlock()
		return best, set, true
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
		return "", "", false
	}
	r.resolved[best] = set
	// A set resolved for the first time may already hold the removals of a
	// process that died with them queued. Waking the worker is how they are
	// resumed.
	r.wakeLocked()
	r.startLocked()
	return best, set, true
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
func (r *reclaimer) queue(p Purpose, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	served, set, ok := r.setFor(filepath.Clean(abs))
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
	dst := filepath.Join(dir, strconv.FormatInt(r.seq.Add(1), 10)+"."+strconv.Itoa(os.Getpid()))
	if err := os.Rename(abs, dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Removing what is not there is what the caller asked for.
			return true
		}
		return false
	}
	// The set is named again as it is woken: the worker may have found it
	// gone and dropped it between setFor and the rename that recreated it,
	// and an entry in a set the worker does not know of is never freed.
	r.mu.Lock()
	r.resolved[served] = set
	r.wakeLocked()
	r.startLocked()
	r.mu.Unlock()
	return true
}

// remove renames path into the to-free set that serves it, or frees it where
// it lies when no set serves it.
func (r *reclaimer) remove(p Purpose, path string) error {
	if r.queue(p, path) {
		return nil
	}
	return r.freeInPlace(p, path)
}

// freeInPlace gives one path back where it lies, at the pace, for a caller no
// to-free set serves. It is what a standalone tool and the toolchain store --
// neither of which has a pool to rename into, and neither of which is on an
// index run's path -- do instead of queueing.
func (r *reclaimer) freeInPlace(p Purpose, path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer dir.Close()
	return r.freeTree(path, p, dir)
}

func (r *reclaimer) wakeLocked() {
	r.idle = false
	// A wake is new work, and the entry that could not be freed a moment ago
	// may be freeable now -- the process holding it open has exited, the
	// mount is writable again, the directory above it has been made
	// writable. Retrying it here is what the reclaimer does instead of
	// retrying it in a loop: once per wake, never in a spin. What is cleared
	// is the skip set, never the disclosure: a retry that fails records the
	// same reason again, and one that succeeds removes it.
	clear(r.tried)
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
		batch, ok := r.nextEntries()
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
		for _, q := range batch {
			r.freeQueued(q)
		}
	}
}

// freeQueued frees one queued entry and records the outcome.
//
// A failure to free one entry must not stop the reclaimer: the remaining
// entries are other callers' space. It stays named in the set, is recorded
// with its reason so an operator can read what is stuck and why, and is
// passed over until the next wake.
//
// An entry that is gone from the disk is not stuck whatever was returned:
// freeing a file shrinks it before unlinking it, and a shrink that failed is
// still reported although the unlink that followed succeeded.
//
// Gone is exactly one answer -- the entry does not exist -- and every other
// answer is the filesystem refusing to say. A refusal read as "gone" hands the
// entry straight back to the queue, which returns it again at once: nothing is
// recorded, the operator is told nothing is stuck, and the entry is retried in
// a spin that never reaches anything queued behind it. So only not-exist
// passes over; anything else is stuck with the reason the free itself gave.
func (r *reclaimer) freeQueued(q queued) {
	err := r.freeEntry(q.path, q.purpose)
	r.mu.Lock()
	r.tried[q.path] = true
	r.mu.Unlock()
	if err == nil {
		r.freed(q.path)
		return
	}
	if _, statErr := os.Lstat(q.path); errors.Is(statErr, fs.ErrNotExist) {
		r.freed(q.path)
		return
	}
	r.recordStuck(q.path, err)
}

// freed drops what an entry that is gone left behind: its disclosure, because
// an entry no operator can find is not an explanation of anything, and its
// skip, because the path may be minted again.
func (r *reclaimer) freed(path string) {
	r.mu.Lock()
	delete(r.stuck, path)
	delete(r.tried, path)
	r.mu.Unlock()
}

// forgetUnder drops every record of dir and of anything inside it: the
// directory is gone, so nothing in it holds a byte or explains anything.
func (r *reclaimer) forgetUnder(dir string) {
	r.mu.Lock()
	for path := range r.stuck {
		if under(path, dir) {
			delete(r.stuck, path)
		}
	}
	for path := range r.tried {
		if under(path, dir) {
			delete(r.tried, path)
		}
	}
	r.mu.Unlock()
}

// listed clears the record of a directory the reclaimer has just listed: it
// is no longer a directory that cannot be listed.
func (r *reclaimer) listed(dir string) {
	r.mu.Lock()
	delete(r.stuck, dir)
	r.mu.Unlock()
}

// dropSet forgets a to-free set that is no longer on the disk -- its pool was
// removed with the directory it served -- along with every record under it.
// The registered directory keeps its resolver, so a later removal under it
// resolves, and so recreates, its set again. The absence is confirmed under
// the lock a queueing removal names its set under, so a set recreated since
// the listing is kept.
func (r *reclaimer) dropSet(set string) {
	r.mu.Lock()
	if _, err := os.Lstat(set); !errors.Is(err, fs.ErrNotExist) {
		r.mu.Unlock()
		return
	}
	for dir, s := range r.resolved {
		if s == set {
			delete(r.resolved, dir)
		}
	}
	r.mu.Unlock()
	r.forgetUnder(set)
}

// A queued is one removal waiting in a to-free set, with the purpose its
// directory names.
type queued struct {
	path    string
	purpose Purpose
}

// nextEntries names the queued removals of the first purpose directory, in
// the order the sets and their purposes read, that holds one the reclaimer
// has not already tried this wake. It reports false when every set holds
// nothing but entries this wake could not free, which is when the worker has
// nothing left to do and goes to sleep. A whole directory's worth is returned
// at once so that freeing a queue of n entries lists it once, not n times.
//
// A directory that is no longer there holds nothing: a set is dropped with
// every record under it, and a purpose directory's records are cleared. A
// directory that is there and cannot be LISTED is recorded stuck with its
// reason, exactly as an entry it cannot free is, because everything queued
// inside it is space this process is holding and cannot give back. A listing
// that succeeds clears that record.
func (r *reclaimer) nextEntries() ([]queued, bool) {
	r.mu.Lock()
	sets := make([]string, 0, len(r.resolved))
	for _, set := range r.resolved {
		sets = append(sets, set)
	}
	tried := make(map[string]bool, len(r.tried))
	for path := range r.tried {
		tried[path] = true
	}
	r.mu.Unlock()
	sort.Strings(sets)
	for _, set := range sets {
		purposes, err := os.ReadDir(set)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r.dropSet(set)
			} else {
				r.recordStuck(set, err)
			}
			continue
		}
		r.listed(set)
		for _, pe := range purposes {
			if !pe.IsDir() {
				continue
			}
			purposeDir := filepath.Join(set, pe.Name())
			entries, err := os.ReadDir(purposeDir)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					r.forgetUnder(purposeDir)
				} else {
					r.recordStuck(purposeDir, err)
				}
				continue
			}
			r.listed(purposeDir)
			var batch []queued
			for _, e := range entries {
				path := filepath.Join(purposeDir, e.Name())
				if !tried[path] {
					batch = append(batch, queued{path: path, purpose: purposeByName[pe.Name()]})
				}
			}
			if len(batch) > 0 {
				return batch, true
			}
		}
	}
	return nil, false
}

// recordStuck names one thing the reclaimer could not give back and why, so
// that space it is still holding is never left without an explanation beside
// it. A gone entry is never recorded (see freeQueued); everything else is, whether
// it is a queued removal that failed or a directory that could not even be
// listed. The next wake retries it; the record stands until it is freed.
//
// The reason is the filesystem's own, with this process's scratch path taken
// out of it: what the removal is called inside its set is what identifies it
// to an operator, and where this process happens to keep its pool is not
// theirs to read (it is a path under a cache directory, in a report and a log
// line that go to other people).
func (r *reclaimer) recordStuck(path string, err error) {
	r.mu.Lock()
	r.stuck[path] = scrubPath(err.Error(), path)
	r.tried[path] = true
	r.mu.Unlock()
}

// scrubPath takes the absolute part of a to-free path out of a message,
// leaving the purpose and name the entry is known by. The directories above
// the set are what it removes, whichever of the three shapes the path has --
// a set, a purpose directory inside it, or an entry inside that.
func scrubPath(msg, path string) string {
	for _, dir := range []string{
		filepath.Dir(filepath.Dir(path)), filepath.Dir(path), path,
	} {
		if dir == "." || dir == string(filepath.Separator) {
			continue
		}
		msg = strings.ReplaceAll(msg, dir+string(filepath.Separator), "")
		msg = strings.ReplaceAll(msg, dir, entryName(path))
	}
	return msg
}

// freeEntry gives one queued file or tree back to the filesystem at the pace,
// attributing every byte it releases to the purpose the removal named.
func (r *reclaimer) freeEntry(path string, p Purpose) error {
	// The directory the entry itself will leave, which is the one whose
	// commit carries the entry's own removal.
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	return r.freeTree(path, p, parent)
}

// freeTree frees one path, depth first: a directory's children are freed
// before the directory itself, so nothing is unlinked while it still holds
// space. parent is the directory holding path, open, because that is the
// directory whose journal commit carries this removal's freed extents to the
// device -- the wait the pace makes would otherwise be a wait for a free that
// had not left the filesystem yet. Descending opens each directory in turn,
// so a nested file's free is committed by a sync of the directory IT leaves,
// not of the set at the top of the tree.
//
// A tree removal is never all-or-nothing. One child that cannot be freed --
// a device error, a read-only mount, a file another process is executing --
// does not leave the rest of the tree behind; the walk goes on, every child
// that can go goes, and the first failure is reported once the walk is over.
// Leaving the whole tree because of one file is the leak a removal exists to
// prevent.
func (r *reclaimer) freeTree(path string, p Purpose, parent *os.File) error {
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
		here, err := os.Open(path)
		if err != nil {
			return err
		}
		var first error
		for _, e := range entries {
			if err := r.freeTree(filepath.Join(path, e.Name()), p, here); err != nil && first == nil {
				first = err
			}
		}
		here.Close()
		if err := os.Remove(path); err != nil && first == nil {
			first = err
		}
		return first
	}
	if !st.Mode().IsRegular() {
		// A symbolic link, a socket or a device holds no blocks of its own.
		return os.Remove(path)
	}
	return r.freeFile(path, st, p, parent)
}

// freeFile empties one regular file a window at a time and then unlinks it.
// A file that could not be emptied is unlinked all the same -- the shrink is
// best effort, the unlink is not. Every byte it releases is charged to the
// pace, whether it went by truncation or with the unlink: a thousand small
// files freed at once cost the filesystem what one large file of the same
// bytes costs, and the measurement that set FreeInterval counted bytes, not
// calls.
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
// So the unlink is asked of the directory BEFORE the wait: a directory that
// refuses it -- one the process may not write, a read-only mount -- is
// reported at once and charges nothing. Waiting a file's whole length for an
// unlink that cannot happen would repeat that wait at every wake the entry is
// retried, and hold every entry queued behind it for as long.
//
// Attribution goes the other way: the counter is credited only once the name
// is actually gone, so what an operator reads as given back is never bytes
// that are still on the disk.
func (r *reclaimer) freeFile(path string, st fs.FileInfo, p Purpose, dir *os.File) error {
	size := st.Size()
	var shrinkErr error
	switch {
	case !shrinkable(st):
		// Another name reaches this object, so unlinking this one gives
		// nothing back: the object and its blocks stay. There is nothing to
		// pace and nothing to attribute.
		size = 0
	case size > Window:
		size, shrinkErr = r.empty(path, size, p, dir)
	}
	if size > 0 {
		if err := unlinkRefused(filepath.Dir(path)); err != nil {
			return &fs.PathError{Op: "remove", Path: path, Err: err}
		}
	}
	owed := r.charge(size, dir)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		// A refusal the directory's permissions did not foretell. The waits
		// already made are time and are not returnable; the part of this
		// call's bytes still unpaid is, and returning it is what keeps the
		// entry's next retry from paying for it again.
		r.refund(owed)
		return err
	}
	attribute(p, size)
	return shrinkErr
}

// An unpaid is the part of one call's bytes still unpaid when the call
// returned: the bytes it added to spent after the last wait it took, and the
// turn count they were added at.
type unpaid struct {
	bytes int64
	turn  int64
}

// refund gives back the unpaid part of a charge whose bytes were not released
// after all. Only that call's own share goes, and only while no wait has been
// taken since: a wait paid for everything spent before it, other chargers'
// bytes included, so after one there is nothing of this call's left to return
// and subtracting anyway would erase what other chargers still owe.
func (r *reclaimer) refund(c unpaid) {
	if c.bytes <= 0 {
		return
	}
	r.paceMu.Lock()
	defer r.paceMu.Unlock()
	if r.turns == c.turn {
		r.spent -= c.bytes
	}
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
func (r *reclaimer) empty(path string, size int64, p Purpose, dir *os.File) (int64, error) {
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
		r.charge(cur-next, dir)
		cur = next
	}
	return 0, nil
}

// charge spends the bytes just released against the pace and waits whenever a
// window of them has been spent. Freed extents reach the device when the
// filesystem journals the change, so a wait with nothing synced would be a
// wait for nothing: the truncation path has already synced the file's data,
// and for an unlink the directory the entry leaves -- dir, the file's own
// parent, not the set at the top of the tree -- is synced here, which is the
// journal commit that carries the freed extents with it.
//
// The budget is the host's, not one removal's, not one process's and not one
// cache's: every charger waits under paceMu, and every process of this user
// takes its windows in turn through one turn file (see UseTurnDir), so no two
// of them ever hand the disk two windows in one interval.
//
// It reports the part of n still unpaid on return, which is what a caller
// whose bytes turn out not to have been released may refund.
func (r *reclaimer) charge(n int64, dir *os.File) unpaid {
	r.paceMu.Lock()
	defer r.paceMu.Unlock()
	r.spent += n
	for r.spent >= Window {
		r.spent -= Window
		if dir != nil {
			_ = dir.Sync()
		}
		r.takeTurn()
		r.turns++
	}
	return unpaid{bytes: min(n, r.spent), turn: r.turns}
}

// paceFileName is the file whose lock and recorded time are how the processes
// of this user take one window's turn at a time.
const paceFileName = "free-pace.lock"

// UseTurnDir names the directory holding this user's one turn file on the
// host, and is called once, at process start, with the product's directory in
// the user cache. The rate a discard costs is a property of the DEVICE
// underneath, not of a data directory, so the turn cannot be taken per data
// directory: `codectx index --rebuild` writes into a sibling data directory
// while a server serves the original, and two directories taking turns
// separately hand the host twice the measured rate on an ordinary pair of
// commands. One file above every data directory of this user is what makes
// the sum of what this product frees on a host the rate that was measured.
//
// The user cache is the one location every process of this user agrees on
// without being told. Two processes given different user cache directories
// keep a pace each, and so hand the host twice the measured rate between
// them; nothing inside a process can see the other, so that case is written
// down here rather than described as harmless.
func UseTurnDir(dir string) {
	reclaim.mu.Lock()
	reclaim.turnDir = filepath.Clean(dir)
	reclaim.mu.Unlock()
}

// takeTurn waits until the host may be handed another window and records that
// it has been. The record is in the turn file, under an exclusive lock, so the
// turn is taken across processes and not only across this one's chargers: two
// runs free at the pace between them and not at twice it, whether they share
// a data directory or one of them is a --rebuild writing a sibling one. The
// rate the measurement established is what the disk underneath tolerates, and
// a disk does not care how many processes are asking.
//
// The remainder is clamped into one interval. The recorded time is a wall
// clock and comes from another process, so a clock adjustment, a record from
// a machine-image restore or a torn write must cost at most one interval and
// never hang a run.
//
// With no turn file -- a standalone tool that has opened no store, a turn
// directory it cannot write -- there is nothing to share and the wait is the
// interval itself.
func (r *reclaimer) takeTurn() {
	f := r.turn()
	if f == nil {
		r.sleep(FreeInterval)
		return
	}
	if err := fslock.Lock(f); err != nil {
		r.sleep(FreeInterval)
		return
	}
	defer fslock.Unlock(f)
	wait := FreeInterval
	var record [8]byte
	if n, err := f.ReadAt(record[:], 0); err == nil && n == len(record) {
		since := time.Since(time.Unix(0, int64(binary.BigEndian.Uint64(record[:]))))
		wait = min(max(FreeInterval-since, 0), FreeInterval)
	}
	r.sleep(wait)
	binary.BigEndian.PutUint64(record[:], uint64(time.Now().UnixNano()))
	_, _ = f.WriteAt(record[:], 0)
}

// turn is this user's turn file, opened once. It is the one in the turn
// directory the process named; only a process that named none falls back to
// the outermost directory it registered, which shares the pace with the
// processes over that same data directory and no others. It reports nil when
// there is neither, or when the file cannot be opened -- a read-only cache, a
// directory already removed -- and the caller then keeps the pace for itself
// alone. For ONE process that is the same rate; for two it is twice it, which
// is why this is a last resort and not an equivalent.
//
// The caller holds paceMu.
func (r *reclaimer) turn() *os.File {
	r.mu.Lock()
	dir, fallback := r.turnDir, r.paceRoot
	r.mu.Unlock()
	named := dir != ""
	if !named {
		dir = fallback
	}
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, paceFileName)
	if r.turnFile != nil && r.turnAt == path {
		return r.turnFile
	}
	// The named turn directory is the product's own and is made on first
	// use. A registered directory is never made here: a directory a caller
	// may yet remove must not be recreated behind its back.
	if named {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil
	}
	if r.turnFile != nil {
		r.turnFile.Close()
	}
	r.turnFile, r.turnAt = f, path
	return f
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
func Drain() []Stuck { return reclaim.drain() }

func (r *reclaimer) drain() []Stuck {
	r.mu.Lock()
	if r.started {
		for !r.idle {
			r.cond.Wait()
		}
	}
	r.mu.Unlock()
	return r.stuckFrees()
}

// StuckFrees is every queued removal, and every to-free directory, the
// reclaimer has tried and could not give back or list and that is still on
// the disk, with the reason. It is disclosed beside PendingFreeBytes: that
// figure alone says space is waiting, and this says which of it is waiting on
// something that will not resolve itself.
func StuckFrees() []Stuck { return reclaim.stuckFrees() }

func (r *reclaimer) stuckFrees() []Stuck {
	r.mu.Lock()
	out := make([]Stuck, 0, len(r.stuck))
	for path, reason := range r.stuck {
		out = append(out, Stuck{Entry: entryName(path), Reason: reason})
	}
	r.mu.Unlock()
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
func PendingFreeBytes() (int64, error) { return reclaim.pendingFreeBytes() }

func (r *reclaimer) pendingFreeBytes() (int64, error) {
	r.mu.Lock()
	sets := make([]string, 0, len(r.resolved))
	for _, set := range r.resolved {
		sets = append(sets, set)
	}
	r.mu.Unlock()
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
