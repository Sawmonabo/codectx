package link_test

import (
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// externalOverride is one `overrides` into a method outside the project:
// the method it is from, the one it reaches, its technique and set size.
type externalOverride struct {
	from, to, detail string
	set              int64
}

func externalOverrideEdges(t *testing.T, files map[string]string) []externalOverride {
	t.Helper()
	l := linkSources(t, newStructural(t), files)
	var out []externalOverride
	for _, r := range l.out.rels {
		to := l.out.nodes[r.Relation.To]
		if r.Relation.Kind != model.RelOverrides || nodeResolution(to) != model.ResolutionExternal {
			continue
		}
		if len(r.Evidence) != 1 {
			t.Fatalf("the override into %s carries %d occurrences, want one", to.QualifiedName, len(r.Evidence))
		}
		from := l.out.nodes[r.Relation.From]
		out = append(out, externalOverride{from: from.QualifiedName, to: to.QualifiedName,
			detail: r.Evidence[0].Detail, set: r.Evidence[0].SetSize})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].from != out[j].from {
			return out[i].from < out[j].from
		}
		return out[i].to < out[j].to
	})
	return out
}

// TestJavaOverrideOfSeveralExternalSupertypes protects what an `@Override`
// publishes when supertypes outside the project may declare the method: a
// method overrides every supertype's of its signature at once (JLS
// §8.4.8.1) and the mark proves at least one (§9.6.4.4), so with two
// external candidates each is an edge of the set "at least one of 2"; with
// one, that one is proven; and beside an override of the project's own, the
// walk stops at the project's type that declares the method, and the
// external candidate left is still published.
//
// Mutation: publish only a single external supertype (the rule before sets)
// -> C.run publishes nothing. Mutation: walk past a type of the project that
// declares the method -> D.go also reaches lib.Base.go. Mutation: publish no
// external candidate when a target of the project exists -> D.go reaches
// nothing outside. Mutation: give the set size 1 to a lone candidate -> E.run
// carries set 1.
func TestJavaOverrideOfSeveralExternalSupertypes(t *testing.T) {
	got := externalOverrideEdges(t, map[string]string{
		"C.java": `import lib.Base;
import lib.Task;

class C extends Base implements Task {
  @Override
  public void run() {}
}

class Mid extends Base {
  public void go() {}
}

class D extends Mid implements Task {
  @Override
  public void go() {}
}

class E implements Task {
  @Override
  public void run() {}
}
`,
	})
	want := []externalOverride{
		{"C.run", "lib.Base.run", link.DetailSupertypeCandidate, 2},
		{"C.run", "lib.Task.run", link.DetailSupertypeCandidate, 2},
		{"D.go", "lib.Task.go", link.DetailSupertypeCandidate, 0},
		{"E.run", "lib.Task.run", link.DetailSupertype, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("external overrides %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("external override %+v, want %+v", got[i], want[i])
		}
	}
}

// TestTypeScriptOverrideReachesTheExtendedClass protects that a TypeScript
// `override` proves a member of the class `extends` names (TS4112, TS4113),
// never of an interface `implements` lists: B.draw overrides lib.Base.draw
// alone.
//
// Mutation: read an `implements` interface as a candidate -> B.draw also
// reaches shapes.Shape.draw, a set of 2.
func TestTypeScriptOverrideReachesTheExtendedClass(t *testing.T) {
	got := externalOverrideEdges(t, map[string]string{
		"b.ts": `import { Base } from "lib";
import { Shape } from "shapes";

class B extends Base implements Shape {
    override draw(): void {}
}
`,
	})
	want := []externalOverride{{"B.draw", "lib.Base.draw", link.DetailSupertype, 0}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("external overrides %+v, want %+v", got, want)
	}
}

// TestPythonOverrideOfSeveralExternalBases protects what `typing.override`
// publishes when no class of the project along the method resolution order
// defines the method: the mark proves the first ancestor along the order
// that defines it (PEP 698), one of the bases outside the project, so each
// is an edge of a set of their number; with one base, that one alone.
//
// Mutation: publish only a single external base (the rule before sets) ->
// C.run publishes nothing. Mutation: give a lone base the set size 1 ->
// D.go carries set 1.
func TestPythonOverrideOfSeveralExternalBases(t *testing.T) {
	got := externalOverrideEdges(t, map[string]string{
		"app.py": `from typing import override

from lib import Base
from other import Mixin


class C(Base, Mixin):
    @override
    def run(self):
        pass


class D(Base):
    @override
    def go(self):
        pass
`,
	})
	want := []externalOverride{
		{"C.run", "lib.Base.run", link.DetailSupertype, 2},
		{"C.run", "other.Mixin.run", link.DetailSupertype, 2},
		{"D.go", "lib.Base.go", link.DetailSupertype, 0},
	}
	if len(got) != len(want) {
		t.Fatalf("external overrides %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("external override %+v, want %+v", got[i], want[i])
		}
	}
}

// TestPythonOverrideFollowsTheFullOrder protects where the bases outside the
// project stand in the method resolution order an override is read along:
// the C3 linearisation places each base outside the project as the
// linearisation of itself ("The Python 2.3 Method Resolution Order"), and
// `typing.override` proves the first ancestor along it that defines the
// method (PEP 698). A base outside the project ahead of the first class of
// the project defining it may be that ancestor, so it and that class form
// one set; one after it is not a candidate.
//
// Mutation: read the bases outside the project only when no class of the
// project defines the method (the order without them) -> C.run and E.run
// reach U.run alone. Mutation: keep each class's external bases in written
// order after the project's classes -> E.run misses lib.Base.run, which C3
// places ahead of U (E, M, Base, U). Mutation: place the external bases
// first whatever the order -> D.run gains lib.Base.run. Mutation: count
// only the external edges in the set -> C.run's edges carry set 0.
func TestPythonOverrideFollowsTheFullOrder(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"app.py": `from typing import override

from lib import Base


class U:
    def run(self):
        pass


class C(Base, U):
    @override
    def run(self):
        pass


class D(U, Base):
    @override
    def run(self):
        pass


class M(Base):
    pass


class E(M, U):
    @override
    def run(self):
        pass
`,
	})
	var got []externalOverride
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelOverrides || len(r.Evidence) != 1 {
			continue
		}
		got = append(got, externalOverride{from: l.out.nodes[r.Relation.From].QualifiedName,
			to: l.out.nodes[r.Relation.To].QualifiedName, detail: r.Evidence[0].Detail, set: r.Evidence[0].SetSize})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].from != got[j].from {
			return got[i].from < got[j].from
		}
		return got[i].to < got[j].to
	})
	want := []externalOverride{
		{"C.run", "U.run", link.DetailSupertype, 2},
		{"C.run", "lib.Base.run", link.DetailSupertype, 2},
		{"D.run", "U.run", link.DetailSupertype, 0},
		{"E.run", "U.run", link.DetailSupertype, 2},
		{"E.run", "lib.Base.run", link.DetailSupertype, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("overrides %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("override %+v, want %+v", got[i], want[i])
		}
	}
}
