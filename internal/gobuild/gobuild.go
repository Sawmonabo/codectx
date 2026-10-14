// Package gobuild reads the build constraints of Go source files and decides
// whether a combination of them holds in some build, by the rules the go
// command applies (go/build, "Build Constraints"; the toolchain's go/build
// Context.matchTag, goodOSArchFile, shouldBuild and parseFileHeader, Go
// 1.27.2).
package gobuild

import (
	"bytes"
	"go/build/constraint"
	"strconv"
	"strings"
)

// Header is the build constraint the leading comments of a Go file's content
// state, as a constraint expression's text, or "" when it states none: the
// `//go:build` line when there is one, else the conjunction of the `// +build`
// lines of the leading run of line comments and blank lines that a blank line
// ends. A content with two `//go:build` lines, or one the parser refuses, is
// a file the go command refuses to build; it is reported unconstrained here,
// as the declarations the linker reads of it are.
func Header(content []byte) string {
	end, goBuild, ok := header(content)
	if !ok {
		return ""
	}
	if goBuild != nil {
		x, err := constraint.Parse(string(goBuild))
		if err != nil {
			return ""
		}
		return x.String()
	}
	var all constraint.Expr
	for line := range bytes.Lines(content[:end]) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("//")) || !bytes.Contains(line, []byte("+build")) {
			continue
		}
		text := string(line)
		if !constraint.IsPlusBuild(text) {
			continue
		}
		x, err := constraint.Parse(text)
		if err != nil {
			continue
		}
		if all == nil {
			all = x
		} else {
			all = &constraint.AndExpr{X: all, Y: x}
		}
	}
	if all == nil {
		return ""
	}
	return all.String()
}

// header is go/build's parseFileHeader: the end of the leading run of line
// comments and blank lines up to its last blank line, and the `//go:build`
// line among the leading comments; ok is false for two such lines.
func header(content []byte) (end int, goBuild []byte, ok bool) {
	p := content
	ended := false
	inSlashStar := false
Lines:
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(line, '\n'); i >= 0 {
			line, p = line[:i], p[i+1:]
		} else {
			p = p[len(p):]
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(content) - len(p)
			continue Lines
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inSlashStar && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return 0, nil, false
			}
			goBuild = line
		}
	Comments:
		for len(line) > 0 {
			if inSlashStar {
				if i := bytes.Index(line, []byte("*/")); i >= 0 {
					inSlashStar = false
					line = bytes.TrimSpace(line[i+2:])
					continue Comments
				}
				continue Lines
			}
			if bytes.HasPrefix(line, []byte("//")) {
				continue Lines
			}
			if bytes.HasPrefix(line, []byte("/*")) {
				inSlashStar = true
				line = bytes.TrimSpace(line[2:])
				continue Comments
			}
			break Lines
		}
	}
	return end, goBuild, true
}

// File is the build constraint of the Go file named name (its base name) of
// content content: Header's and FileName's together, "" when neither states
// one.
func File(name string, content []byte) string {
	h, n := Header(content), FileName(name)
	switch {
	case h == "":
		return n
	case n == "":
		return h
	}
	x, err := constraint.Parse("//go:build (" + h + ") && (" + n + ")")
	if err != nil {
		return h
	}
	return x.String()
}

// FileName is the build constraint a Go file's base name states (go/build
// goodOSArchFile), as a constraint expression's text, or "": the name, cut
// at its first dot and with everything before its first underscore ignored,
// ending in `_GOOS_GOARCH`, `_GOOS` or `_GOARCH` (a `_test` suffix aside)
// for a known operating system and architecture.
func FileName(name string) string {
	name, _, _ = strings.Cut(name, ".")
	i := strings.Index(name, "_")
	if i < 0 {
		return ""
	}
	l := strings.Split(name[i:], "_")
	if n := len(l); n > 0 && l[n-1] == "test" {
		l = l[:n-1]
	}
	n := len(l)
	if n >= 2 && knownOS[l[n-2]] && knownArch[l[n-1]] {
		return l[n-2] + " && " + l[n-1]
	}
	if n >= 1 && (knownOS[l[n-1]] || knownArch[l[n-1]]) {
		return l[n-1]
	}
	return ""
}

// knownOS, unixOS and knownArch are the toolchain's internal/syslist (Go
// 1.27.2): the operating systems and architectures a file name or tag can
// name, and the operating systems the `unix` tag matches.
var knownOS = set("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "js", "linux",
	"nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows", "zos")

var unixOS = set("aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios", "linux", "netbsd",
	"openbsd", "solaris")

var knownArch = set("386", "amd64", "amd64p32", "arm", "armbe", "arm64", "arm64be", "loong64", "mips", "mipsle",
	"mips64", "mips64le", "mips64p32", "mips64p32le", "ppc", "ppc64", "ppc64le", "riscv", "riscv64", "s390",
	"s390x", "sparc", "sparc64", "wasm")

