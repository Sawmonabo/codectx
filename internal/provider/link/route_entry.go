package link

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// Declared entry points: what a project's own manifest names as the code it
// runs, resolved to a declaration or a file of the unit. The manifest
// provider reads no declaration and the structural provider reads one file,
// so the join of a manifest's `module:attr` with a declaration is the
// linker's: it reads the unit's root manifest (as it reads go.mod and
// Cargo.toml for its resolution) and the members' tables.
//
//   - pyproject.toml `[project.scripts]` (the console_scripts group),
//     `[project.gui-scripts]` (gui_scripts) and `[project.entry-points.<g>]`,
//     each `module:attr` or `module` (the entry points specification), the
//     module a member file of the unit can be imported as and the attribute
//     its declaration's qualified name;
//   - package.json `bin` (a path, or a map of command name to path) and
//     `main`, each a member file found as Node's require loads a file (the
//     path, then with .js, .mjs, .cjs, then its index.js);
//   - Cargo.toml `[[bin]]`, its `path`, else src/bin/<name>.rs or
//     src/bin/<name>/main.rs (Cargo's target layout), whose top-level
//     `fn main` it routes to, else the file.
//
// Each is an `endpoint` of the file it resolves to, routed to the
// declaration or the file's module; a name several files can supply gives
// one endpoint in each, with the set size. A value that resolves to no
// member publishes nothing.

// fieldEntry + manifest path + NUL + group + NUL + name is one declared
// entry point, its value as written.
const fieldEntry = "entry\x00"

// entryFields are the unit's declared entry points as manifest fields, so a
// change to one re-resolves the unit (manifestFields).
func entryFields(ctx context.Context, u *unit) ([]manifestField, error) {
	var name string
	switch u.family {
	case FamilyPython:
		name = "pyproject.toml"
	case FamilyJavaScript:
		name = "package.json"
	case FamilyRust:
		name = "Cargo.toml"
	default:
		return nil, nil
	}
	if u.content == nil {
		return nil, nil
	}
	p := path.Join(u.root, name)
	var data []byte
	err := u.content.EachFile(ctx, model.FileSelection{Paths: []string{p}}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || fv.Path != p {
			return nil
		}
		rc, fv, err := u.content.Open(ctx, fv.ID)
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err = io.ReadAll(io.LimitReader(rc, fv.Size))
		return err
	})
	if err != nil || data == nil {
		return nil, err
	}
	var out []manifestField
	add := func(group, name, value string) {
		if name != "" && value != "" {
			out = append(out, manifestField{Key: fieldEntry + p + "\x00" + group + "\x00" + name, Value: value})
		}
	}
	switch name {
	case "pyproject.toml":
		var doc struct {
			Project struct {
				Scripts     map[string]string            `toml:"scripts"`
				GUIScripts  map[string]string            `toml:"gui-scripts"`
				EntryPoints map[string]map[string]string `toml:"entry-points"`
			} `toml:"project"`
		}
		if toml.Unmarshal(data, &doc) != nil {
			return nil, nil
		}
		for n, v := range doc.Project.Scripts {
			add("console_scripts", n, v)
		}
		for n, v := range doc.Project.GUIScripts {
			add("gui_scripts", n, v)
		}
		for g, m := range doc.Project.EntryPoints {
			for n, v := range m {
				add(g, n, v)
			}
		}
	case "package.json":
		var doc struct {
			Name string          `json:"name"`
			Bin  json.RawMessage `json:"bin"`
			Main string          `json:"main"`
		}
		if json.Unmarshal(data, &doc) != nil {
			return nil, nil
		}
		var one string
		var many map[string]string
		switch {
		case json.Unmarshal(doc.Bin, &one) == nil:
			// A string bin is the package's one command, named by the
			// package's name without its scope.
			add("bin", doc.Name[strings.LastIndexByte(doc.Name, '/')+1:], one)
		case json.Unmarshal(doc.Bin, &many) == nil:
			for n, v := range many {
				add("bin", n, v)
			}
		}
		add("main", "main", doc.Main)
	case "Cargo.toml":
		var doc struct {
			Bin []struct {
				Name string `toml:"name"`
				Path string `toml:"path"`
			} `toml:"bin"`
		}
		if toml.Unmarshal(data, &doc) != nil {
			return nil, nil
		}
		for _, b := range doc.Bin {
			add("bin", b.Name, "path:"+b.Path)
		}
	}
	return out, nil
}

