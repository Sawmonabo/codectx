// Package hosttool finds the language toolchains on this machine that the
// precise indexers and language servers drive and codectx does not ship: the
// go command, and cargo with rustc. A process that a service manager, an
// editor or an agent client starts often has a shorter PATH than the user's
// shell, and a toolchain missing from that PATH is not missing from the
// machine. Each tool is therefore looked up where its installers put it, in
// this order: PATH, then the tool's own home variable, then the user's own
// executable directory and the installers' default locations.
package hosttool

import (
	"os"
	"path/filepath"
	"strings"
)

// Lookup reads one environment variable. The product passes os.LookupEnv; a
// caller that builds a child's environment from an allowlist passes the same
// lookup it reads that allowlist with.
type Lookup func(string) (string, bool)

// Find returns the absolute path of the named host tool ("go", "cargo" or
// "rustc") and true, or "" and false when none of its places holds an
// executable file. A relative PATH entry is skipped, as the go command's own
// lookup refuses one: it names a different directory in every working
// directory.
func Find(name string, lookup Lookup) (string, bool) {
	exe := executableName(name)
	if path, ok := lookup("PATH"); ok {
		if p, ok := inDirs(filepath.SplitList(path), exe); ok {
			return p, true
		}
	}
	return inDirs(fallbackDirs(name, lookup), exe)
}

// ChildPATH is the PATH of a child process that runs the named tools: the
// parent's PATH, with the directory of each named tool that the PATH does not
// hold but Find locates elsewhere appended after it, so nothing the user put
// first is shadowed. ok is false only when the parent has no PATH and no tool
// was found, which is the one case where the child gets no PATH at all.
func ChildPATH(lookup Lookup, names ...string) (string, bool) {
	path, had := lookup("PATH")
	dirs := filepath.SplitList(path)
	out := path
	for _, name := range names {
		exe := executableName(name)
		if _, ok := inDirs(dirs, exe); ok {
			continue
		}
		p, ok := inDirs(fallbackDirs(name, lookup), exe)
		if !ok {
			continue
		}
		dir := filepath.Dir(p)
		dirs = append(dirs, dir)
		if out == "" {
			out = dir
		} else {
			out += string(os.PathListSeparator) + dir
		}
	}
	if !had && out == "" {
		return "", false
	}
	return out, true
}

// GoEnv reads one Go environment variable the way the go command does ("go help
// environment"): the environment, else the user's go env file (GOENV, default
// os.UserConfigDir()/go/env, "off" for none), which is where `go env -w`
// writes. The file is read by the go command's own rule (cmd/go
// internal/cfg readEnvFile): a line is KEY=value with KEY starting with an
// upper-case letter, nothing is trimmed, and a later line for a key replaces
// an earlier one.
func GoEnv(key string, lookup Lookup) string {
	if v, ok := lookup(key); ok && v != "" {
		return v
	}
	file, _ := lookup("GOENV")
	if file == "off" {
		return ""
	}
	if file == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		file = filepath.Join(dir, "go", "env")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	var value string
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSuffix(line, "\n")
		k, v, ok := strings.Cut(line, "=")
		if ok && k != "" && k[0] >= 'A' && k[0] <= 'Z' && k == key {
			value = v
		}
	}
	return value
}

// fallbackDirs are the directories after PATH where the named tool's
// installers put it:
//   - go: GOROOT/bin (GOROOT as GoEnv reads it), then the user's executable
//     directory (userBinDir), then the Go installers' and the common package
//     managers' default locations (goInstallDirs);
//   - cargo and rustc: the directory of the executable CARGO or RUSTC names
//     (the variable rust-analyzer reads first), then CARGO_HOME/bin, then the
//     home directory's .cargo/bin, where rustup installs rustc and cargo
//     ("Cargo's bin directory").
func fallbackDirs(name string, lookup Lookup) []string {
	var dirs []string
	switch name {
	case "go":
		if root := GoEnv("GOROOT", lookup); root != "" {
			dirs = append(dirs, filepath.Join(root, "bin"))
		}
		if dir := userBinDir(lookup); dir != "" {
			dirs = append(dirs, dir)
		}
		dirs = append(dirs, goInstallDirs(lookup)...)
	case "cargo", "rustc":
		if exe, ok := lookup(strings.ToUpper(name)); ok && filepath.IsAbs(exe) {
			dirs = append(dirs, filepath.Dir(exe))
		}
		if home, ok := lookup("CARGO_HOME"); ok && home != "" {
			dirs = append(dirs, filepath.Join(home, "bin"))
		}
		if home := userHome(lookup); home != "" {
			dirs = append(dirs, filepath.Join(home, ".cargo", "bin"))
		}
	}
	return dirs
}

// inDirs is the first absolute directory of dirs holding an executable regular
// file named exe.
func inDirs(dirs []string, exe string) (string, bool) {
	for _, dir := range dirs {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, exe)
		if isExecutable(p) {
			return p, true
		}
	}
	return "", false
}
