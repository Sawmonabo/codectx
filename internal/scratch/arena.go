// Package scratch is the pool of working files a store writes for its own
// later reading: sort runs, staging databases, content-store temporaries,
// materialized trees, cursor and traversal state.
//
// Freeing disk space is the expensive act. On a filesystem that discards
// freed blocks under a sparse virtual disk, releasing gigabytes stalls every
// other writer on the machine for about a minute, a minute after the free,
// and nothing inside the machine can observe or wait for that work. A run
// that creates its working files and removes them when it is done pays that
// cost once per surface; a run that takes the same files again pays it never.
//
// So a scratch file is taken from the arena and given back to it. Take hands
// out an existing file of the asked-for purpose opened for overwrite at
// offset zero, with no truncation, or creates one; Release returns it for the
// next taker. A file keeps whatever length its largest tenant gave it for as
// long as the arena exists, and the arena is emptied only when an operator
// asks for it.
//
// Because nothing truncates, the file's length is not the data's length: a
// tenant that wrote a megabyte into a file whose previous tenant wrote ten
// will read nine megabytes of the previous tenant's bytes after its own if
// it reads to end of file. Every tenant therefore carries its own logical
// length — a header, a record count, a byte count it recorded when it wrote —
// and never learns it from os.Stat, from Seek to the end, or from reading
// until EOF. A tenant that cannot do that must not use the arena.
//
// # Two processes, one data directory
//
// A pool is per process, because a lease is an object in one process's heap
// and means nothing to another. Some purposes are written from paths several
// processes can run at once. SortRun and PathSearch are both taken on the
// query path, where any number of readers may serve pages from one data
// directory at the same time, and SortRun is taken on the index path too,
// where the workspace's single writer takes it. ContentTemp and LexicalStage
// are the index path's alone, which one writer holds at a time. Two processes
// sharing a pool would each take slot zero and each read the other's bytes as
// its own, so the claim below applies to every purpose rather than to the two
// that need it.
//
// So the pool lives under a numbered INSTANCE directory that the process
// claims with an exclusive lock held for as long as it runs, and no two live
// processes ever share one. A process that exits or dies leaves its instance
// unlocked, and the next process to start claims that same instance and takes
// over its files: an abandoned pool is inherited, never accumulated and never
// swept.
package scratch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"

	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// A Purpose names a family of interchangeable scratch surfaces. Files of one
// purpose are pooled together, so a purpose groups tenants whose sizes and
// lifetimes are alike: every sort run is one purpose, every staging database
// another. The name becomes a directory under the arena's instance, so it is
// a plain lowercase identifier.
type Purpose string

// The purposes the product takes from an arena. They live here rather than
// beside their tenants so the set of surfaces a store can hold is readable
// in one place.
const (
	// SortRun is one run file of an external sort, and its merged output.
	SortRun Purpose = "sort-run"
	// LexicalStage is a lexical seal's staging database, one per seal slot.
	LexicalStage Purpose = "lexical-stage"
	// ImportSpool is one index import's native-symbol map and occurrence
	// spool, in a database.
	ImportSpool Purpose = "import-spool"
	// PathSearch is one shortest-path search's external-memory state: the
	// settled set, the parent edges and the cost buckets, in a database.
	PathSearch Purpose = "path-search"
	// ContentTemp is the file a content-addressed store streams one blob
	// into while it hashes it, before it knows whether the store already
	// holds that content.
	ContentTemp Purpose = "content-temp"
)

// dirName is the arena's own directory under the directory it serves.
const dirName = "scratch"

// Dir is where the arena of the given directory keeps its surfaces. Anything
// that enumerates a served directory -- a sweep, a budget, a test counting
// what a run left behind -- uses it to tell the pool apart from the state it
// is looking for: the pool is not a leftover, and emptying it is an
// operator's decision.
func Dir(served string) string { return filepath.Join(served, dirName) }

// lockName is the file inside an instance directory whose exclusive lock is
// the claim on that instance.
const lockName = "owner.lock"

// An Arena is the pool of scratch surfaces under one directory. One exists
// per directory: every part of the store that writes a working file under
// that directory takes it from there, so the pool is shared by everything
// rooted at it without a handle threaded through every constructor.
//
// An Arena is safe for concurrent use. Take never hands the same surface to
// two callers within the process, and the instance claim keeps two processes
// out of one pool.
type Arena struct {
	root string

	mu       sync.Mutex
	instance string               // the claimed instance directory, once claimed
	lock     *os.File             // the open, locked claim file; closed by Empty only
	free     map[Purpose][]string // paths available to take, per purpose
	held     map[string]bool      // paths currently leased
	next     map[Purpose]int      // the next ordinal to create, per purpose
	swept    map[Purpose]bool     // purposes whose directory has been read
}

