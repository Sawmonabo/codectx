package gobuild

import (
	"bytes"
	"go/build"
	"go/build/constraint"
	"io"
	"strconv"
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
		{"feat && !feat", false},
		{"feat && other && cgo && gccgo", true},
		{"wasm && !js && !wasip1", false},
		// Mutation: one compiler only -> unsat.
		{"gccgo && !gc", true},
		// Mutation: cgo tied to the port's default cgo support -> unsat
		// (CGO_ENABLED is respected on every port, cmd/go/internal/cfg).
		{"cgo && js && wasm", true},
		// Mutation: releases independent of each other -> sat.
		{"go1.30 && !go1.29", false},
		// Mutation: the module's floor ignored -> sat.
		{"!go1.26", false},
		{"go1.30", true},
		// Mutation: a release allocated between two others not chained to
		// both -> sat.
		{"go1.31 && go1.29 && !go1.30", false},
		// Mutation: feature tags free -> each of these reads sat.
		{"amd64.v3 && arm64", false},
		{"amd64.v2 && !amd64.v1", false},
		{"386.sse2 && 386.softfloat", false},
		{"wasm && !wasm.signext", false},
		// Mutation: one port alone tried when the expression names no
		// operating system or architecture -> unsat (aix/ppc64 has no
		// amd64 feature).
		{"amd64.v2", true},
		{"amd64.v2 && !amd64", false},
		// Mutation: the arm64 v9.x => v8.(x+5) rule dropped -> sat.
		{"arm64.v9.1 && !arm64.v8.6", false},
		{"arm64.v9.1 && !arm64.v8.7", true},
		{"amd64 && !amd64.v2", true},
		// Mutation: an undefined feature of a known architecture taken as
		// a free tag -> sat.
		{"amd64.v9", false},
		{"riscv64.rva23u64 && !riscv64.rva22u64", false},
	}
	for _, c := range cases {
		x, err := constraint.Parse("//go:build " + c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := Satisfiable(x, 27); got != c.want {
			t.Errorf("Satisfiable(%s) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestSatisfiableHasNoTagBound checks conditions naming more free tags than
// any enumeration of them could afford are decided exactly.
func TestSatisfiableHasNoTagBound(t *testing.T) {
	// Mutation: a bound past which a condition counts as met -> 18 free
	// tags with a contradiction read sat.
	contra := "t0 && !t0"
	for i := 1; i < 18; i++ {
		contra += " && t" + strconv.Itoa(i)
	}
	// Mutation: a bound past which a condition counts as not met -> 19
	// free tags with a model read unsat.
	pairs := ""
	for i := 0; i < 18; i += 2 {
		pairs += "(t" + strconv.Itoa(i) + " || t" + strconv.Itoa(i+1) + ") && "
	}
	pairs += "t18"
	for _, c := range []struct {
		expr string
		want bool
	}{{contra, false}, {pairs, true}, {pairs + " && amd64.v3 && arm64", false}} {
		x, err := constraint.Parse("//go:build " + c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if got := Satisfiable(x, 27); got != c.want {
			t.Errorf("Satisfiable(%s) = %v, want %v", c.expr, got, c.want)
		}
	}
}

// TestCondsAreCanonical checks one function is one id, whatever its
// spelling. Mutation: interning conditions by their text -> `a && b` and
// `b && a` are two ids, and so are `x || x` and x.
func TestCondsAreCanonical(t *testing.T) {
	c := NewConds(27)
	parse := func(s string) Cond {
		x, err := constraint.Parse("//go:build " + s)
		if err != nil {
			t.Fatal(err)
		}
		return c.Expr(x)
	}
	ab, ba := parse("a && b"), parse("b && a")
	if ab != ba {
		t.Errorf("a && b is %d, b && a is %d", ab, ba)
	}
	x := parse("linux && (feat || !cgo)")
	if c.Or(x, x) != x || c.And(x, parse("(!cgo || feat) && linux")) != x {
		t.Errorf("one condition spelled twice has two ids")
	}
	if c.And(x, c.Not(x)) != False || c.Or(x, c.Not(x)) != True {
		t.Errorf("a condition and its negation do not meet in False and True")
	}
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

// BenchmarkOrChain builds the disjunction of 1,000 tags one at a time, as
// the linker's running disjunctions do: one node per tag, not a text per
// prefix of the chain.
func BenchmarkOrChain(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		c := NewConds(27)
		or := False
		for i := range 1000 {
			or = c.Or(or, c.tag("t"+strconv.Itoa(i)))
		}
		if !c.Holds(or) {
			b.Fatal("unmet")
		}
	}
}
