package manifest

import (
	"context"
	"strconv"

	"golang.org/x/mod/modfile"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

const (
	ecosystemGo = "go"
	languageGo  = "go"
)

// lineOf converts a modfile line span to a source range.
func (u *unit) lineOf(l *modfile.Line) *model.SourceRange {
	if l == nil {
		return nil
	}
	start, end := l.Span()
	return u.rng(start.Byte, end.Byte)
}

// goMod reads a go.mod with x/mod: the module, its requirements (an indirect
// requirement is still a runtime dependency; the comment travels on the
// evidence), replacements as `configures` edges and the go/toolchain
// directives as metadata. Nothing is fetched. Strict parsing is used because
// a go.mod in the repository is a main module: ParseLax would drop the
// replace and exclude directives that only a main module carries.
func (u *unit) goMod(ctx context.Context) error {
	f, err := modfile.Parse(u.e.File().Path, u.data, nil)
	if err != nil || f.Module == nil || f.Module.Mod.Path == "" {
		u.malformed()
		return nil
	}
	meta := map[string]any{}
	if f.Go != nil {
		meta["go"] = f.Go.Version
	}
	if f.Toolchain != nil {
		meta["toolchain"] = f.Toolchain.Name
	}
	if f.Module.Deprecated != "" {
		meta["deprecated"] = f.Module.Deprecated
	}
	mod, err := u.defines(ctx, model.NodeModule, ecosystemGo+":"+f.Module.Mod.Path, f.Module.Mod.Path, languageGo, u.lineOf(f.Module.Syntax), meta)
	if err != nil {
		return err
	}
	if len(f.Require) > MaxDependencies {
		f.Require = f.Require[:MaxDependencies]
		u.overBound()
	}
	for _, r := range f.Require {
		if err := u.edge(ctx, mod, model.RelDependsOn, ecosystemGo, r.Mod.Path, languageGo, KindRuntime, r.Mod.Version, u.lineOf(r.Syntax),
			"indirect", strconv.FormatBool(r.Indirect)); err != nil {
			return err
		}
	}
	if err := u.goReplacements(ctx, mod, f.Replace); err != nil {
		return err
	}
	for _, x := range boundedExcludes(f.Exclude, u) {
		if err := u.edge(ctx, mod, model.RelConfigures, ecosystemGo, x.Mod.Path, languageGo, KindRuntime, x.Mod.Version, u.lineOf(x.Syntax), "exclude", "true"); err != nil {
			return err
		}
	}
	return nil
}

// goReplacements emits `configures` edges for replace directives: the module
// (or workspace) redirects the old module to a path or another version.
func (u *unit) goReplacements(ctx context.Context, from model.Node, replaces []*modfile.Replace) error {
	if len(replaces) > MaxEntries {
		replaces = replaces[:MaxEntries]
		u.overBound()
	}
	for _, r := range replaces {
		target := r.New.Path
		if r.New.Version != "" {
			target += "@" + r.New.Version
		}
		if err := u.edge(ctx, from, model.RelConfigures, ecosystemGo, r.Old.Path, languageGo, KindRuntime, r.Old.Version, u.lineOf(r.Syntax), "replace", target); err != nil {
			return err
		}
	}
	return nil
}

func boundedExcludes(list []*modfile.Exclude, u *unit) []*modfile.Exclude {
	if len(list) > MaxEntries {
		u.overBound()
		return list[:MaxEntries]
	}
	return list
}

// goWork reads a go.work: a configuration node whose `use` directives
// configure the module directories they name (the repository for `use .`)
// and whose replacements configure the replaced modules.
func (u *unit) goWork(ctx context.Context) error {
	f, err := modfile.ParseWork(u.e.File().Path, u.data, nil)
	if err != nil {
		u.malformed()
		return nil
	}
	meta := map[string]any{}
	if f.Go != nil {
		meta["go"] = f.Go.Version
	}
	if f.Toolchain != nil {
		meta["toolchain"] = f.Toolchain.Name
	}
	rel := u.e.File().Path
	work, err := u.defines(ctx, model.NodeConfiguration, ecosystemGo+":work:"+rel, "go.work", languageGo, nil, meta)
	if err != nil {
		return err
	}
	uses := f.Use
	if len(uses) > MaxEntries {
		uses = uses[:MaxEntries]
		u.overBound()
	}
	for _, use := range uses {
		rng := u.lineOf(use.Syntax)
		target, ok, err := u.pathTarget(ctx, use.Path, true, rng, model.PrecisionSyntax)
		if err != nil {
			return err
		}
		if !ok {
			u.malformedEntry()
			continue
		}
		if err := u.e.Relation(work.ID, model.RelConfigures, target.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng,
			Detail: filesystem.Detail("use", use.Path, "module", use.ModulePath)}); err != nil {
			return err
		}
	}
	return u.goReplacements(ctx, work, f.Replace)
}