// arenas is the process's arena per served directory.
var (
	arenasMu sync.Mutex
	arenas   = map[string]*Arena{}
)

// All is every arena this process has opened, in no particular order. A
// store's surfaces are pooled under more than one directory -- the data
// directory, the continuation store's, a provider's work directory -- so the
// disk a run is holding rather than freeing is the sum over all of them, and
// disclosing one of them would understate it.
func All() []*Arena {
	arenasMu.Lock()
	defer arenasMu.Unlock()
	out := make([]*Arena, 0, len(arenas))
	for _, a := range arenas {
		out = append(out, a)
	}
	return out
}

// For returns the arena of the given directory, creating it on first use.
// Callers that already hold the directory a surface belongs under — every
// producer of scratch in the product already takes one — reach the pool
// through it with no new plumbing. Nothing touches the disk until the first
// Take, so For cannot fail.
func For(dir string) *Arena {
	root := Dir(dir)
	arenasMu.Lock()
	defer arenasMu.Unlock()
	if a := arenas[root]; a != nil {
		return a
	}
	a := &Arena{
		root:  root,
		free:  map[Purpose][]string{},
		held:  map[string]bool{},
		next:  map[Purpose]int{},
		swept: map[Purpose]bool{},
	}
	arenas[root] = a
	return a
}

// Root is the arena's directory: the parent of every instance.
func (a *Arena) Root() string { return a.root }

// A Lease is one scratch surface taken from an arena. It goes back to the
// pool with Release, which never removes it from the disk, or leaves the pool
// with Discard when the tenant has moved the file somewhere else.
type Lease struct {
	arena   *Arena
	path    string
	purpose Purpose

	mu   sync.Mutex
	file *os.File
}

// Path is the leased file's path. The file exists when the lease is handed
// out.
func (l *Lease) Path() string { return l.path }

// File is the leased file opened for reading and writing, positioned at
// offset zero, with nothing truncated. The same *os.File is returned on
// every call for the life of the lease; Release and Discard close it.
//
// The file's length is the previous tenant's high-water mark, not this
// tenant's data length. See the package comment.
func (l *Lease) File() (*os.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		if _, err := l.file.Seek(0, 0); err != nil {
			return nil, err
		}
		return l.file, nil
	}
	f, err := os.OpenFile(l.path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	l.file = f
	return f, nil
}

// close shuts the open file, if any, and answers the arena the lease was
// taken from, or nil when the lease has already ended.
func (l *Lease) close() *Arena {
	l.mu.Lock()
	f := l.file
	l.file = nil
	l.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
	a := l.arena
	l.arena = nil
	return a
}

// Release gives the surface back to the arena for the next taker. The file
// stays on the disk at its high-water length: releasing frees nothing, which
// is the whole point of the arena. Releasing twice is a no-op.
func (l *Lease) Release() {
	a := l.close()
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.held[l.path] {
		return
	}
	delete(a.held, l.path)
	a.free[l.purpose] = append(a.free[l.purpose], l.path)
}

// Discard ends the lease of a surface that has LEFT the pool: the tenant
// renamed the file somewhere else, so there is nothing at this slot to hand
// the next taker and the arena forgets it. It is the hand-off a sort makes
// when it moves its runs into a continuation's state directory.
//
// It is not a way to delete a surface. A tenant that is finished with its
// bytes releases; only a tenant that moved the file discards.
func (l *Lease) Discard() {
	a := l.close()
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.held, l.path)
}

// Take hands out a surface of the given purpose: one the arena already holds
// and nobody is using, or a new one when the pool is empty. The surface is an
// existing file, never truncated, whose bytes past this tenant's writes are
// the previous tenant's.
func (a *Arena) Take(p Purpose) (*Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.claimLocked(); err != nil {
		return nil, err
	}
	if err := a.sweepLocked(p); err != nil {
		return nil, err
	}
	if n := len(a.free[p]); n > 0 {
		path := a.free[p][n-1]
		a.free[p] = a.free[p][:n-1]
		a.held[path] = true
		return &Lease{arena: a, path: path, purpose: p}, nil
	}
	path, err := a.createLocked(p)
	if err != nil {
		return nil, err
	}
	a.held[path] = true
	return &Lease{arena: a, path: path, purpose: p}, nil
}

