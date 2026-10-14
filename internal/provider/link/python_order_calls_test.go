package link_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestPythonCallsReadTheFullOrder protects what a Python call publishes when
// the method resolution order places a base outside the project ahead of the
// class of the project it binds ("The Python 2.3 Method Resolution Order":
// attributes resolve along it): the base may hold the method instead, so
// the call reaches that base's method and the class's, a set of 2; a base
// after the class is no candidate. `super().run()` in Q reads, for each
// class below Q, the order past Q: S places Base between Q and P.
//
// Mutation: bind the first class of the project along the order alone (the
// order without the bases outside) -> use_c and Q.run reach U.run or P.run
// alone. Mutation: place the outside bases first whatever the order ->
// use_d also reaches lib.Base.run. Mutation: start super()'s walk at Q
// itself -> Q.run's call reaches Q.run.
func TestPythonCallsReadTheFullOrder(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"app.py": `from lib import Base


class U:
    def run(self):
        pass


class C(Base, U):
    pass


class D(U, Base):
    pass


def use_c(c: C):
    c.run()


def use_d(d: D):
    d.run()


class P:
    def run(self):
        pass


class Q(P):
    def run(self):
        super().run()


class S(Q, Base, P):
    pass
`,
	})
	type call struct {
		from, to string
		set      int64
	}
	var got []call
	for _, r := range l.out.rels {
		// The run calls only (`super()` itself is a call too).
		if r.Relation.Kind != model.RelCalls || len(r.Evidence) == 0 ||
			!strings.HasSuffix(l.out.nodes[r.Relation.To].QualifiedName, ".run") {
			continue
		}
		got = append(got, call{l.out.nodes[r.Relation.From].QualifiedName, l.out.nodes[r.Relation.To].QualifiedName,
			r.Evidence[0].SetSize})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i].from != got[j].from {
			return got[i].from < got[j].from
		}
		return got[i].to < got[j].to
	})
	want := []call{
		{"Q.run", "P.run", 2},
		{"Q.run", "lib.Base.run", 2},
		{"use_c", "U.run", 2},
		{"use_c", "lib.Base.run", 2},
		{"use_d", "U.run", 0},
	}
	if len(got) != len(want) {
		t.Fatalf("calls %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %+v, want %+v", got[i], want[i])
		}
	}
}
