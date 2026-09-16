// Package scratch is the pool of working files a store writes for its own
// later reading: spool segments, sort runs, staging databases, content-store
// temporaries, materialized trees, cursor and traversal state.
//
// Freeing disk space is the expensive act. On a filesystem that discards
// freed blocks under a sparse virtual disk, releasing gigabytes stalls every
// other writer on the machine for about a minute, a minute after the free,
// and nothing inside the machine can observe or wait for that work. A run
// that creates its working files and removes them when it is done pays that
// cost once per surface; a run that takes the same files again pays it never.
//
// So a scratch file is taken from the arena and given back to it. Take hands
// out a file of the asked-for purpose opened for overwrite at offset zero,
// with no truncation, or creates one when the pool is empty; Release returns
// it for the next taker. A file keeps whatever length its largest tenant
// gave it for as long as the arena exists, and the arena is emptied only
// when an operator asks for it.
//
// Because nothing truncates, the file's length is not the data's length: a
// tenant that wrote a megabyte into a file whose previous tenant wrote ten
// will read nine megabytes of the previous tenant's bytes after its own if
// it reads to end of file. Every tenant therefore carries its own logical
// length — a header, a record count, a size the caller already knows — and
// never learns it from os.Stat, from Seek to the end, or from reading until
// EOF. A tenant that cannot do that must not use the arena.
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

	"github.com/Sawmonabo/codectx/internal/paced"
)

// A Purpose names a family of interchangeable scratch surfaces. Files of one
// purpose are pooled together, so a purpose groups tenants whose sizes and
// lifetimes are alike: every spool segment is one purpose, every sort run
// another. The name becomes a directory under the arena root, so it is a
// plain lowercase identifier.
type Purpose string

// The purposes the product takes from an arena. They live here rather than
// beside their tenants so the set of surfaces a store can hold is readable
// in one place.
const (
	// SpoolSegment is one segment file of a paginated result spool.
	SpoolSegment Purpose = "spool-segment"
	// SortRun is one run file of an external sort, and its merged output.
	SortRun Purpose = "sort-run"
)

// dirName is the arena's own directory under a store's data directory.
const dirName = "scratch"

// An Arena is the pool of scratch surfaces under one root directory. One
// exists per root: a store's arena is the arena of its data directory, and
// every part of the store that writes a working file takes it from there,
// so the pool is shared by the whole store without a handle threaded
// through every constructor.
//
// An Arena is safe for concurrent use within one process: Take never hands
// the same surface to two callers, because a taken surface leaves the free
// list and returns to it only when its lease is released. It claims nothing
// across processes, so a purpose may only be pooled where a single process
// owns the directory — true of the writer's surfaces, which the workspace's
// one indexing owner holds. A purpose written from a path several processes
// can run at once needs a claim across processes first, or two of them would
// take the same surface and each read the other's bytes as its own.
type Arena struct {
	root string

	mu    sync.Mutex
	free  map[Purpose][]string // paths available to take, per purpose
	held  map[string]bool      // paths currently leased
	next  map[Purpose]int      // the next ordinal to create, per purpose
	swept map[Purpose]bool     // purposes whose directory has been read
}

// arenas is the process's arena per root directory.
var (
	arenasMu sync.Mutex
	arenas   = map[string]*Arena{}
)

// For returns the arena rooted under the given data directory, creating it on
// first use. Callers that already hold the directory a surface belongs under
// — every producer of scratch in the product already takes one — reach the
// store's pool through it with no new plumbing.
func For(dataDir string) *Arena {
	root := filepath.Join(dataDir, dirName)
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

// Root is the arena's directory.
func (a *Arena) Root() string { return a.root }

// A Lease is one scratch surface taken from an arena. It is returned to the
// pool by Release, which never removes it from the disk.
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
// every call for the life of the lease; Release closes it.
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

// Release gives the surface back to the arena for the next taker. The file
// stays on the disk at its high-water length: releasing frees nothing, which
// is the whole point of the arena. Releasing twice is a no-op.
func (l *Lease) Release() {
	l.mu.Lock()
	f := l.file
	l.file = nil
	l.mu.Unlock()
	if f != nil {
		_ = f.Close()
	}
	if l.arena == nil {
		return
	}
	a := l.arena
	l.arena = nil
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.held[l.path] {
		return
	}
	delete(a.held, l.path)
	a.free[l.purpose] = append(a.free[l.purpose], l.path)
}

// Take hands out a surface of the given purpose: one the arena already holds
// and nobody is using, or a new one when the pool is empty. The surface is an
// existing file, never truncated, whose bytes past this tenant's writes are
// the previous tenant's.
func (a *Arena) Take(p Purpose) (*Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
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

// sweepLocked reads a purpose's directory once per process so surfaces left
// by an earlier run of the store are taken again instead of created beside
// them. Without it a restarted store would grow a second pool and free the
// first only when an operator emptied the arena.
func (a *Arena) sweepLocked(p Purpose) error {
	if a.swept[p] {
		return nil
	}
	a.swept[p] = true
	entries, err := os.ReadDir(filepath.Join(a.root, string(p)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var names []string
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 0 {
			continue
		}
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
		if n >= a.next[p] {
			a.next[p] = n + 1
		}
	}
	sort.Strings(names)
	for _, n := range names {
		a.free[p] = append(a.free[p], filepath.Join(a.root, string(p), n))
	}
	return nil
}

// createLocked makes one new surface of a purpose.
func (a *Arena) createLocked(p Purpose) (string, error) {
	dir := filepath.Join(a.root, string(p))
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

// ErrInUse refuses to empty an arena while something holds a surface of it.
var ErrInUse = errors.New("scratch arena in use")

// Empty removes every surface the arena holds, one window at a time, and
// forgets them. It is the only place the arena's disk is given back, and it
// runs only when an operator asks for it: no timer, no threshold, no
// setting. It refuses while any lease is outstanding, because removing a
// surface a tenant is writing would corrupt that tenant's work.
func (a *Arena) Empty() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.held); n > 0 {
		return fmt.Errorf("%w: %d surfaces are leased", ErrInUse, n)
	}
	if err := paced.RemoveAll(a.root); err != nil {
		return err
	}
	a.free = map[Purpose][]string{}
	a.next = map[Purpose]int{}
	a.swept = map[Purpose]bool{}
	return nil
}
