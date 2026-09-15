package manifest

import (
	"context"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	ecosystemCargo = "cargo"
	languageRust   = "rust"
)

// cargoDep is one dependency entry: a bare requirement string or a table.
type cargoDep struct {
	requirement string
	inherited   bool // `workspace = true`: the version lives in the workspace root
	optional    bool
	path        string
	git         string
}

func decodeCargoDep(v any) cargoDep {
	switch t := v.(type) {
	case string:
		return cargoDep{requirement: t}
	case map[string]any:
		d := cargoDep{requirement: asString(t["version"]), path: asString(t["path"]), git: asString(t["git"])}
		d.inherited, _ = t["workspace"].(bool)
		d.optional, _ = t["optional"].(bool)
		return d
	}
	return cargoDep{}
}

// inherits reports whether a [package] field is written `{ workspace = true }`.
func inherits(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	w, _ := m["workspace"].(bool)
	return w
}

// cargo reads a Cargo.toml: a package with its dependency tables by kind, a
// workspace with its members (globs stay unexpanded) and its shared
// dependency catalog. A field written `workspace = true` is explicitly
// inherited and stays unresolved here, because resolving it would read the
// workspace root, which is not this unit's input.
func (u *unit) cargo(ctx context.Context) error {
	var doc struct {
		Package   map[string]any `toml:"package"`
		Workspace *struct {
			Members      []string       `toml:"members"`
			Dependencies map[string]any `toml:"dependencies"`
			Package      map[string]any `toml:"package"`
		} `toml:"workspace"`
		Dependencies      map[string]any `toml:"dependencies"`
		DevDependencies   map[string]any `toml:"dev-dependencies"`
		BuildDependencies map[string]any `toml:"build-dependencies"`
		Target            map[string]struct {
			Dependencies      map[string]any `toml:"dependencies"`
			DevDependencies   map[string]any `toml:"dev-dependencies"`
			BuildDependencies map[string]any `toml:"build-dependencies"`
		} `toml:"target"`
	}
	// The pinned decoder has no zero-copy path (parse takes a string,
	// toml@v1.6.0 parse.go:32), but Unmarshal accepts the bytes directly
	// and avoids the extra copy a string conversion would add.
	if err := toml.Unmarshal(u.data, &doc); err != nil {
		u.malformed()
		return nil
	}
	layout, overLines := layoutTOML(u.data)
	if overLines {
		// The layout was not built, so every fact of this manifest is
		// published without a range. That is a cut, and it is reported.
		u.overBound()
	}
	rel := u.e.File().Path
	var owner model.Node
	switch {
	case doc.Package != nil && asString(doc.Package["name"]) != "":
		name := asString(doc.Package["name"])
		meta := map[string]any{}
		var inherited []string
		for _, field := range []string{"version", "edition", "rust-version", "license", "description"} {
			switch v := doc.Package[field]; {
			case inherits(v):
				inherited = append(inherited, field)
			case asString(v) != "":
				meta[field] = asString(v)
			}
		}
		if len(inherited) > 0 {
			meta["inherited"] = inherited
		}
		if doc.Workspace != nil {
			meta["workspace_members"] = doc.Workspace.Members
		}
		node, err := u.defines(ctx, model.NodePackage, ecosystemCargo+":"+name, name, languageRust, u.rangeOf(layout.key(u.data, "package", "name")), meta)
		if err != nil {
			return err
		}
		owner = node
	case doc.Workspace != nil:
		// A virtual manifest: only a workspace.
		node, err := u.defines(ctx, model.NodeConfiguration, ecosystemCargo+":workspace:"+rel, "workspace", languageRust,
			u.rangeOf(layout.section("workspace")), map[string]any{"workspace_members": doc.Workspace.Members})
		if err != nil {
			return err
		}
		owner = node
	default:
		u.malformed()
		return nil
	}
	total := 0
	emit := func(table string, deps map[string]any, kind string, extra ...string) error {
		names := make([]string, 0, len(deps))
		for n := range deps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if total++; total > MaxDependencies {
				u.overBound()
				return nil
			}
			d := decodeCargoDep(deps[n])
			k := kind
			if d.optional {
				k = KindOptional
			}
			detail := append([]string{}, extra...)
			if d.inherited {
				detail = append(detail, "inherited", "workspace")
			}
			detail = append(detail, "path", d.path, "git", d.git)
			if err := u.edge(ctx, owner, model.RelDependsOn, ecosystemCargo, n, languageRust, k, d.requirement, u.rangeOf(layout.key(u.data, table, n)), detail...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := emit("dependencies", doc.Dependencies, KindRuntime); err != nil {
		return err
	}
	if err := emit("dev-dependencies", doc.DevDependencies, KindDev); err != nil {
		return err
	}
	if err := emit("build-dependencies", doc.BuildDependencies, KindBuild); err != nil {
		return err
	}
	targets := make([]string, 0, len(doc.Target))
	for t := range doc.Target {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	for _, t := range targets {
		tables := doc.Target[t]
		prefix := "target." + t + "."
		if err := emit(prefix+"dependencies", tables.Dependencies, KindRuntime, "target", t); err != nil {
			return err
		}
		if err := emit(prefix+"dev-dependencies", tables.DevDependencies, KindDev, "target", t); err != nil {
			return err
		}
		if err := emit(prefix+"build-dependencies", tables.BuildDependencies, KindBuild, "target", t); err != nil {
			return err
		}
	}
	if doc.Workspace != nil {
		if len(doc.Workspace.Members) > MaxEntries {
			u.overBound()
		}
		// The workspace catalog declares versions members inherit; it is a
		// dependency declaration of the workspace itself.
		if err := emit("workspace.dependencies", doc.Workspace.Dependencies, KindRuntime, "declared", "workspace"); err != nil {
			return err
		}
	}
	return nil
}
