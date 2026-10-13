package link

import (
	"context"
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestProjectionKeepsWhatTheHierarchyReads fills every field of a Java
// table, walking its type so that a field added later is filled too, names
// an anonymous body in a site and in a value alone, and has a routing mount
// pass a module argument. The stored projection must read back with every
// field the hierarchy reads equal to the table's, rank the anonymous bodies
// as the table does, and give the mount the key the table gives it; and the
// mount's argument must be part of the table's interface, since the module
// index reads it.
//
// Mutations that fail it: drop the anonymous body offsets from the stored
// form (the site's body loses its rank); drop the mount's argument from the
// projection (the mount has no key); drop a kept field from the projection
// or its stored form; leave the mount's argument out of the interface (two
// tables whose mounts include different modules share an interface digest,
// so a refresh keeps the stale module index).
func TestProjectionKeepsWhatTheHierarchyReads(t *testing.T) {
	var tab Table
	fill(reflect.ValueOf(&tab).Elem())
	tab.Language = "java"
	tab.Sites[0].Qualifier = "#40"
	tab.Values[0].Type.Name = "#70"
	tab.Supertypes[0].Super = "#90"
	tab.Sites[0].Args[0] = wire.Type{Kind: wire.TypeModule, Name: "routes.users.router"}
	m := &tab.Routing.Mounts[0]
	m.Site, m.Arg, m.Module, m.Target, m.Join, m.Call = 0, 0, "", "", JoinNone, nil

	raw, err := tab.project().encodeProjected()
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeProjected(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := anonymousNames(p), anonymousNames(&tab); !reflect.DeepEqual(got, want) {
		t.Errorf("the projection ranks the anonymous bodies %v, the table %v", got.rank, want.rank)
	}
	gotKey, _, gotOK := mountKey(p, &p.Routing.Mounts[0])
	wantKey, _, wantOK := mountKey(&tab, &tab.Routing.Mounts[0])
	if gotKey != wantKey || gotOK != wantOK || !wantOK {
		t.Errorf("the projection's mount key is %q (%v), the table's %q (%v)", gotKey, gotOK, wantKey, wantOK)
	}
	dropped := map[string]bool{"Sites": true, "Values": true, "Handlers": true, "DynamicImports": true, "Dropped": true,
		"Routing": true}
	tv, pv := reflect.ValueOf(tab), reflect.ValueOf(*p)
	for i := 0; i < tv.NumField(); i++ {
		f := tv.Type().Field(i)
		if !f.IsExported() || dropped[f.Name] {
			continue
		}
		if !reflect.DeepEqual(pv.Field(i).Interface(), tv.Field(i).Interface()) {
			t.Errorf("the projection's %s is not the table's", f.Name)
		}
	}
	if !reflect.DeepEqual(p.Routing.Objects, tab.Routing.Objects) || !reflect.DeepEqual(p.Routing.Routes, tab.Routing.Routes) ||
		!reflect.DeepEqual(p.Routing.Settings, tab.Routing.Settings) {
		t.Error("the projection's routing is not the table's")
	}

	other := tab
	other.Sites = []Site{tab.Sites[0]}
	other.Sites[0].Args = []wire.Type{{Kind: wire.TypeModule, Name: "routes.admin.router"}}
	a, err := tab.InterfaceDigest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := other.InterfaceDigest()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two tables whose mounts pass different modules share an interface digest")
	}
}

// countedTables counts each table read.
type countedTables struct {
	fakeTables
	reads map[string]int
}

func (c countedTables) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	c.reads[fv.Path]++
	return c.fakeTables.Table(ctx, gen, fv)
}

// TestFullBuildReadsEachTableOnce builds the hierarchy of two files, b.js's
// class B extending a.js's A, and counts the table reads: each table is read
// once, and the second pass, which links B under A from the projections
// alone, still finds the subtype.
//
// Mutation that fails it: read the tables again in the second pass (each
// is read twice).
func TestFullBuildReadsEachTableOnce(t *testing.T) {
	a := js("a.js", []Decl{exported(decl(model.NodeClass, "A", -1))}, nil, nil)
	b := js("b.js", []Decl{decl(model.NodeClass, "B", -1)}, nil, nil, wire.Supertype{Type: "B", Super: "A"})
	b.Imports = []Import{{Name: "A", Path: "./a"}}
	u := goldenUnit(t, a, b)
	counted := countedTables{fakeTables: u.tables.(fakeTables), reads: map[string]int{}}
	u.tables = counted
	h, err := buildHierarchy(context.Background(), u, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	for _, p := range []string{"a.js", "b.js"} {
		if n := counted.reads[p]; n != 1 {
			t.Errorf("the full build read %s's table %d times", p, n)
		}
	}
	var subs []Ref
	if err := h.links.lookup(keySub+refKey(Ref{File: 0, Index: 0}), func(e entry) error {
		subs = append(subs, e.Ref)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []Ref{{File: 1, Index: 0}}; !reflect.DeepEqual(subs, want) {
		t.Errorf("A's subtypes are %v, want %v", subs, want)
	}
}