// ports are the GOOS/GOARCH pairs the toolchain builds for (`go tool dist
// list`, Go 1.27.2): every build is one of them.
var ports = [][2]string{
	{"aix", "ppc64"}, {"android", "386"}, {"android", "amd64"}, {"android", "arm"}, {"android", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"}, {"dragonfly", "amd64"}, {"freebsd", "386"}, {"freebsd", "amd64"},
	{"freebsd", "arm"}, {"freebsd", "arm64"}, {"illumos", "amd64"}, {"ios", "amd64"}, {"ios", "arm64"},
	{"js", "wasm"}, {"linux", "386"}, {"linux", "amd64"}, {"linux", "arm"}, {"linux", "arm64"},
	{"linux", "loong64"}, {"linux", "mips"}, {"linux", "mips64"}, {"linux", "mips64le"}, {"linux", "mipsle"},
	{"linux", "ppc64"}, {"linux", "ppc64le"}, {"linux", "riscv64"}, {"linux", "s390x"}, {"netbsd", "386"},
	{"netbsd", "amd64"}, {"netbsd", "arm"}, {"netbsd", "arm64"}, {"openbsd", "386"}, {"openbsd", "amd64"},
	{"openbsd", "arm"}, {"openbsd", "arm64"}, {"openbsd", "ppc64"}, {"openbsd", "riscv64"}, {"plan9", "386"},
	{"plan9", "amd64"}, {"plan9", "arm"}, {"solaris", "amd64"}, {"wasip1", "wasm"}, {"windows", "386"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// MaxFreeTags bounds the tags of no fixed meaning (custom tags, tool tags such
// as `amd64.v2`, experiments) one decision enumerates both ways: 2^16
// assignments of them for each port, compiler, cgo setting and release.
const MaxFreeTags = 16

// Satisfiable reports whether some build satisfies x: some port, compiler
// (gc or gccgo), cgo setting, release of at least go1.minor (release tags
// go1.1 through the toolchain's, which is at least the module's), and
// assignment of every other tag x names. decided is false when x names more
// than MaxFreeTags such tags, which are then not enumerated.
func Satisfiable(x constraint.Expr, minor int) (sat, decided bool) {
	var free []string
	var releases []int
	platform, compiler, cgo := false, false, false
	seen := map[string]bool{}
	tags(x, func(tag string) {
		if tag == "boringcrypto" {
			tag = "goexperiment.boringcrypto"
		}
		if seen[tag] {
			return
		}
		seen[tag] = true
		switch {
		case tag == "cgo":
			cgo = true
		case tag == "gc" || tag == "gccgo":
			compiler = true
		case tag == "unix" || knownOS[tag] || knownArch[tag]:
			platform = true
		default:
			if r, ok := release(tag); ok {
				if r > minor {
					releases = append(releases, r)
				}
				return
			}
			free = append(free, tag)
		}
	})
	if len(free) > MaxFreeTags {
		return true, false
	}
	plats := ports
	if !platform {
		plats = ports[:1]
	}
	compilers := []string{"gc"}
	if compiler {
		compilers = []string{"gc", "gccgo"}
	}
	cgos := []bool{false}
	if cgo {
		cgos = []bool{false, true}
	}
	levels := append([]int{minor}, releases...)
	on := map[string]bool{}
	for _, port := range plats {
		for _, comp := range compilers {
			for _, c := range cgos {
				for _, level := range levels {
					for bits := 0; bits < 1<<len(free); bits++ {
						clear(on)
						for i, f := range free {
							if bits&(1<<i) != 0 {
								on[f] = true
							}
						}
						b := setting{goos: port[0], goarch: port[1], compiler: comp, cgo: c, release: level, tags: on}
						if x.Eval(b.match) {
							return true, true
						}
					}
				}
			}
		}
	}
	return false, true
}

// setting is one build's settings: its port, compiler, cgo setting, release
// (go1.1 through go1.release hold) and the other tags it sets.
type setting struct {
	goos, goarch, compiler string
	cgo                    bool
	release                int
	tags                   map[string]bool
}

// match is go/build's Context.matchTag for b.
func (b setting) match(tag string) bool {
	if b.cgo && tag == "cgo" {
		return true
	}
	if tag == b.goos || tag == b.goarch || tag == b.compiler {
		return true
	}
	if b.goos == "android" && tag == "linux" || b.goos == "illumos" && tag == "solaris" || b.goos == "ios" && tag == "darwin" {
		return true
	}
	if tag == "unix" && unixOS[b.goos] {
		return true
	}
	if tag == "boringcrypto" {
		tag = "goexperiment.boringcrypto"
	}
	if r, ok := release(tag); ok {
		return r <= b.release
	}
	return b.tags[tag]
}

// tags calls visit with every tag x names (Eval stops early, so it cannot
// list them).
func tags(x constraint.Expr, visit func(string)) {
	switch x := x.(type) {
	case *constraint.TagExpr:
		visit(x.Tag)
	case *constraint.NotExpr:
		tags(x.X, visit)
	case *constraint.AndExpr:
		tags(x.X, visit)
		tags(x.Y, visit)
	case *constraint.OrExpr:
		tags(x.X, visit)
		tags(x.Y, visit)
	}
}

// release is the minor version of a release tag `go1.N`.
func release(tag string) (int, bool) {
	rest, ok := strings.CutPrefix(tag, "go1.")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
