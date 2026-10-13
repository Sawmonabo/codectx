package link

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestShiftedMountKeepsRoutingDigests is a Python file whose routing mount
// moves to a later site (a call inserted before it): its interface digest
// and the module index entries its routing adds stay the same, since no
// reader reads the mount's site or argument index. Mutation caught: the
// site and argument indices kept in the interface's routing (the digest
// moves) or in the routing payload (the index entry moves).
func TestShiftedMountKeepsRoutingDigests(t *testing.T) {
	table := func(shift int) *Table {
		arg := wire.Type{Kind: wire.TypeModule, Name: "users.router"}
		tab := &Table{Path: "app/main.py", Language: "python",
			Decls: []Decl{{Endpoint: Endpoint{Node: "n1", Name: "api", Qualified: "api"}, Parent: -1}}}
		for range shift {
			tab.Sites = append(tab.Sites, Site{Caller: -1, Callee: "setup"})
		}
		tab.Sites = append(tab.Sites, Site{Caller: -1, Callee: "include_router", Args: []wire.Type{{}, arg}})
		tab.Routing = Routing{
			Objects: []RouteObject{{Decl: 0, Framework: "fastapi", OwnKnown: true}},
			Mounts: []RouteMount{{Site: int32(shift), Arg: 1, Framework: "fastapi", Prefix: "/u", PrefixKnown: true,
				Object: 0, BaseKnown: true}},
		}
		return tab
	}
	entries := func(tab *Table) []entry {
		var out []entry
		if err := indexPythonRouting(0, tab, func(e entry) error { out = append(out, e); return nil }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before, after := table(0), table(2)
	was, err := before.InterfaceDigest()
	if err != nil {
		t.Fatal(err)
	}
	now, err := after.InterfaceDigest()
	if err != nil {
		t.Fatal(err)
	}
	if was != now {
		t.Errorf("the interface digest moved when the mount only moved to a later site")
	}
	if got, want := entries(after), entries(before); len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("the routing's module index entries moved with the mount's site:\nbefore %v\nafter  %v", want, got)
	}
}
