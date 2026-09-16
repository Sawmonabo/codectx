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

// windows counts the waits issued so far and truncations the windows freed
// one at a time, for the package's own tests, which prove that a write or a
// truncation large enough to need them went through this file system.
var (
	windows     atomic.Int64
	truncations atomic.Int64
)