// TakeFile is Take followed by File: the common case of a byte file taken to
// be overwritten from offset zero. On an error opening the file the lease is
// released, so a failed take holds nothing.
func (a *Arena) TakeFile(p Purpose) (*Lease, *os.File, error) {
	l, err := a.Take(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := l.File()
	if err != nil {
		l.Release()
		return nil, nil, err
	}
	return l, f, nil
}

// claimLocked claims this process's instance directory, the first time the
// arena is used. It takes the lowest-numbered instance whose lock is free,
// which is how a process started after a crash inherits the crashed
// process's files instead of leaving them for an operator to notice.
func (a *Arena) claimLocked() error {
	if a.instance != "" {
		return nil
	}
	for n := 0; ; n++ {
		dir := filepath.Join(a.root, strconv.Itoa(n))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		held, err := fslock.TryLock(f)
		if err != nil {
			f.Close()
			return err
		}
		if !held {
			// A live process owns this instance and its files are its own.
			f.Close()
			continue
		}
		a.instance, a.lock = dir, f
		return nil
	}
}

// sweepLocked reads a purpose's directory once, so surfaces left in this
// instance by the process that held it before — an earlier run of the store,
// or one that crashed — are taken again instead of created beside them.
// Without it the instance would grow a second pool every time it was claimed.
func (a *Arena) sweepLocked(p Purpose) error {
	if a.swept[p] {
		return nil
	}
	a.swept[p] = true
	entries, err := os.ReadDir(filepath.Join(a.instance, string(p)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 0 || e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		if n >= a.next[p] {
			a.next[p] = n + 1
		}
	}
	sort.Strings(names)
	for _, n := range names {
		a.free[p] = append(a.free[p], filepath.Join(a.instance, string(p), n))
	}
	return nil
}

// createLocked makes one new surface of a purpose.
func (a *Arena) createLocked(p Purpose) (string, error) {
	dir := filepath.Join(a.instance, string(p))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for {
		n := a.next[p]
		a.next[p] = n + 1
		path := filepath.Join(dir, strconv.Itoa(n))
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
}

// Bytes is the disk this arena's instances hold: every surface, taken or
// free, at its current length, including the instances of processes that are
// no longer running. It is what the resources block discloses as the space a
// run keeps instead of freeing.
func (a *Arena) Bytes() (int64, error) { return dirBytes(a.root) }

// ErrInUse refuses to empty an arena while something holds a surface of it.
var ErrInUse = errors.New("scratch arena in use")

// Empty removes every surface this process's instance holds and every
// instance no live process has claimed, one window at a time, and forgets
// them. It is the only place the arena's disk is given back, and it runs only
// when an operator asks for it: no timer, no threshold, no setting. It
// returns the bytes it freed.
//
// It refuses while any lease of this arena is outstanding, because removing a
// surface a tenant is writing would corrupt that tenant's work, and it leaves
// alone the instance of any other process that is still running.
func (a *Arena) Empty() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.held); n > 0 {
		return 0, fmt.Errorf("%w: %d surfaces are leased", ErrInUse, n)
	}
	entries, err := os.ReadDir(a.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var freed int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(a.root, e.Name())
		if dir != a.instance {
			free, err := a.emptyIdleLocked(dir)
			if err != nil {
				return freed, err
			}
			freed += free
			continue
		}
		n, err := dirBytes(dir)
		if err != nil {
			return freed, err
		}
		// This instance's claim stays held: the process still owns it and
		// will take surfaces again. Only its pooled files go, and they are
		// found by reading the instance rather than by naming the purposes,
		// so a purpose added later is emptied by the same code that counted
		// its bytes instead of being silently left behind and over-reported.
		purposes, err := os.ReadDir(dir)
		if err != nil {
			return freed, err
		}
		for _, pe := range purposes {
			if !pe.IsDir() {
				continue
			}
			if err := paced.RemoveAllFor(paced.ScratchCollection, filepath.Join(dir, pe.Name())); err != nil {
				return freed, err
			}
		}
		freed += n
		a.free, a.next, a.swept = map[Purpose][]string{}, map[Purpose]int{}, map[Purpose]bool{}
	}
	return freed, nil
}

// emptyIdleLocked removes one instance directory if no live process holds its
// claim, and reports the bytes it freed. An instance a running process owns is
// left exactly as it is.
func (a *Arena) emptyIdleLocked(dir string) (int64, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	held, err := fslock.TryLock(f)
	if err != nil {
		return 0, err
	}
	if !held {
		return 0, nil
	}
	defer fslock.Unlock(f)
	n, err := dirBytes(dir)
	if err != nil {
		return 0, err
	}
	if err := paced.RemoveAllFor(paced.ScratchCollection, dir); err != nil {
		return 0, err
	}
	return n, nil
}

// dirBytes is the length of every regular file under dir except the claim.
func dirBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || d.Name() == lockName {
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
