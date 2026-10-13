package link

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
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

// TestShiftedGoMountKeepsRoutingDigests is the same for a Go function that
// passes the router it receives on to another package's function: the
// call's site moves, and with it the site index the passed router's type
// names. Mutation caught: the site kept in the projection, or the range in
// the routing payload (the hierarchy entry moves), or the argument's type
// hashed in the interface (the digest moves).
func TestShiftedGoMountKeepsRoutingDigests(t *testing.T) {
	table := func(shift int) *Table {
		tab := &Table{Path: "api/api.go", Language: "go",
			Decls: []Decl{{Endpoint: Endpoint{Node: "n1", Name: "Register", Qualified: "api.Register"}, Parent: -1}}}
		for range shift {
			tab.Sites = append(tab.Sites, Site{Caller: 0, Callee: "setup"})
		}
		group := len(tab.Sites)
		tab.Sites = append(tab.Sites, Site{Caller: 0, Callee: "Group"})
		tab.Sites = append(tab.Sites, Site{Caller: 0, Callee: "Register", Shape: wire.ShapeStatic, Qualifier: "users",
			Described: true, Args: []wire.Type{{Kind: wire.TypeResult, Index: uint32(group)}}})
		call, ok := CallOf(&tab.Sites[group+1])
		if !ok {
			t.Fatal("the call was not carried")
		}
		tab.Routing = Routing{
			Objects: []RouteObject{{Decl: 0, Framework: "gin", OwnKnown: true, Param: 1}},
			Mounts: []RouteMount{{Site: int32(group + 1), Arg: 0, Framework: "gin", PrefixKnown: true, Object: 0,
				Base: "/users", BaseKnown: true, Join: JoinArgument, Call: call,
				Range: model.SourceRange{Start: model.Position{Byte: uint64(40 + 10*shift)}}}},
		}
		return tab
	}
	entries := func(tab *Table) []entry {
		var out []entry
		if err := addGoRouting(sink(func(e entry) error { out = append(out, e); return nil }), 0, tab.project()); err != nil {
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
		t.Errorf("the Go routing's hierarchy entries moved with the mount's site:\nbefore %v\nafter  %v", want, got)
	}
}

// sink adds each entry to a function.
type sink func(entry) error

func (s sink) Add(e entry) error { return s(e) }
