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
