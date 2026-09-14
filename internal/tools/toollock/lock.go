package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// Payload is one platform's published asset. url, sha256 and size are the
// schema the runtime resolver consumes.
//
// entry and entry_sha256 are additional per-platform fields. They exist because
// a native tool's launcher differs per platform (bin/node vs node.exe) and no
// single tool-level digest can be true for all six; the runtime must prefer
// these when present. See the lane report's shared-helper section.
type Payload struct {
	URL            string `json:"url"`
	SHA256         string `json:"sha256"`
	Size           int64  `json:"size"`
	Entry          string `json:"entry"`
	EntrySHA256    string `json:"entry_sha256"`
	Provenance     string `json:"provenance"`                // "upstream" or "built"
	UpstreamDigest string `json:"upstream_digest,omitempty"` // "<algo>:<hex>" that upstream published
}

// Entry is one lock entry.
type Entry struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Kind        string             `json:"kind"`
	License     string             `json:"license"`
	Upstream    string             `json:"upstream"`
	Runtime     string             `json:"runtime"`
	Entry       string             `json:"entry"`
	EntrySHA256 string             `json:"entry_sha256"`
	Platforms   map[string]Payload `json:"platforms"`
	Languages   []string           `json:"languages,omitempty"`
	Notes       string             `json:"notes,omitempty"`
}

// Lock is the embedded tool lock.
type Lock struct {
	LockVersion int              `json:"lock_version"`
	Tools       map[string]Entry `json:"tools"`
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// validate applies the same init-time rules the runtime applies, so a lock this
// generator writes can never be one the product refuses to start with.
func (l Lock) validate() error {
	if l.LockVersion != 1 {
		return fmt.Errorf("lock_version %d", l.LockVersion)
	}
	runtimes := map[string]bool{}
	for name, e := range l.Tools {
		if e.Runtime != "" {
			runtimes[e.Runtime] = true
		}
		if name != e.Name {
			return fmt.Errorf("tool key %q disagrees with name %q", name, e.Name)
		}
		if e.Version == "" || e.Kind == "" || e.License == "" || e.Upstream == "" {
			return fmt.Errorf("%s: incomplete entry", name)
		}
		switch e.Kind {
		case "indexer", "server", "cpg", "runtime":
		default:
			return fmt.Errorf("%s: unknown kind %q", name, e.Kind)
		}
		if _, err := confinedRel(e.Entry); err != nil {
			return fmt.Errorf("%s: entry %q: %w", name, e.Entry, err)
		}
		if e.EntrySHA256 != "" && !hexDigest.MatchString(e.EntrySHA256) {
			return fmt.Errorf("%s: entry_sha256 %q is not lowercase hex", name, e.EntrySHA256)
		}
		if len(e.Platforms) == 0 {
			return fmt.Errorf("%s: no platforms", name)
		}
		for plat, p := range e.Platforms {
			if !validPlatform(plat) {
				return fmt.Errorf("%s: unknown platform key %q", name, plat)
			}
			u, err := url.Parse(p.URL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.Path == "" {
				return fmt.Errorf("%s/%s: %q is not an absolute https URL with a host and a path", name, plat, p.URL)
			}
			if !hexDigest.MatchString(p.SHA256) {
				return fmt.Errorf("%s/%s: sha256 %q is not lowercase hex", name, plat, p.SHA256)
			}
			if p.Size <= 0 {
				return fmt.Errorf("%s/%s: size %d", name, plat, p.Size)
			}
			if _, err := confinedRel(p.Entry); err != nil {
				return fmt.Errorf("%s/%s: entry %q: %w", name, plat, p.Entry, err)
			}
			if !hexDigest.MatchString(p.EntrySHA256) {
				return fmt.Errorf("%s/%s: entry_sha256 %q is not lowercase hex", name, plat, p.EntrySHA256)
			}
		}
	}
	for r := range runtimes {
		if _, ok := l.Tools[r]; !ok {
			return fmt.Errorf("runtime %q referenced but absent from the lock", r)
		}
	}
	return nil
}

func validPlatform(p string) bool {
	for _, known := range platformOrder {
		if known == p {
			return true
		}
	}
	return false
}

func writeLock(l Lock, path string) error {
	if err := l.validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(l); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), fileMode)
}

func readLock(path string) (Lock, error) {
	var l Lock
	b, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, err
	}
	return l, nil
}

func sortedToolNames(l Lock) []string {
	names := make([]string, 0, len(l.Tools))
	for n := range l.Tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
