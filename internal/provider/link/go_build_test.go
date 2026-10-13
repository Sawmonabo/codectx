package link

import (
	"context"
	"strconv"
	"testing"
)

// buildConsts is a package whose constants are each declared once per
// build: by name, each declaration's form and the constraint of its file.
type buildConsts struct {
	c     *goConds
	names goNames
	decls map[string][][2]string
}

func (t buildConsts) lookup(context.Context, string, string) (goTypeAnswer, bool, error) {
	return goTypeAnswer{}, true, nil
}

func (t buildConsts) constant(_ context.Context, _, name string) ([]goDecl, error) {
	var out []goDecl
	for _, d := range t.decls[name] {
		out = append(out, goDecl{form: d[0], names: t.names, cond: t.c.parse(d[1])})
	}
	return out, nil
}

// TestGoMethodKeysMergeLeavesByKey checks a signature naming two constants
// declared per build is one record per distinct key, each under the builds
// of every choice that spells it: A is 1 under t0 and 2 under !t0, B is 0
// under t1 and 1 under !t1, so [A+B]byte is 1, 2 (twice) or 3. Mutation:
// leaves merged only when every leaf spells one key -> four records, key 2
// twice. Mutation: a merged key under one choice's condition alone -> key
// 2's condition misses the builds of the other.
func TestGoMethodKeysMergeLeavesByKey(t *testing.T) {
	h := &hierarchy{goConds: newGoConds("1.27")}
	c := h.goConds
	types := buildConsts{c: c, names: fakeNames{path: "example.com/m"}, decls: map[string][][2]string{
		"A": {{"1", "t0"}, {"2", "!t0"}},
		"B": {{"0", "t1"}, {"1", "!t1"}},
	}}
	got := map[string]goCond{}
	n := 0
	err := h.goMethodKeys(context.Background(), types, types.names, "M", "( ) ( [# ( .A + .B ) ] .byte )", Ref{},
		func(key string, _ goSpelling, cond goCond) error {
			n++
			got[key] = cond
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]goCond{
		"M\x00( ) ( [ 1 ] uint8 )": c.parse("t0 && t1"),
		"M\x00( ) ( [ 2 ] uint8 )": c.parse("(t0 && !t1) || (!t0 && t1)"),
		"M\x00( ) ( [ 3 ] uint8 )": c.parse("!t0 && !t1"),
	}
	if n != len(want) || len(got) != len(want) {
		t.Fatalf("%d records of keys %v, want one per distinct key %d", n, got, len(want))
	}
	for k, cond := range want {
		if got[k] != cond {
			t.Errorf("key %q under condition %d, want %d", k, got[k], cond)
		}
	}
}

// buildAliases is a package whose aliases are each declared once per
// build: by name, each declaration's form and the constraint of its file.
type buildAliases struct {
	c     *goConds
	names goNames
	decls map[string][][2]string
}

func (t buildAliases) lookup(_ context.Context, _, name string) (goTypeAnswer, bool, error) {
	ds, ok := t.decls[name]
	if !ok {
		return goTypeAnswer{}, true, nil
	}
	a := goTypeAnswer{found: true, alias: true, form: ds[0][0], names: t.names, cond: t.c.parse(ds[0][1])}
	for _, d := range ds[1:] {
		a.others = append(a.others, goDecl{form: d[0], names: t.names, cond: t.c.parse(d[1])})
	}
	return a, true, nil
}

func (t buildAliases) constant(context.Context, string, string) ([]goDecl, error) { return nil, nil }

// BenchmarkGoMethodKeysEarlyJoin walks a signature naming 70 aliases, each
// written two ways per operating system that spell one type: one key and
// one walk node, where forking on the written forms would make 2^70.
func BenchmarkGoMethodKeysEarlyJoin(b *testing.B) {
	h := &hierarchy{goConds: newGoConds("1.27")}
	types := buildAliases{c: h.goConds, names: fakeNames{path: "example.com/m"}, decls: map[string][][2]string{
		"Count": {{".int", ""}},
	}}
	form := "("
	for i := range 70 {
		name := "A" + strconv.Itoa(i)
		types.decls[name] = [][2]string{{".Count", "linux"}, {".int", "!linux"}}
		if i > 0 {
			form += " ,"
		}
		form += " ." + name
	}
	form += " ) ( )"
	b.ReportAllocs()
	for b.Loop() {
		n := 0
		err := h.goMethodKeys(context.Background(), types, types.names, "M", form, Ref{},
			func(string, goSpelling, goCond) error {
				n++
				return nil
			})
		if err != nil || n != 1 {
			b.Fatalf("%d keys, %v", n, err)
		}
	}
}
