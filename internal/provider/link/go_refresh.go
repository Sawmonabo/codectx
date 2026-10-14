package link

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A Go unit's manifest fields are of two kinds. The module path decides
// which import paths name the unit's own packages, so the hierarchy built
// from the tables depends on it. Every other field (the go and toolchain
// lines, each requirement's version, each replacement, the governing
// go.work, the vendor manifest, the standard library and what the module
// cache holds) only says where each package outside the unit is read from
// (goExternal). A change to those alone keeps the unit's layout: the
// packages outside it are read again, and the hierarchy is kept when what
// they declare is what the predecessor's build read (early cutoff, Mokhov,
// Mitchell and Peyton Jones, "Build systems à la carte", §2.3; Salsa's
// backdating). A dependency bump that changes no declaration the unit
// reads therefore reads no table and re-runs nothing; one that does
// rebuilds the hierarchy, and only the members whose reads changed re-run.

// fieldGoSurface keys the group whose hash is the digest of everything the
// unit's hierarchy read from packages outside it (goExternal.surface).
const fieldGoSurface = "go.external\x00surface"

// goExternalField reports a Go manifest field that only says where a
// package outside the unit is read from.
func goExternalField(key string) bool {
	for _, p := range [...]string{fieldGoVersion, fieldGoToolchain, fieldGoRequire, fieldGoReplace, fieldGoWork,
		fieldGoWorkUse, fieldGoVendor, fieldGoRoot, fieldGoSource} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// layoutFields is fields without the Go fields that only say where a
// package outside the unit is read from: the fields a hierarchy built from
// the tables depends on.
func layoutFields(fields []manifestField) []manifestField {
	return slices.DeleteFunc(slices.Clone(fields), func(f manifestField) bool { return goExternalField(f.Key) })
}

// surface is the digest of every package x read, in import path order:
// each package's name, its files' imports and every type it declares with
// its methods and embedded types, or its absence. The hierarchy reads
// nothing else outside the unit, and it is read in full (materialize,
// lookup), so two reads of one surface give one hierarchy.
func (x *goExternal) surface() string {
	paths := make([]string, 0, len(x.pkgs))
	for p := range x.pkgs {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	parts := []string{strconv.Itoa(x.read), strconv.Itoa(x.unavailable)}
	for _, p := range paths {
		parts = append(parts, p)
		pkg := x.pkgs[p]
		if pkg == nil {
			parts = append(parts, "\x00absent")
			continue
		}
		parts = append(parts, pkg.name, strconv.Itoa(len(pkg.files)))
		for _, f := range pkg.files {
			names := make([]string, 0, len(f.imports))
			for n := range f.imports {
				names = append(names, n)
			}
			slices.Sort(names)
			for _, n := range names {
				parts = append(parts, n, strings.Join(f.imports[n], "\x00"))
			}
			parts = append(parts, "\x01"+strings.Join(f.dot, "\x00"), "\x02"+strings.Join(f.unnamed, "\x00"), "\x04"+f.constraint)
		}
		consts := make([]string, 0, len(pkg.consts))
		for n := range pkg.consts {
			consts = append(consts, n)
		}
		slices.Sort(consts)
		for _, n := range consts {
			for _, c := range pkg.consts[n] {
				parts = append(parts, "\x03"+n, c.form, strconv.Itoa(c.file), goFilesString(c.also))
			}
		}
		types := make([]string, 0, len(pkg.types))
		for n := range pkg.types {
			types = append(types, n)
		}
		slices.Sort(types)
		for _, n := range types {
			for _, t := range append([]*goExtType{pkg.types[n]}, pkg.types[n].variants...) {
				parts = append(parts, n, strconv.Itoa(t.file), goFilesString(t.also), t.form,
					flagString(t.alias, t.iface, t.ineligible, t.conflict))
				for _, ms := range [][]goExtMethod{t.methods, t.recv, t.embeds} {
					parts = append(parts, strconv.Itoa(len(ms)))
					for _, m := range ms {
						parts = append(parts, m.name, m.form, strconv.Itoa(m.file), goFilesString(m.also))
					}
				}
			}
		}
	}
	return model.H(groupDomain, parts...)
}

// goFilesString is the file ordinals fs, in order.
func goFilesString(fs []int) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(strconv.Itoa(f))
		b.WriteByte(',')
	}
	return b.String()
}

func flagString(flags ...bool) string {
	b := make([]byte, len(flags))
	for i, f := range flags {
		b[i] = '0'
		if f {
			b[i] = '1'
		}
	}
	return string(b)
}

// sameGoSurface reads the packages outside a Go unit again, under the
// unit's fresh manifest over prev's hierarchy (whose declarations are this
// build's when the layout holds), and answers their surface and whether it
// is the one prev's groups hold: what prev's build read.
func sameGoSurface(ctx context.Context, u *unit, prev *previous) (string, bool, error) {
	info, err := readGoModuleInfo(ctx, u)
	if err != nil {
		return "", false, err
	}
	h := prev.h
	h.goInfo = info
	x, err := newGoExternal(ctx, h, u)
	if err != nil {
		return "", false, err
	}
	defer x.close()
	surface, want, same := x.surface(), groupKey(fieldGoSurface), false
	err = prev.groups.Each(func(g group) error {
		if g.Key == want {
			same = g.Hash == surface
		}
		return nil
	})
	return surface, same, err
}

// groupKey is the group key of the manifest field (or derived field) key.
func groupKey(key string) string { return manifestKey(key).hash() }

// withFields is groups with every manifest field's group replaced by those
// of fields, and the external surface's group by surface's (none when
// empty): what indexGroups gives the same hierarchy under fields. Only the
// manifest's few keys are held.
func withFields(dir string, groups *pagination.SortedRun[group], old, fields []manifestField,
	surface string) (*pagination.SortedRun[group], error) {
	drop := map[string]bool{groupKey(fieldGoSurface): true}
	for _, f := range old {
		drop[groupKey(f.Key)] = true
	}
	out, err := newGroupSort(dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	err = groups.Each(func(g group) error {
		if drop[g.Key] {
			return nil
		}
		return out.Add(g)
	})
	if err != nil {
		return nil, err
	}
	if err := fieldGroups(fields, surface, out.Add); err != nil {
		return nil, err
	}
	return out.Sorted()
}

// fieldGroups adds one group per manifest field key of fields (sorted by
// key; two manifests may give one key two values, two crates of one name,
// which are one group), and the external surface's when surface is set.
func fieldGroups(fields []manifestField, surface string, add func(group) error) error {
	for i := 0; i < len(fields); {
		j := i
		var values []string
		for ; j < len(fields) && fields[j].Key == fields[i].Key; j++ {
			values = append(values, fields[j].Value)
		}
		if err := add(group{Key: groupKey(fields[i].Key), Hash: model.H(groupDomain, values...)}); err != nil {
			return err
		}
		i = j
	}
	if surface == "" {
		return nil
	}
	return add(group{Key: groupKey(fieldGoSurface), Hash: surface})
}
