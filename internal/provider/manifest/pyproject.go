package manifest

import (
	"context"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	ecosystemPyPI  = "pypi"
	languagePython = "python"
)

// pyproject reads a pyproject.toml: PEP 621 [project] metadata and
// dependencies, optional-dependency extras, PEP 735 dependency groups,
// build-system requirements and, when present, Poetry's tables. A field
// listed under `dynamic` is explicitly unresolved and is not invented.
func (u *unit) pyproject(ctx context.Context) error {
	var doc struct {
		Project *struct {
			Name                 string              `toml:"name"`
			Version              string              `toml:"version"`
			Dynamic              []string            `toml:"dynamic"`
			RequiresPython       string              `toml:"requires-python"`
			Dependencies         []string            `toml:"dependencies"`
			OptionalDependencies map[string][]string `toml:"optional-dependencies"`
		} `toml:"project"`
		BuildSystem *struct {
			Requires     []string `toml:"requires"`
			BuildBackend string   `toml:"build-backend"`
		} `toml:"build-system"`
		DependencyGroups map[string][]any `toml:"dependency-groups"`
		Tool             struct {
			Poetry *struct {
				Name            string         `toml:"name"`
				Version         string         `toml:"version"`
				Dependencies    map[string]any `toml:"dependencies"`
				DevDependencies map[string]any `toml:"dev-dependencies"`
				Group           map[string]struct {
					Dependencies map[string]any `toml:"dependencies"`
				} `toml:"group"`
			} `toml:"poetry"`
		} `toml:"tool"`
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
		u.degraded(BoundTOMLLines, "layout not built; facts carry no range")
	}
	poetry := doc.Tool.Poetry
	var name, version string
	meta := map[string]any{}
	nameKey := span{}
	nameFound := false
	switch {
	case doc.Project != nil && doc.Project.Name != "":
		name, version = doc.Project.Name, doc.Project.Version
		if len(doc.Project.Dynamic) > 0 {
			meta["dynamic"] = doc.Project.Dynamic
		}
		if doc.Project.RequiresPython != "" {
			meta["requires_python"] = doc.Project.RequiresPython
		}
		nameKey, nameFound = layout.key(u.data, "project", "name")
	case poetry != nil && poetry.Name != "":
		name, version = poetry.Name, poetry.Version
		nameKey, nameFound = layout.key(u.data, "tool.poetry", "name")
	default:
		u.malformed()
		return nil
	}
	if version != "" {
		meta["version"] = version
	}
	if doc.BuildSystem != nil && doc.BuildSystem.BuildBackend != "" {
		meta["build_backend"] = doc.BuildSystem.BuildBackend
	}
	pkg, err := u.defines(ctx, model.NodePackage, ecosystemPyPI+":"+normalizePyPI(name), name, languagePython, u.rangeOf(nameKey, nameFound), meta)
	if err != nil {
		return err
	}
	total := 0
	// requirements emits PEP 508 strings; the range of each is its quoted
	// occurrence inside the located array.
	requirements := func(table, key string, list []string, kind string, extra ...string) error {
		arr, found := layout.keyValue(u.data, table, key)
		for _, req := range list {
			if total++; u.cut(BoundDependencies, u.deps, int64(total)) {
				return nil
			}
			depName, spec := splitPEP508(req)
			var rng *model.SourceRange
			if found {
				rng = u.rangeOf(quoted(u.data, arr, req))
			}
			if err := u.edge(ctx, pkg, model.RelDependsOn, ecosystemPyPI, normalizePyPI(depName), languagePython, kind, spec, rng, extra...); err != nil {
				return err
			}
		}
		return nil
	}
	if doc.Project != nil {
		if err := requirements("project", "dependencies", doc.Project.Dependencies, KindRuntime); err != nil {
			return err
		}
		for _, extra := range sortedKeys(doc.Project.OptionalDependencies) {
			if err := requirements("project.optional-dependencies", extra, doc.Project.OptionalDependencies[extra], KindOptional, "extra", extra); err != nil {
				return err
			}
		}
	}
	for _, group := range sortedKeys(doc.DependencyGroups) {
		var list []string
		for _, entry := range doc.DependencyGroups[group] {
			if s, ok := entry.(string); ok {
				list = append(list, s)
			}
			// {include-group = "..."} entries reference other groups; the
			// included group's own entries are already emitted under it.
		}
		if err := requirements("dependency-groups", group, list, KindDev, "group", group); err != nil {
			return err
		}
	}
	if doc.BuildSystem != nil {
		if err := requirements("build-system", "requires", doc.BuildSystem.Requires, KindBuild); err != nil {
			return err
		}
	}
	if poetry == nil {
		return nil
	}
	poetryDeps := func(table string, deps map[string]any, kind string, extra ...string) error {
		for _, n := range sortedKeys(deps) {
			if n == "python" {
				continue
			}
			if total++; u.cut(BoundDependencies, u.deps, int64(total)) {
				return nil
			}
			spec := asString(deps[n])
			if m, ok := deps[n].(map[string]any); ok {
				spec = asString(m["version"])
			}
			if err := u.edge(ctx, pkg, model.RelDependsOn, ecosystemPyPI, normalizePyPI(n), languagePython, kind, spec, u.rangeOf(layout.key(u.data, table, n)), extra...); err != nil {
				return err
			}
		}
		return nil
	}
	if err := poetryDeps("tool.poetry.dependencies", poetry.Dependencies, KindRuntime); err != nil {
		return err
	}
	if err := poetryDeps("tool.poetry.dev-dependencies", poetry.DevDependencies, KindDev); err != nil {
		return err
	}
	for _, group := range sortedKeys(poetry.Group) {
		if err := poetryDeps("tool.poetry.group."+group+".dependencies", poetry.Group[group].Dependencies, KindDev, "group", group); err != nil {
			return err
		}
	}
	return nil
}

// splitPEP508 separates a requirement string into the project name and the
// rest (extras, specifier, markers). Only the name grammar is applied; the
// rest is carried verbatim.
func splitPEP508(req string) (name, spec string) {
	req = strings.TrimSpace(req)
	i := 0
	for i < len(req) {
		c := req[i]
		if c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			i++
			continue
		}
		break
	}
	return req[:i], strings.TrimSpace(req[i:])
}

// normalizePyPI applies PEP 503 name normalization.
func normalizePyPI(name string) string {
	var b strings.Builder
	prevSep := false
	for _, r := range strings.ToLower(name) {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				b.WriteByte('-')
			}
			prevSep = true
			continue
		}
		prevSep = false
		b.WriteRune(r)
	}
	return b.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
