package manifest

import (
	"context"
	"path"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

// KindLocked marks an occurrence a lock file states: the version a resolver
// selected, not a range a manifest asks for.
const KindLocked = "locked"

// lockEntry is one package a lock file pins: its name as its ecosystem's
// manifests name it (so the dependency node is the one a manifest's
// requirement reaches), the exact version selected, the integrity value the
// format records for the package, where it comes from, and what it depends
// on. start and end are the entry's bytes in the lock file.
type lockEntry struct {
	name, version string
	// integrity is the format's one hash of the package (an npm Subresource
	// Integrity string, a Cargo or yarn checksum), or for a Python lock the
	// hash of the package's one source distribution. hashes counts the
	// distribution-file hashes a Python lock pins, each of one file.
	integrity string
	hashes    int
	// source is where the package comes from as the lock writes it (a
	// registry, a git URL, a local path), and local marks a package of the
	// repository itself: a workspace member, a path or editable install.
	source string
	local  bool
	// group is the lock's own grouping of the package when it has one (a
	// Pipfile.lock section, a PDM or Poetry group), empty otherwise.
	group string
	deps  []lockDep
	// start and end are the entry's byte span, used as the evidence range.
	start, end int
}

// lockDep is one dependency of a locked package: the name it is declared
// by, the requirement as the lock records it, the version the lock resolves
// it to when the lock says which, and the dependency kind.
type lockDep struct {
	name, requirement, version, kind string
}

// lockFile is what one lock reader produces: the ecosystem and language its
// names belong to, and every entry in file order.
type lockFile struct {
	ecosystem, language string
	// version is the lock format version the file declares, when it does.
	version string
	entries []lockEntry
	// resolve, when set, resolves a dependency of entry i the lock names
	// without a version to the version it selects (npm's node_modules
	// lookup, bun's nested keys). Without it a dependency resolves only when
	// the lock pins exactly one version of its name.
	resolve func(i int, dep string) string
}

// lock publishes a lock file: a `configuration` node the file defines, one
// `depends_on` occurrence from it to each pinned package's dependency node
// (the node a manifest's requirement of the same name reaches) with the
// resolved version, integrity and source, and one `depends_on` occurrence
// from each pinned package to each package it depends on. A name the lock
// pins at several versions publishes one occurrence per version, each with
// set_size the number of versions, so the set is never resolved by
// preference.
func (u *unit) lock(ctx context.Context, lf lockFile) error {
	rel := u.e.File().Path
	meta := map[string]any{"format": formatOf(rel)}
	if lf.version != "" {
		meta["lock_version"] = lf.version
	}
	cfg, err := u.defines(ctx, model.NodeConfiguration, lf.ecosystem+":lock:"+rel, path.Base(rel), lf.language, nil, meta)
	if err != nil {
		return err
	}
	versions := map[string][]string{}
	for _, e := range lf.entries {
		if vs := versions[e.name]; !slices.Contains(vs, e.version) {
			versions[e.name] = append(vs, e.version)
		}
	}
	// nodes holds each name's dependency node once resolved: a pinned
	// package adds its own entry's evidence to its node, while a package
	// named only as a dependency is resolved once, not once per dependent.
	nodes := map[string]model.Node{}
	total := 0
	for i, e := range lf.entries {
		if total++; u.cut(BoundDependencies, u.deps, int64(total)) {
			return nil
		}
		if e.name == "" || len(e.name) > model.MaxNameBytes {
			u.malformedEntry()
			continue
		}
		rng := u.rng(e.start, e.end)
		dep, err := u.e.Node(ctx, dependencyCandidate(lf.ecosystem, e.name, lf.language), filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng})
		if err != nil {
			return err
		}
		nodes[e.name] = dep
		u.e.Symbol(dep, rng)
		detail := []string{"kind", KindLocked, "resolved_version", e.version, "integrity", e.integrity, "source", e.source,
			"set_size", strconv.Itoa(len(versions[e.name]))}
		if e.hashes > 0 {
			detail = append(detail, "hashes", strconv.Itoa(e.hashes))
		}
		if e.local {
			detail = append(detail, "local", "true")
		}
		if e.group != "" {
			detail = append(detail, "group", e.group)
		}
		if err := u.e.Relation(cfg.ID, model.RelDependsOn, dep.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng,
			Detail: filesystem.Detail(detail...)}); err != nil {
			return err
		}
		for _, d := range e.deps {
			if d.name == "" || len(d.name) > model.MaxNameBytes {
				u.malformedEntry()
				continue
			}
			resolved := d.version
			if resolved == "" && lf.resolve != nil {
				resolved = lf.resolve(i, d.name)
			}
			if vs := versions[d.name]; resolved == "" && len(vs) == 1 {
				resolved = vs[0]
			}
			to, ok := nodes[d.name]
			if !ok {
				if to, err = u.e.Node(ctx, dependencyCandidate(lf.ecosystem, d.name, lf.language), filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng}); err != nil {
					return err
				}
				nodes[d.name] = to
			}
			kind := d.kind
			if kind == "" {
				kind = KindRuntime
			}
			if err := u.e.Relation(dep.ID, model.RelDependsOn, to.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng,
				Detail: filesystem.Detail("kind", kind, "version", e.version, "requirement", d.requirement,
					"resolved_version", resolved)}); err != nil {
				return err
			}
		}
	}
	return nil
}
