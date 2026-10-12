package link

import "context"

// fieldGoModule is a Go unit's module path (goModulePath).
const fieldGoModule = "go.mod\x00module"

// goManifestFields is every go.mod field a Go unit's resolution reads.
func goManifestFields(ctx context.Context, u *unit) ([]manifestField, error) {
	m, err := goModulePath(ctx, u)
	if err != nil {
		return nil, err
	}
	return []manifestField{{Key: fieldGoModule, Value: m}}, nil
}
