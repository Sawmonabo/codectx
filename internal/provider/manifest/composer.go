package manifest

import (
	"context"
	"encoding/json"
	"path"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	ecosystemComposer = "composer"
	languagePHP       = "php"
)

// composerSections maps a composer.json link table to its kind.
var composerSections = []struct{ key, kind string }{
	{"require", KindRuntime},
	{"require-dev", KindDev},
}

// composerJSON reads a composer.json with the standard decoder: the package
// name, version and type, and the `require` and `require-dev` tables. PHP
// code itself is not parsed; the dependency facts need no PHP grammar.
// Platform requirements (`php`, `ext-*`, `lib-*`, the Composer APIs) are
// dependencies like any other and carry `platform: true`. Package names are
// case-insensitive in Composer, so identity is the lowercased name.
func (u *unit) composerJSON(ctx context.Context) error {
	var doc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(u.data, &doc); err != nil {
		u.malformed()
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(u.data, &members); err != nil {
		u.malformed()
		return nil
	}
	sections := map[string]map[string]json.RawMessage{}
	for _, sec := range composerSections {
		raw, ok := members[sec.key]
		if !ok {
			continue
		}
		var deps map[string]json.RawMessage
		if err := json.Unmarshal(raw, &deps); err != nil {
			// A link table that is not an object is one unusable entry, not
			// a malformed manifest.
			u.malformedEntry()
			continue
		}
		sections[sec.key] = deps
	}
	keys := make([]string, len(composerSections))
	for i, sec := range composerSections {
		keys[i] = sec.key
	}
	spans, err := jsonSpans(u.data, keys)
	if err != nil {
		u.malformed()
		return nil
	}
	rel := u.e.File().Path
	name, qualified := doc.Name, ecosystemComposer+":"+strings.ToLower(doc.Name)
	if name == "" {
		// A root project need not be named; it is identified by where it
		// lives.
		name, qualified = path.Base(path.Dir(rel)), ecosystemComposer+":"+rel
		if path.Dir(rel) == "." {
			name = "."
		}
	}
	meta := map[string]any{}
	if doc.Version != "" {
		meta["version"] = doc.Version
	}
	if doc.Type != "" {
		meta["type"] = doc.Type
	}
	var nameRange *model.SourceRange
	if s, ok := spans.top["name"]; ok {
		nameRange = u.rng(s.start, s.end)
	}
	node, err := u.defines(ctx, model.NodePackage, qualified, name, languagePHP, nameRange, meta)
	if err != nil {
		return err
	}
	total := 0
	for _, sec := range composerSections {
		deps := sections[sec.key]
		names := make([]string, 0, len(deps))
		for n := range deps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if total++; u.cut(BoundDependencies, u.deps, int64(total)) {
				return nil
			}
			var constraint string
			if err := json.Unmarshal(deps[n], &constraint); err != nil {
				u.malformedEntry()
				continue
			}
			var rng *model.SourceRange
			if s, ok := spans.nested[sec.key][n]; ok {
				rng = u.rng(s.start, s.end)
			}
			var extra []string
			if composerPlatform(n) {
				extra = []string{"platform", "true"}
			}
			if err := u.edge(ctx, node, model.RelDependsOn, ecosystemComposer, strings.ToLower(n), languagePHP, sec.kind, constraint, rng, extra...); err != nil {
				return err
			}
		}
	}
	return nil
}

// composerPlatform reports whether a link names a platform package: the PHP
// or HHVM runtime and its variants, an extension, a system library, or a Composer
// API. Composer satisfies these from the running system, never from a
// repository.
func composerPlatform(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "php", "php-64bit", "php-ipv6", "php-zts", "php-debug", "hhvm", "composer", "composer-plugin-api", "composer-runtime-api":
		return true
	}
	return strings.HasPrefix(n, "ext-") || strings.HasPrefix(n, "lib-")
}
