package pacedvfs

import (
	"errors"
	"fmt"
	"sync/atomic"
)

var (
	errNoDefault = errors.New("paced: the engine has no default file system to wrap")
	errNoMemory  = errors.New("paced: out of memory registering the file system")
)

func registrationError(rc int32) error {
	return fmt.Errorf("paced: the engine refused the file system (result code %d)", rc)
}

// windows counts the waits issued so far, truncations the windows freed one
// at a time, and logBytes the bytes written to a write-ahead log through this
// file system.
var (
	windows     atomic.Int64
	truncations atomic.Int64
	logBytes    atomic.Int64
)

// Truncations reports how many windows this file system has freed one at a
// time since the process started. A run that reuses its space instead of
// freeing it leaves this at zero, which is what the diagnostics disclose and
// what the store's log test asserts.
func Truncations() int64 { return truncations.Load() }

// LogBytes reports the bytes written to a write-ahead log through this file
// system since the process started. It is the store's spill signal: nothing
// reaches the log while a group's dirty pages fit the writer's page cache, so
// a group that has moved this figure is one the cache has begun spilling. The
// log's own size cannot say that any more, because the log is rewound in
// place and keeps its high-water length, so a spill that fits inside it
// changes neither its size nor its header. The count is the process's, not
// one database's: a second store's log moves it too, which can only make a
// group commit earlier than it had to, never later than it must.
func LogBytes() int64 { return logBytes.Load() }
