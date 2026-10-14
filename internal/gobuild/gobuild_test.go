package gobuild

import (
	"bytes"
	"go/build"
	"go/build/constraint"
	"io"
	"testing"
)

// TestConstraintsMatchTheGoCommand checks the constraint Header and FileName
// read of each file against go/build's own decision for that file in every
// port, with and without cgo and a custom tag: a header rule or file-name
// rule that differs from the go command's (a +build line past the blank
// line, a //go:build line after code, the android/linux exception, the
// _test suffix) fails here.
func TestConstraintsMatchTheGoCommand(t *testing.T) {
	files := []struct{ name, src string }{
		{"a.go", "//go:build linux && !cgo\n\npackage p\n"},
		{"b.go", "// Copyright\n\n//go:build (darwin || windows) && feat\n\npackage p\n"},
		{"c.go", "// +build linux darwin\n// +build amd64\n\npackage p\n"},
		{"d.go", "// +build linux\npackage p\n"},
		{"e.go", "/* block */\n//go:build windows\n\npackage p\n"},
		{"f.go", "package p\n\n//go:build windows\n"},
		{"g_linux.go", "package p\n"},
		{"h_windows_arm64.go", "package p\n"},
		{"i_386_test.go", "package p\n"},
		{"linux.go", "package p\n"},
		{"j_unix.go", "//go:build unix\n\npackage p\n"},
		{"k_solaris.go", "package p\n"},
		{"l.go", "//go:build ios || illumos\n\npackage p\n"},
		{"m_android.go", "//go:build !feat\n\npackage p\n"},
	}
	for _, f := range files {
		x := and(Header([]byte(f.src)), FileName(f.name))
		for _, port := range ports {
			for _, cgo := range []bool{false, true} {
				for _, tags := range [][]string{nil, {"feat"}} {
					ctx := build.Default
					ctx.GOOS, ctx.GOARCH, ctx.CgoEnabled, ctx.BuildTags = port[0], port[1], cgo, tags
					ctx.OpenFile = func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader([]byte(f.src))), nil }
					want, err := ctx.MatchFile(".", f.name)
					if err != nil {
						t.Fatal(err)
					}
					b := setting{goos: port[0], goarch: port[1], compiler: "gc", cgo: cgo, release: 27, tags: map[string]bool{}}
					for _, tag := range tags {
						b.tags[tag] = true
					}
					got := x == nil || x.Eval(b.match)
					if got != want {
						t.Fatalf("%s on %s/%s cgo=%v tags=%v: holds %v, the go command builds it %v", f.name, port[0], port[1], cgo, tags, got, want)
					}
				}
			}
		}
	}
}

// TestSatisfiableNeedsOneBuild checks that a conjunction holds only when one
// build sets all of it: two ports' files never meet, an implied tag meets its
// implying port, and a release above the module's minimum is some build's.
func TestSatisfiableNeedsOneBuild(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"linux && 386", true},
		{"linux && windows", false},
		{"386 && amd64", false},
		{"android && linux", true},
		{"unix && !linux && !darwin && amd64", true},
		{"windows && unix", false},
		{"go1.30 && !go1.29", false},
		{"go1.30", true},
		{"!go1.21", false},
		{"feat && !feat", false},
		{"feat && other && cgo && gccgo", true},
		{"wasm && !js && !wasip1", false},
	}
	for _, c := range cases {
		x, err := constraint.Parse("//go:build " + c.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, decided := Satisfiable(x, 22)
		if !decided || got != c.want {
			t.Errorf("Satisfiable(%s) = %v (decided %v), want %v", c.expr, got, decided, c.want)
		}
	}
}

func and(a, b string) constraint.Expr {
	var x constraint.Expr
	for _, s := range []string{a, b} {
		if s == "" {
			continue
		}
		y, err := constraint.Parse("//go:build " + s)
		if err != nil {
			panic(err)
		}
		if x == nil {
			x = y
		} else {
			x = &constraint.AndExpr{X: x, Y: y}
		}
	}
	return x
}
