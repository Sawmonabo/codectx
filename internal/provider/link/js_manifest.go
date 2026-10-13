package link

import (
	"context"
	"strings"
)

// The keys of an ECMAScript unit's manifest fields: + a config's path, its
// options after `extends` (tsConfig.spell); + a package.json's directory,
// the fields resolution reads (esPackage.raw); + a pnpm-workspace.yaml's or
// lerna.json's path, its `packages` globs.
const (
	fieldJSConfig    = "tsconfig\x00"
	fieldJSPackage   = "package.json\x00"
	fieldJSWorkspace = "workspace\x00"
)

// jsManifestFields is every configuration field an ECMAScript unit's
// resolution reads. A change to any of them makes a refresh rebuild the
// unit's hierarchy, whose specifier answers (keyESSpec) are then diffed key
// by key, so only the nodes whose answers moved re-run.
func jsManifestFields(ctx context.Context, u *unit) ([]manifestField, error) {
	in, err := esInputsOf(ctx, u)
	if err != nil {
		return nil, err
	}
	var out []manifestField
	for p := range in.raws {
		c, err := in.config(ctx, p)
		if err != nil {
			return nil, err
		}
		if c != nil {
			out = append(out, manifestField{Key: fieldJSConfig + p, Value: c.digest})
		}
	}
	for dir, p := range in.pkgs {
		out = append(out, manifestField{Key: fieldJSPackage + dir, Value: p.raw})
	}
	for p, globs := range in.workspaceFiles {
		out = append(out, manifestField{Key: fieldJSWorkspace + p, Value: strings.Join(globs, "\x00")})
	}
	return out, nil
}
