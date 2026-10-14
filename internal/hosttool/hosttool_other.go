//go:build !windows

package hosttool

import (
	"os"
	"path/filepath"
)

// goInstallDirs are the default locations of a Go installation outside any
// PATH: the official archive and installer (/usr/local/go), the common
// system-wide link directory, and the package manager prefixes /opt/homebrew
// (macOS on Apple silicon) and /home/linuxbrew/.linuxbrew (Linux). A variable
// so a test can name its own.
var goInstallDirs = func(Lookup) []string {
	return []string{"/usr/local/go/bin", "/usr/local/bin", "/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin"}
}

// userHome is the user's home directory as the environment names it: HOME, as
// os.UserHomeDir reads it on Unix.
func userHome(lookup Lookup) string {
	h, _ := lookup("HOME")
	return h
}

// userBinDir is the user's own executable directory, ~/.local/bin: in the Linux
// file system hierarchy, "Executables that shall appear in the user's $PATH
// search path". A login shell's profile puts it on PATH, and a service manager
// or an editor started outside a login shell does not read that profile, so a
// toolchain linked there is missing from their PATH. Empty with no home.
func userBinDir(lookup Lookup) string {
	if h := userHome(lookup); h != "" {
		return filepath.Join(h, ".local", "bin")
	}
	return ""
}

func executableName(name string) string { return name }

// isExecutable is a regular file some execute bit is set on.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
