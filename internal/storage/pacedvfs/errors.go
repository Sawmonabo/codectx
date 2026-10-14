package pacedvfs

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
)

var (
	errNoDefault = errors.New("paced: the engine has no default file system to wrap")
	errNoMemory  = errors.New("paced: out of memory registering the file system")
)

func registrationError(rc int32) error {
	return fmt.Errorf("paced: the engine refused the file system (result code %d)", rc)
}

// windows counts the waits issued so far. The space this file system frees
// is not counted here: it goes to internal/paced, where every byte the
// process gives back is counted once whatever released it.
var (
	windows atomic.Int64
	// truncations counts the windowed steps a shortening took. Nothing
	// discloses it -- what an operator reads is bytes, paced.FreedBytes --
	// but whether a shortening stepped at all is the difference between
	// freeing a window at a time and freeing in a burst, which the bytes
	// alone cannot show.
	truncations atomic.Int64
)

// Windows reports how many window waits this file system has issued since the
// process started: one per window of bytes written to a file through it. It is
// what says the shim is between the engine and the disk -- a database opened
// before the registration writes through the engine's own file system and
// moves nothing here -- so a package that opens its own database asserts on it
// rather than on the order its packages happened to initialize in.
func Windows() int64 { return windows.Load() }

// LogBytes reports the bytes written through this file system, since the
// process started, to the write-ahead log at walPath -- the database's path
// as the engine names it (PRAGMA database_list) followed by "-wal". It is the
// store's spill signal: nothing reaches the log while a group's dirty pages
// fit the writer's page cache, so a group that has moved this figure is one
// the cache has begun spilling. The log's own size cannot say that, because
// the log is rewound in place and keeps its high-water length, so a spill
// that fits inside it changes neither its size nor its header.
//
// The figure is the one database's: another database writing its own log in
// the process -- the run ledger commits four times a second -- says nothing
// about this store's cache, and counted together it would cut every group a
// quarter second in. A log removed and opened again at the same path keeps
// adding to the same figure. A path no log has been opened at reports zero.
func LogBytes(walPath string) int64 {
	logsMu.Lock()
	slot, ok := logSlots[walPath]
	logsMu.Unlock()
	if !ok {
		return 0
	}
	return (*logCounts.Load())[slot-1].Load()
}

// The per-log figures. A log's slot is fixed at its first open and its
// header carries the slot, so a write adds to its counter without a lookup.
// logCounts is replaced whole when a slot is added, so a writer reads it
// without the lock; a counter's address never changes once handed out. The
// set holds one entry per distinct log path the process ever opened.
var (
	logsMu    sync.Mutex
	logSlots  = map[string]int32{}
	logCounts atomic.Pointer[[]*atomic.Int64]
)

func init() { logCounts.Store(&[]*atomic.Int64{}) }

// logSlot returns the slot, counted from one, that counts the log at path.
func logSlot(path string) int32 {
	logsMu.Lock()
	defer logsMu.Unlock()
	if slot, ok := logSlots[path]; ok {
		return slot
	}
	counts := append(slices.Clone(*logCounts.Load()), new(atomic.Int64))
	logCounts.Store(&counts)
	slot := int32(len(counts))
	logSlots[path] = slot
	return slot
}
