package lsp

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// platformConfigDirs are the jdtls payload's per-platform Equinox
// configuration directories, by GOOS and GOARCH. The payload ships one tree
// per platform and the launcher must be pointed at the right one.
var platformConfigDirs = map[string]string{
	"linux/amd64":   "config_linux",
	"linux/arm64":   "config_linux_arm",
	"darwin/amd64":  "config_mac",
	"darwin/arm64":  "config_mac_arm",
	"windows/amd64": "config_win",
	"windows/arm64": "config_win",
}

// maxPlatformConfigBytes bounds the whole configuration tree this copies. The
// real one is a single 12 KiB config.ini; a payload whose configuration
// exceeds this is refused rather than silently half-copied.
const maxPlatformConfigBytes = 4 << 20

// seedPlatformConfig gives a runtime-hosted server its own writable copy of the
// payload's Equinox configuration directory, at <work_dir>/config, which is
// what the definition's -configuration argument names.
//
// Why a copy. Equinox writes into whatever `-configuration` names: OSGi bundle
// state, a p2 data area and its own error log. Left at the default it writes
// into the installation directory, which here is the store's published,
// digest-identified version directory -- and `codectx tools verify` cannot see
// that, because it rehashes the pinned entry and not the payload tree, so the
// immutability every store invariant rests on is broken silently (measured:
// three failed starts left four new paths inside the jdtls version directory
// while verify still reported 14 installed, exit 0).
//
// The copy is seeded once per work directory and never refreshed: the manager
// reuses the work directory across server starts, and overwriting a live
// Equinox configuration underneath a running server would corrupt its state.
func seedPlatformConfig(payloadRoot, workDir string) error {
	dst := filepath.Join(workDir, "config")
	if _, err := os.Lstat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return unavailable("the language server configuration directory cannot be inspected: %v", err)
	}
	name, ok := platformConfigDirs[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return unavailable("the pinned java language server payload carries no configuration for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	src := filepath.Join(payloadRoot, name)
	// The copy lands beside its destination and is renamed into place, so a
	// start interrupted halfway never leaves a partial configuration that the
	// next start would accept as already seeded.
	staging, err := os.MkdirTemp(workDir, "config-")
	if err != nil {
		return unavailable("the language server configuration directory cannot be created: %v", err)
	}
	defer os.RemoveAll(staging)
	budget := int64(maxPlatformConfigBytes)
	if err := copyConfigTree(src, staging, &budget); err != nil {
		return err
	}
	if err := os.Rename(staging, dst); err != nil && !errors.Is(err, fs.ErrExist) {
		// Another start of the same server won the race; its copy is as good as
		// this one, and the caller is about to use it.
		if _, serr := os.Stat(dst); serr != nil {
			return unavailable("the language server configuration directory cannot be published: %v", err)
		}
	}
	return nil
}

// copyConfigTree copies a bounded directory tree of regular files. A symlink,
// a device or anything else is refused rather than followed: the source is a
// payload the lock pinned, and a copy that resolved a link would put bytes from
// outside the payload into the server's configuration.
func copyConfigTree(src, dst string, budget *int64) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return unavailable("the pinned java language server payload has no readable configuration: %v", err)
	}
	for _, e := range entries {
		from, to := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		info, err := os.Lstat(from)
		if err != nil {
			return unavailable("the pinned java language server configuration cannot be read: %v", err)
		}
		switch {
		case info.IsDir():
			if err := os.Mkdir(to, 0o700); err != nil {
				return unavailable("the language server configuration directory cannot be created: %v", err)
			}
			if err := copyConfigTree(from, to, budget); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if *budget -= info.Size(); *budget < 0 {
				return resourceLimit("the pinned java language server configuration exceeds %d bytes", int64(maxPlatformConfigBytes))
			}
			if err := copyFile(from, to); err != nil {
				return err
			}
		default:
			return outputInvalid("the pinned java language server configuration holds %q, which is not a regular file", e.Name())
		}
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return unavailable("the pinned java language server configuration cannot be read: %v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return unavailable("the language server configuration cannot be written: %v", err)
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return unavailable("the language server configuration cannot be written: %v", err)
	}
	return nil
}
