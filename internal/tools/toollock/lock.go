package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// The lock schema is the runtime's, not a second copy of it. A generator that
// declared its own structs and its own validation could emit a document the
// shipped binary refuses -- and toolchain.Embedded panics on such a document,
// by design, because the lock is a trust root rather than user input -- so the
// only defence that cannot drift is writing through the runtime's own parser.
type (
	Payload = toolchain.Payload
	Entry   = toolchain.Entry
	Lock    = toolchain.Lock
)

// writeLock marshals the assembled lock and round-trips it through
// toolchain.ParseLock before a byte reaches disk, so the file this generator
// writes is a file the runtime has already accepted.
func writeLock(l Lock, path string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		return err
	}
	if _, err := toolchain.ParseLock(buf.Bytes()); err != nil {
		return fmt.Errorf("the assembled lock is one the runtime would refuse: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), fileMode)
}

// readLock parses an existing lock with the runtime's parser, which is also its
// validation: there is no second gate that could accept more than the product.
func readLock(path string) (Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Lock{}, err
	}
	l, err := toolchain.ParseLock(b)
	if err != nil {
		return Lock{}, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}
