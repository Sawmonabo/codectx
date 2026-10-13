package link_test

import (
	"strconv"
	"testing"
)

// TestGoBuildGoldens runs the Go goldens whose answers depend on deciding,
// per build, which declarations one build holds together (runGoldens). Each
// fixture's facts were checked with the go command in every build it names
// (go vet with a `var _ I = T{}` assertion per build).
func TestGoBuildGoldens(t *testing.T) { runGoldens(t, goBuildGoldens) }

var goBuildGoldens = []golden{
	{
		name:   "external-overrides-in-a-satisfying-build",
		anchor: "go/build, \"Build Constraints\"; The Go Programming Language Specification, \"Method sets\", \"Interface types\", \"Alias declarations\"",
		files: map[string]string{
			"go.mod":     "module example.com/m\n\ngo 1.27\n\nrequire example.com/arch v1.0.0\n",
			"t.go":       "package p\n\nimport \"example.com/arch\"\n\ntype T struct{} // @tt\n\nfunc use(arch.Wide) {}\n",
			"f_386.go":   "package p\n\nfunc (T) F() uint32 { return 0 } // @f32\n",
			"f_amd64.go": "package p\n\nfunc (T) F() uint64 { return 0 } // @f64\n\nfunc (T) G() {} // @tg\n",
		},
		// Checked with go vet: arch.Wide's F returns Word, uint32 on 386 and uint64 on amd64; T implements
		// Wide on amd64 and lacks G on 386. So T implements Wide, and only the amd64 F overrides Wide.F: the
		// 386 F matches Wide.F's 386 key, but no build that satisfies Wide holds it.
		want: []string{},
		facts: []string{
			"tt T implements external example.com/arch.Wide",
			"f64 F overrides external example.com/arch.Wide.F",
			"tg G overrides external example.com/arch.Wide.G",
		},
		// Mutation: a key counted as met when some build holds it in both, not a build that satisfies the
		// whole interface -> f32 F overrides external example.com/arch.Wide.F.
		fails: "an override published for a key no build satisfying the interface holds",
	},
	{
		name:   "per-build-constants-without-a-choice-bound",
		anchor: "go/build, \"Build Constraints\"; The Go Programming Language Specification, \"Constant expressions\", \"Array types\", \"Method sets\"",
		files:  manyChoicesFiles(),
		// Checked with go vet: S<i> is 1 << i under t<i> and 0 under !t<i>, so M's length is any of 1..128,
		// each in the builds of one choice; -tags t0,t2 gives [6]byte, so T implements I6; no build gives
		// [200]byte; and [6]byte needs t0, where N is missing, so T never implements I6x.
		want: []string{},
		facts: []string{
			"tt T implements i6 I6",
			"tm M overrides i6 M",
		},
		// Mutation: past 64 choices the signature read over every declaration at once -> its key is
		// unknown and T implements no I6. Mutation: each choice's key under condition 0 rather than the
		// conjunction of its choices -> tt T implements i6x I6x.
		fails: "a signature with more build-dependent spellings than a bound left unknown, or a spelling taken as every build's",
	},
	{
		name:   "per-build-names-of-one-dimension-prune",
		anchor: "go/build, \"Build Constraints\" (_GOARCH file names); The Go Programming Language Specification, \"Constant expressions\", \"Alias declarations\", \"Method sets\"",
		files: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.27\n",
			"t.go": `package p

type T struct{} // @tt

func (T) R() ([PtrSize]byte, Word) { return [PtrSize]byte{}, 0 } // @tr

type A64 interface{ R() ([8]byte, uint64) } // @a64

type A32 interface{ R() ([4]byte, uint32) } // @a32

type Mix interface{ R() ([8]byte, uint32) } // @mix
`,
			"ptr_386.go":   "package p\n\nconst PtrSize = 4\n\ntype Word = uint32\n",
			"ptr_arm.go":   "package p\n\nconst PtrSize = 4\n\ntype Word = uint32\n",
			"ptr_amd64.go": "package p\n\nconst PtrSize = 8\n\ntype Word = uint64\n",
			"ptr_arm64.go": "package p\n\nconst PtrSize = 8\n\ntype Word = uint64\n",
		},
		// Checked with go vet per GOARCH: A64 on amd64 and arm64, A32 on 386 and arm, Mix on none: PtrSize
		// and Word are chosen by one architecture, so no build pairs 8 with uint32.
		want: []string{},
		facts: []string{
			"tt T implements a64 A64",
			"tt T implements a32 A32",
			"tr R overrides a64 R",
			"tr R overrides a32 R",
		},
		// Mutation: a choice no build makes kept (no pruning) -> tt T implements mix Mix.
		fails: "a signature spelled with declarations of two different builds",
	},
	{
		name:   "external-type-structure-per-build",
		anchor: "go/build, \"Build Constraints\" (_GOARCH file names); The Go Programming Language Specification, \"Struct types\" (promoted methods), \"Method sets\"",
		files: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.27\n\nrequire example.com/arch v1.0.0\n",
			"t.go": `package p

import "example.com/arch"

type ER struct{ arch.Regs } // @ter

type EP struct{ arch.Ptrace } // @tep

type PC64 interface{ PC() uint64 } // @ipc

type W interface{ Wide() } // @iw

type N interface{ Narrow() } // @in

type WN interface {
	Wide()
	Narrow()
}
`,
		},
		// Checked with go vet per GOARCH: arch.Regs embeds Lo (Narrow) on 386 and Hi (Wide) on amd64 and has
		// PC on amd64; arch.Ptrace is declared alike on 386, amd64 and arm64 and has PC on amd64 and arm64.
		// So ER implements N (386), PC64 and W (amd64), never WN; EP implements PC64 (amd64, arm64).
		want: []string{},
		facts: []string{
			"ter ER implements ipc PC64",
			"ter ER implements iw W",
			"ter ER implements in N",
			"tep EP implements ipc PC64",
		},
		// Mutation: an external type's condition its first declaration's (386) -> neither ER nor EP
		// implements PC64. Mutation: struct declarations judged the same whatever they embed -> Hi's Wide
		// lost, ER implements no W. Mutation: every declaration's embedded types under condition 0 -> ter ER
		// implements WN.
		fails: "an external type read as its first declaration, or its declarations' structures merged across builds",
	},
	{
		name:   "many-free-tags-decided-exactly",
		anchor: "go/build, \"Build Constraints\"; The Go Programming Language Specification, \"Method sets\", \"Interface types\"",
		files:  manyTagsFiles(),
		// Checked with go vet: -tags t0,...,t16 lacks N, and -tags t1,...,t16 lacks M0, so no build gives T
		// all of I; J's M0 and M1 meet under t0 && t1.
		want: []string{},
		facts: []string{
			"tt T implements ij J",
			"m0 M0 overrides ij0 M0",
			"m1 M1 overrides ij1 M1",
		},
		// Mutation: a condition naming more than 16 free tags counted as met -> tt T implements ii I, though
		// M0 needs t0 and N needs !t0.
		fails: "a method set judged satisfied because its condition names too many tags to decide",
	},
}

