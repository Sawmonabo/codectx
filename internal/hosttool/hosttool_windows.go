//go:build windows

package hosttool

import (
	"os"
	"path/filepath"
)

// goInstallDirs are the default locations of the official Go installer: Go
// under Program Files, and under Program Files (x86) for a 32-bit install. A
// variable so a test can name its own.
var goInstallDirs = func(lookup Lookup) []string {
	var dirs []string
	for _, v := range []struct{ name, fallback string }{
		{"ProgramFiles", `C:\Program Files`},
		{"ProgramFiles(x86)", `C:\Program Files (x86)`},
	} {
		root, ok := lookup(v.name)
		if !ok || root == "" {
			root = v.fallback
		}
		dirs = append(dirs, filepath.Join(root, "Go", "bin"))
	}
	return dirs
}

func executableName(name string) string { return name + ".exe" }

// isExecutable is a regular file; Windows has no execute bit to check.
func isExecutable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
