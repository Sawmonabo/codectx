package paced

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

// windows counts the waits issued so far, for the package's own tests, which
// prove that a write large enough to need one went through this file system.
var windows atomic.Int64
