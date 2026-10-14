package link

import (
	"context"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// esPlanInputs is the paths JavaScript unit u's configuration reads through
// `extends` beyond the files OwnsInput's names cover (Unit.Inputs), sorted:
// every config base looked up by its path, present or not, since creating
// or editing it changes the options the unit resolves under. found is every
// snapshot file esConfigFile admits; the unit's own are read and their
// `extends` chains followed as the link reads them (esInputs.config).
// package.json files are read only when a config of the unit extends a
// package by name, the one form the workspace map decides.
func esPlanInputs(ctx context.Context, view model.SnapshotView, u Unit, found []model.FileVersion) ([]string, error) {
	var own []model.FileVersion
	for _, fv := range found {
		if u.Contains(fv.Path) {
			own = append(own, fv)
		}
	}
	in := newESInputs(view, u.Root)
	if err := in.readConfigs(ctx, own); err != nil {
		return nil, err
	}
	byName := false
	for _, r := range in.raws {
		bases, _ := r.v.strings("extends")
		for _, spec := range bases {
			byName = byName || !esRelative(spec)
		}
	}
	if byName {
		if err := in.readPackages(ctx, own); err != nil {
			return nil, err
		}
	}
	paths := make([]string, 0, len(in.raws))
	for p := range in.raws {
		paths = append(paths, p)
	}
	for _, p := range paths {
		if _, err := in.config(ctx, p); err != nil {
			return nil, err
		}
	}
	if len(in.outside) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in.outside))
	for p := range in.outside {
		out = append(out, p)
	}
	slices.Sort(out)
	return out, nil
}
