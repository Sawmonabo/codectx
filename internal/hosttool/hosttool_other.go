//go:build !windows

package hosttool

import "os"

// goInstallDirs are the default locations of a Go installation outside any
// PATH: the official archive and installer (/usr/local/go), the common
// system-wide link directory, and the default Homebrew prefixes on Apple
// silicon and on Linux. A variable so a test can name its own.
var goInstallDirs = func(Lookup) []string {
	return []string{"/usr/local/go/bin", "/usr/local/bin", "/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin"}
}

func executableName(name string) string { return name }

// isExecutable is a regular file some execute bit is set on.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