// manyTagsFiles is a type T whose methods M0..M16 are each declared under a
// tag of its own, t0..t16, and N under !t0, with I naming all of them and J
// only M0 and M1.
func manyTagsFiles() map[string]string {
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.27\n"}
	t := "package p\n\ntype T struct{} // @tt\n\ntype I interface { // @ii\n"
	for i := range 17 {
		n := strconv.Itoa(i)
		t += "\tM" + n + "()\n"
		m := "//go:build t" + n + "\n\npackage p\n\nfunc (T) M" + n + "() {}"
		if i < 2 {
			m += " // @m" + n
		}
		files["m"+n+".go"] = m + "\n"
	}
	t += "\tN()\n}\n\ntype J interface { // @ij\n\tM0() // @ij0\n\tM1() // @ij1\n}\n"
	files["t.go"] = t
	files["n.go"] = "//go:build !t0\n\npackage p\n\nfunc (T) N() {}\n"
	return files
}

// manyChoicesFiles is a type T whose method M returns [1+S0+...+S6]byte,
// each S<i> declared 1 << i under t<i> and 0 under !t<i>, N declared under
// !t0, I6 naming M of [6]byte, I200 of [200]byte and I6x M of [6]byte and N.
func manyChoicesFiles() map[string]string {
	files := map[string]string{"go.mod": "module example.com/m\n\ngo 1.27\n"}
	n := "1"
	for i := range 7 {
		k := strconv.Itoa(i)
		files["s"+k+"_on.go"] = "//go:build t" + k + "\n\npackage p\n\nconst S" + k + " = 1 << " + k + "\n"
		files["s"+k+"_off.go"] = "//go:build !t" + k + "\n\npackage p\n\nconst S" + k + " = 0\n"
		n += " + S" + k
	}
	files["t.go"] = "package p\n\ntype T struct{} // @tt\n\nfunc (T) M() [" + n + "]byte { return [" + n + "]byte{} } // @tm\n\n" +
		"type I6 interface{ M() [6]byte } // @i6\n\ntype I200 interface{ M() [200]byte }\n\n" +
		"type I6x interface { // @i6x\n\tM() [6]byte // @i6m\n\tN()\n}\n"
	files["n.go"] = "//go:build !t0\n\npackage p\n\nfunc (T) N() {}\n"
	return files
}