// declaredEntry is one declared entry point read back from its field.
type declaredEntry struct {
	manifest, group, name, value string
}

// descriptor is the entry point's group and name, `console_scripts:demo`,
// or the group alone for package.json's one `main`.
func (d declaredEntry) descriptor() string {
	if d.group == d.name {
		return d.group
	}
	return d.group + ":" + d.name
}

// entryPayload is a FactEntry's endpoint (Binding.External).
type entryPayload struct {
	Descriptor string `json:"d"`
	Manifest   string `json:"m"`
	Set        int64  `json:"n,omitempty"`
}

// withEntries is bindings with every FactEntry replaced by the unit's
// declared entry points resolved afresh: each one is read from its manifest
// field and joined to the members' paths (no table is read but those of the
// files it can name), so it costs one pass over the member list and the
// candidates' tables, whatever else the refresh re-derived. prev are the
// predecessor's fields, nil for none: when neither declares an entry point,
// no fact of the kind can be in bindings and they are returned as they are.
func withEntries(ctx context.Context, u *unit, bindings *pagination.SortedRun[idBinding],
	fields, prev []manifestField) (*pagination.SortedRun[idBinding], error) {
	var entries []declaredEntry
	for _, f := range fields {
		rest, ok := strings.CutPrefix(f.Key, fieldEntry)
		if !ok {
			continue
		}
		parts := strings.SplitN(rest, "\x00", 3)
		if len(parts) == 3 {
			entries = append(entries, declaredEntry{manifest: parts[0], group: parts[1], name: parts[2], value: f.Value})
		}
	}
	had := slices.ContainsFunc(prev, func(f manifestField) bool { return strings.HasPrefix(f.Key, fieldEntry) })
	if len(entries) == 0 && !had {
		return bindings, nil
	}
	out, err := newIDBindingSort(u.dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	err = bindings.Each(func(b idBinding) error {
		if b.Kind == FactEntry {
			return nil
		}
		return out.Add(b)
	})
	if err != nil {
		return nil, err
	}
	found, err := resolveEntries(ctx, u, entries)
	if err != nil {
		return nil, err
	}
	for _, b := range found {
		if err := out.Add(b); err != nil {
			return nil, err
		}
	}
	return out.Sorted()
}

// entryCandidate is one member an entry point's value may name, with the
// declaration's qualified name it names there ("" for the file itself).
type entryCandidate struct {
	entry     int
	fv        model.FileVersion
	qualified string
	rustMain  bool
}

// resolveEntries resolves each entry point to its targets.
func resolveEntries(ctx context.Context, u *unit, entries []declaredEntry) ([]idBinding, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	var cands []entryCandidate
	err := u.members(func(fv model.FileVersion) error {
		for i, e := range entries {
			if q, main, ok := entryNames(u, e, fv.Path); ok {
				cands = append(cands, entryCandidate{entry: i, fv: fv, qualified: q, rustMain: main})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	type hit struct {
		entry  int
		fv     model.FileVersion
		target model.NodeID
	}
	var hits []hit
	for _, c := range cands {
		t, has, err := u.tables.Table(ctx, u.gen, c.fv)
		if err != nil {
			return nil, err
		}
		if !has {
			continue
		}
		target := model.NodeID("")
		switch {
		case c.qualified == "":
			target = t.Module.Node
			if c.rustMain {
				for i := range t.Decls {
					d := &t.Decls[i]
					if d.Parent < 0 && d.Kind == model.NodeFunction && d.Name == "main" && !d.Prototype {
						target = d.Node
						break
					}
				}
			}
		default:
			for i := range t.Decls {
				if t.Decls[i].Qualified == c.qualified {
					target = t.Decls[i].Node
					break
				}
			}
		}
		if target != "" {
			hits = append(hits, hit{entry: c.entry, fv: c.fv, target: target})
		}
	}
	sets := map[int]int64{}
	for _, h := range hits {
		sets[h.entry]++
	}
	out := make([]idBinding, 0, len(hits))
	for _, h := range hits {
		e := entries[h.entry]
		p := entryPayload{Descriptor: e.descriptor(), Manifest: e.manifest}
		if n := sets[h.entry]; n >= 2 {
			p.Set = n
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, internalErr("entry payload: " + err.Error())
		}
		out = append(out, idBinding{Kind: FactEntry, SiteFile: h.fv.ID, TargetFile: h.fv.ID, Target: h.target,
			External: string(raw), Detail: DetailImport})
	}
	return out, nil
}

// entryNames reports whether the member at p is a file entry point e may
// name, with the qualified name of the declaration it names there ("" for
// the file) and, for a Rust binary, that its `fn main` is the target.
func entryNames(u *unit, e declaredEntry, p string) (string, bool, bool) {
	dir := path.Dir(e.manifest)
	switch u.family {
	case FamilyPython:
		mod, attr, _ := strings.Cut(e.value, ":")
		mod, attr = strings.TrimSpace(mod), strings.TrimSpace(attr)
		if i := strings.IndexAny(attr, " ["); i >= 0 {
			// `module:attr [extra]`: the extras name no declaration.
			attr = strings.TrimSpace(attr[:i])
		}
		if !strings.HasSuffix(p, ".py") || dir != "." && !strings.HasPrefix(p, dir+"/") {
			return "", false, false
		}
		rel := strings.TrimPrefix(p, dir+"/")
		if dir == "." {
			rel = p
		}
		// The module is imported from the project's root or, in the src
		// layout, its src directory.
		found := false
		_ = pythonModuleNames(rel, func(name, root string) error {
			found = found || name == mod && (root == "" || root == "src/")
			return nil
		})
		return attr, false, found
	case FamilyJavaScript:
		v := path.Clean(path.Join(dir, e.value))
		for _, c := range []string{v, v + ".js", v + ".mjs", v + ".cjs", v + "/index.js"} {
			if p == c {
				return "", false, true
			}
		}
	case FamilyRust:
		given := strings.TrimPrefix(e.value, "path:")
		var want []string
		if given != "" {
			want = []string{path.Clean(path.Join(dir, given))}
		} else {
			want = []string{path.Join(dir, "src/bin", e.name+".rs"), path.Join(dir, "src/bin", e.name, "main.rs")}
		}
		if slices.Contains(want, p) {
			return "", true, true
		}
	}
	return "", false, false
}

// entries publishes the declared entry points the FactEntries of this file,
// group, found: one `endpoint` per distinct entry point, in the file's
// scope, keyed by its descriptor and manifest, located at its target,
// defined by the file's module and routed to the target, with the set size
// when several files supply it.
func (f *filePublication) entries(group []idBinding) error {
	byPayload := map[string][]idBinding{}
	var order []string
	for _, b := range group {
		if b.Kind != FactEntry {
			continue
		}
		if _, seen := byPayload[b.External]; !seen {
			order = append(order, b.External)
		}
		byPayload[b.External] = append(byPayload[b.External], b)
	}
	sort.Strings(order)
	byNode := targetable(f.t)
	for _, raw := range order {
		var p entryPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return internalErr("entry payload: " + err.Error())
		}
		for _, b := range byPayload[raw] {
			target, ok := byNode[b.Target]
			if !ok {
				return internalErr("a declared entry point targets node " + string(b.Target) + ", which the link table of " +
					f.fv.Path + " does not declare")
			}
			n, err := f.mint(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + f.fv.Path,
				NativeKey: fitKey("endpoint:declared:" + p.Descriptor + "@" + p.Manifest), Kind: model.NodeEndpoint,
				Language: f.t.Language, Name: nodeName(p.Descriptor, "endpoint")}, target.Range, []byte("{}"))
			if err != nil {
				return err
			}
			f.node(occurrenceOf(f.fv, f.t, f.t.Module))
			f.relation(f.t.Module.Node, model.RelDefines, n, located(target.Range), "", "", "", 0)
			f.relation(n, model.RelRoutesTo, b.Target, located(target.Range), "", b.Detail, "", p.Set)
		}
	}
	return nil
}
