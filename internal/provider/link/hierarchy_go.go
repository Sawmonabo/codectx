package link

import (
	"context"
	"go/token"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// keyGoField + refKey(type) + name: the field a selector of that name on a
// Go type denotes, its own or promoted (satisfy). Sig is the field's type as
// written in the file of Ref, a declaration of that file; empty when the
// field's type names no declaration.
const keyGoField = "gf\x00"

// The records addLinks writes to methodSets for a Go table, each under the
// refKey of the type it describes, in this order within one type.
const (
	// goTypeRecord marks a type declaration; flagAbstract in Flags says it
	// is an interface.
	goTypeRecord byte = iota
	// goMethodRecord is a method the type itself declares: its name in Aux,
	// its signature as written in Sig, its method-set key (goMethodKey, the
	// name and canonical signature) in Returns, its flags in Flags. Every
	// record carries in MaxArgs the build condition (goCond) of the builds
	// it is part of.
	goMethodRecord
	// goFieldRecord is a field the type itself declares, its name in Aux:
	// a named field, or an embedded field, named by its type's unqualified
	// name ("Struct types"). Sig is the field's type as written, in the file
	// of Ref (the field's declaration, or the embedding type's for an
	// embedded field).
	goFieldRecord
	// goEmbedRecord is a type the type embeds, in Ref.
	goEmbedRecord
	// goExternalRecord is a type outside the unit the type embeds, its
	// qualified name (externalName) in Aux.
	goExternalRecord
)

// addGoMethod records that typ itself declares the method e (keyed under
// typ by name in links), of method-set key key, in the builds of condition
// cond.
func addGoMethod(methodSets *pagination.ExternalSort[entry], typ Ref, name, key string, cond goCond, e entry) error {
	return methodSets.Add(entry{Key: refKey(typ), Tag: goMethodRecord, Ref: e.Ref, Aux: name, Sig: e.Sig, Flags: e.Flags,
		MaxArgs: cond, Returns: key})
}

// goEmbeddings records each type declaration of a Go table, the fields
// each declares and what each embeds. An embedded field or an embedded
// interface promotes the embedded type's methods into the embedding type's
// method set (The Go Programming Language Specification, "Struct types",
// "Interface types"), which satisfy lists in full under the embedding type.
// It is no subtype relation in either direction: a call on the embedded type
// never dispatches into the embedding type, and a call on the embedding type
// reaches only what its method set holds, never a promoted method a
// shallower name hides.
func (h *hierarchy) goEmbeddings(methodSets *pagination.ExternalSort[entry], fc fileCtx, t *Table) error {
	// Fields and embeddings are written in their type's declaration, part
	// of the builds of its file.
	cond, err := h.goFileCond(fc.file)
	if err != nil {
		return err
	}
	for i := range t.Decls {
		d := &t.Decls[i]
		if !isType(d.Kind) {
			continue
		}
		e := entry{Key: refKey(Ref{File: fc.file, Index: int32(i)}), Tag: goTypeRecord}
		if d.Kind == model.NodeInterface {
			e.Flags = flagAbstract
		}
		if err := methodSets.Add(e); err != nil {
			return err
		}
	}
	for i := range t.Decls {
		d := &t.Decls[i]
		if d.Kind != model.NodeField || d.Parent < 0 || !isType(t.Decls[d.Parent].Kind) {
			continue
		}
		f := entry{Key: refKey(Ref{File: fc.file, Index: d.Parent}), Tag: goFieldRecord, Aux: d.Name,
			Ref: Ref{File: fc.file, Index: int32(i)}, MaxArgs: cond}
		if d.Returns.Kind == wire.TypeNamed {
			f.Sig = d.Returns.Name
		}
		if err := methodSets.Add(f); err != nil {
			return err
		}
	}
	for _, st := range t.Supertypes {
		var supers []Ref
		err := h.typesIn(fc, st.Super, func(super entry) error {
			supers = append(supers, super.Ref)
			return nil
		})
		if err != nil {
			return err
		}
		external, ok := "", false
		if len(supers) == 0 {
			if external, ok, err = h.externalName(fc, st.Super); err != nil || !ok {
				return err
			}
		}
		if len(supers) == 0 && !ok {
			continue
		}
		err = h.typesIn(fc, st.Type, func(sub entry) error {
			if slices.Contains(supers, sub.Ref) {
				return nil
			}
			declared, err := h.goFileCond(sub.Ref.File)
			if err != nil {
				return err
			}
			cond := h.goConds.and(cond, declared)
			if !h.goConds.holds(cond) {
				return nil
			}
			// The embedded field, named by its type's unqualified name.
			field := entry{Key: refKey(sub.Ref), Tag: goFieldRecord, Aux: lastComponent(typeName(st.Super), "."),
				Ref: sub.Ref, Sig: st.Super, MaxArgs: cond}
			if err := methodSets.Add(field); err != nil {
				return err
			}
			if ok {
				// A type outside the unit whose method set the build read
				// promotes its methods as one of the unit's does.
				if ext, found := h.goExt.materialized(external); found {
					if err := methodSets.Add(entry{Key: refKey(sub.Ref), Tag: goEmbedRecord, Ref: ext, MaxArgs: cond}); err != nil {
						return err
					}
				}
				return methodSets.Add(entry{Key: refKey(sub.Ref), Tag: goExternalRecord, Aux: external, MaxArgs: cond})
			}
			for _, super := range supers {
				if err := methodSets.Add(entry{Key: refKey(sub.Ref), Tag: goEmbedRecord, Ref: super, MaxArgs: cond}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// satisfy derives each Go type's method set and Go's implicit interface
// satisfaction from the records goEmbeddings and addGoMethod wrote to
// methodSets.
//
// A type's method set is its own methods and those its embedded types
// promote (goSelectors); each promoted method is added to links under the
// type, so a call on the type, or on an interface it satisfies, reaches the
// promoted declaration. Each field a selector on the type reaches, its own
// or promoted, is added under keyGoField, and each type outside the unit it
// embeds under keyExternalSuper. A type satisfies an interface when its
// method set includes every method of the interface's, by name and
// signature ("Interface types", "Method sets"), and, since the method set of
// a type outside the unit is in no table, when it embeds every such type the
// interface embeds (goExternalKey). sets holds, under each
// name-and-signature key, the interfaces whose method sets have it (tag 0)
// before the types whose method sets have it (tag 1). Each interface's
// method keys are sorted under its own refKey, ahead of its pairs with types
// under refKey(interface) + refKey(type); a pair that matches as many
// distinct keys as the interface has is a subtype.
func (h *hierarchy) satisfy(dir string, links, methodSets *pagination.ExternalSort[entry]) error {
	if methodSets.Len() == 0 {
		return nil
	}
	records, err := methodSets.Sorted()
	if err != nil {
		return err
	}
	defer records.Close()
	// facts are the records, looked up by type; types are the type
	// records alone, read in order.
	facts, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer facts.Close()
	types, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer types.Close()
	err = records.Each(func(e entry) error {
		if e.Tag == goTypeRecord {
			if err := types.Add(e); err != nil {
				return err
			}
		}
		return facts.Add(e)
	})
	if err != nil {
		return err
	}
	ix, err := writeIndex(dir, facts)
	if err != nil {
		return err
	}
	defer ix.close()
	sets, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer sets.Close()
	typeRun, err := types.Sorted()
	if err != nil {
		return err
	}
	defer typeRun.Close()
	err = typeRun.Each(func(te entry) error {
		typ := refOf(te.Key)
		var tag byte = 1
		if te.Flags&flagAbstract != 0 {
			tag = 0
		}
		// A type outside the unit is read only for the interfaces it is
		// and the methods it promotes: its own satisfaction is not the
		// unit's, and no index entry names it.
		outside := typ.File == goExternalFile
		if outside && tag == 1 {
			return nil
		}
		return goSelectors(ix, h.goConds, typ, tag == 0, func(name string, m entry, promoted bool) error {
			if m.Tag == goExternalRecord {
				var flags byte
				if tag == 0 {
					flags = flagInterface
				}
				if !outside {
					err := links.Add(entry{Key: keyExternalSuper + refKey(typ), Ref: typ, Aux: m.Aux, Flags: flags})
					if err != nil {
						return err
					}
				}
				// An external embedded type whose method set the build did
				// not read is part of the method set the satisfaction
				// counts: an interface embedding it is satisfied only by a
				// type that embeds it too. One it read is counted by its
				// methods instead (goEmbeddings).
				if _, read := h.goExt.materialized(m.Aux); read {
					return nil
				}
				return sets.Add(entry{Key: goExternalKey + m.Aux, Tag: tag, Ref: typ, MaxArgs: m.MaxArgs})
			}
			if outside {
				return sets.Add(entry{Key: m.Returns, Tag: tag, Ref: typ, MaxArgs: m.MaxArgs})
			}
			if m.Tag == goFieldRecord {
				return links.Add(entry{Key: keyGoField + refKey(typ) + name, Ref: m.Ref, Sig: m.Sig})
			}
			if promoted && m.Ref.File != goExternalFile {
				err := links.Add(entry{Key: keyMethod + refKey(typ) + name, Ref: m.Ref, Sig: m.Sig, Flags: m.Flags})
				if err != nil {
					return err
				}
			}
			return sets.Add(entry{Key: m.Returns, Tag: tag, Ref: typ, MaxArgs: m.MaxArgs})
		})
	})
	if err != nil {
		return err
	}
	return pairSatisfied(dir, h.goConds, links, sets, func(sub, iface Ref, keys []string) error {
		q := h.goExt.qualifiedOf(iface)
		if err := links.Add(entry{Key: keyGoExternalImpl + refKey(sub), Ref: sub, Aux: q, Sig: DetailMethodSet}); err != nil {
			return err
		}
		for _, k := range keys {
			name, _, _ := strings.Cut(k, "\x00")
			if strings.HasPrefix(k, goExternalKey) || !token.IsExported(name) {
				continue
			}
			if err := links.Add(entry{Key: keyGoExternalOverride + refKey(sub) + name, Ref: sub, Aux: q, Sig: k}); err != nil {
				return err
			}
		}
		return nil
	})
}

// goExternalKey prefixes the satisfaction key (satisfy's sets) of a type
// outside the unit that a Go type embeds: no method's name and signature
// starts with it.
const goExternalKey = "\x01"

// goSelectors yields the methods and fields a selector on the Go type typ
// denotes ("Selectors", "Method sets"), each a record of its declaring type
// (Tag says which): every method and field typ declares, then, depth by
// depth through its embedded types, each of a name no shallower depth
// declares. It also yields, with an empty name, every type outside the unit
// typ embeds, directly or through its embedded types (goExternalRecord),
// each once. At one depth a name a struct reaches along two or more embedding
// paths denotes nothing, and hides the name at every deeper depth; an
// interface's method set is the union of its embedded interfaces' and admits
// a name from several of them. An interface embeds only interfaces. Only
// typ's own walk is held.
func goSelectors(ix *index, c *goConds, typ Ref, iface bool, yield func(name string, m entry, promoted bool) error) error {
	// have is each name a shallower depth declares, with the condition of
	// the builds in which it does: a deeper member of that name is part of
	// the others alone, and of none when it is 0.
	have := map[string]goCond{}
	declare := func(name string, cond goCond) {
		if old, ok := have[name]; ok {
			cond = c.or(old, cond)
		}
		have[name] = cond
	}
	type outside struct {
		name string
		cond goCond
	}
	external := map[outside]bool{}
	// level counts the embedding paths reaching each type at the current
	// depth, two meaning two or more, and paths the condition of the
	// builds holding one of them.
	level := map[Ref]int{}
	paths := map[Ref]goCond{}
	reach := func(level map[Ref]int, paths map[Ref]goCond, r Ref, n int, cond goCond) {
		if old, ok := paths[r]; ok {
			cond = c.or(old, cond)
		}
		level[r], paths[r] = min(level[r]+n, 2), cond
	}
	err := ix.lookup(refKey(typ), func(e entry) error {
		switch e.Tag {
		case goMethodRecord, goFieldRecord:
			declare(e.Aux, e.MaxArgs)
			return yield(e.Aux, e, false)
		case goExternalRecord:
			external[outside{e.Aux, e.MaxArgs}] = true
			return yield("", e, false)
		case goEmbedRecord:
			reach(level, paths, e.Ref, 1, e.MaxArgs)
		}
		return nil
	})
	if err != nil {
		return err
	}
	visited := map[Ref]bool{typ: true}
	for len(level) > 0 {
		for r := range level {
			visited[r] = true
		}
		// members are the records of one name at this depth, and paths
		// the embedding paths reaching them, each embedded type counted
		// once however many records of the name it holds (one per build
		// condition).
		type members struct {
			members []entry
			paths   int
			last    Ref
		}
		byName := map[string]*members{}
		next := map[Ref]int{}
		nextPaths := map[Ref]goCond{}
		for embedded, n := range level {
			via := paths[embedded]
			skip := false
			err := ix.lookup(refKey(embedded), func(e entry) error {
				if e.Tag != goTypeRecord && !skip {
					// A promoted member is part of the builds holding it
					// and the path that reaches it.
					if e.MaxArgs = c.and(e.MaxArgs, via); !c.holds(e.MaxArgs) {
						return nil
					}
				}
				switch {
				case e.Tag == goTypeRecord:
					skip = iface && e.Flags&flagAbstract == 0
				case skip:
				case e.Tag == goMethodRecord || e.Tag == goFieldRecord:
					r := byName[e.Aux]
					if r == nil {
						r = &members{}
						byName[e.Aux] = r
					}
					if len(r.members) == 0 || r.last != embedded {
						r.paths = min(r.paths+n, 2)
						r.last = embedded
					}
					r.members = append(r.members, e)
				case e.Tag == goEmbedRecord && !visited[e.Ref]:
					reach(next, nextPaths, e.Ref, n, e.MaxArgs)
				case e.Tag == goExternalRecord && !external[outside{e.Aux, e.MaxArgs}]:
					external[outside{e.Aux, e.MaxArgs}] = true
					return yield("", e, true)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		for name, r := range byName {
			hidden, shallower := have[name]
			if shallower && hidden == 0 {
				continue
			}
			var cond goCond
			for i, m := range r.members {
				if i == 0 {
					cond = m.MaxArgs
				} else {
					cond = c.or(cond, m.MaxArgs)
				}
			}
			declare(name, cond)
			if !iface && r.paths > 1 {
				continue
			}
			for _, m := range r.members {
				if shallower {
					if m.MaxArgs = c.andNot(m.MaxArgs, hidden); !c.holds(m.MaxArgs) {
						continue
					}
				}
				if err := yield(name, m, true); err != nil {
					return err
				}
			}
		}
		level, paths = next, nextPaths
	}
	return nil
}

// pairSatisfied adds every type whose method set includes an interface's
// whole method set as that interface's subtype; sets is satisfy's. A pair
// whose interface is outside the unit (goExternalFile) is handed to external
// with the interface's method-set keys the type matches in a build that
// satisfies the whole interface instead, since no index entry names that interface. A method of a name
// has a key per build-dependent signature (goMethodKeys); the pair is
// satisfied when every name is matched and some build holds, for every name,
// one of its keys in both the interface and the type (go_build.go).
func pairSatisfied(dir string, c *goConds, links, sets *pagination.ExternalSort[entry], external func(sub, iface Ref, keys []string) error) error {
	run, err := sets.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	pairs, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer pairs.Close()
	var key string
	// ifaces are the interfaces declaring the current key's method: what
	// one name and signature pairs with, never more than the key's own.
	var ifaces []Ref
	err = run.Each(func(e entry) error {
		if e.Key != key {
			key, ifaces = e.Key, ifaces[:0]
		}
		if e.Tag == 0 {
			if n := len(ifaces); n == 0 || ifaces[n-1] != e.Ref {
				ifaces = append(ifaces, e.Ref)
			}
			return pairs.Add(entry{Key: refKey(e.Ref), Sig: e.Key, MaxArgs: e.MaxArgs})
		}
		for _, i := range ifaces {
			if err := pairs.Add(entry{Key: refKey(i) + refKey(e.Ref), Sig: e.Key, MaxArgs: e.MaxArgs}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	counted, err := pairs.Sorted()
	if err != nil {
		return err
	}
	defer counted.Close()
	// keyed is a key of the current interface or pair, with the condition
	// of the builds holding it there.
	type keyed struct {
		key  string
		cond goCond
	}
	add := func(ks []keyed, k string, cond goCond) []keyed {
		if n := len(ks); n > 0 && ks[n-1].key == k {
			ks[n-1].cond = c.or(ks[n-1].cond, cond)
			return ks
		}
		return append(ks, keyed{k, cond})
	}
	var iface, pair string
	// want counts the interface's method names; conditional reports a key
	// of it or of the pair under a condition some build lacks.
	want := 0
	var ifaceKeys, pairKeys []keyed
	ifaceConditional := false
	finish := func() error {
		if pair == "" || pair[:8] != iface {
			return nil
		}
		defer func() { pair, pairKeys = "", pairKeys[:0] }()
		names := 0
		conditional := ifaceConditional
		for i, k := range pairKeys {
			if i == 0 || goKeyName(k.key) != goKeyName(pairKeys[i-1].key) {
				names++
			}
			conditional = conditional || k.cond != 0
		}
		if names != want {
			return nil
		}
		// met are the keys some build holds in both, all of them unless a
		// condition says otherwise.
		met := make([]string, 0, len(pairKeys))
		if !conditional {
			for _, k := range pairKeys {
				met = append(met, k.key)
			}
		} else {
			// Some build holds, for every name, one of its keys in both;
			// a key is met when such a build holds it.
			var all goCond
			var terms []keyed
			j := 0
			for i := 0; i < len(ifaceKeys); {
				name := goKeyName(ifaceKeys[i].key)
				var any goCond
				found := false
				for ; i < len(ifaceKeys) && goKeyName(ifaceKeys[i].key) == name; i++ {
					for j < len(pairKeys) && pairKeys[j].key < ifaceKeys[i].key {
						j++
					}
					if j == len(pairKeys) || pairKeys[j].key != ifaceKeys[i].key {
						continue
					}
					term := c.and(ifaceKeys[i].cond, pairKeys[j].cond)
					if !c.holds(term) {
						continue
					}
					terms = append(terms, keyed{ifaceKeys[i].key, term})
					if !found {
						any, found = term, true
					} else {
						any = c.or(any, term)
					}
				}
				if !found {
					return nil
				}
				all = c.and(all, any)
			}
			if !c.holds(all) {
				return nil
			}
			for _, t := range terms {
				if c.holds(c.and(t.cond, all)) {
					met = append(met, t.key)
				}
			}
		}
		if refOf(pair[:8]).File == goExternalFile {
			return external(refOf(pair[8:]), refOf(pair[:8]), met)
		}
		return addSubtype(links, refOf(pair[8:]), refOf(pair[:8]), flagInterface)
	}
	err = counted.Each(func(e entry) error {
		if len(e.Key) == len(refKey(Ref{})) {
			if err := finish(); err != nil {
				return err
			}
			if e.Key != iface {
				iface, want, ifaceKeys, ifaceConditional = e.Key, 0, ifaceKeys[:0], false
			}
			if n := len(ifaceKeys); n == 0 || goKeyName(ifaceKeys[n-1].key) != goKeyName(e.Sig) {
				want++
			}
			ifaceKeys = add(ifaceKeys, e.Sig, e.MaxArgs)
			ifaceConditional = ifaceConditional || e.MaxArgs != 0
			return nil
		}
		if e.Key != pair {
			if err := finish(); err != nil {
				return err
			}
			pair = e.Key
		}
		pairKeys = add(pairKeys, e.Sig, e.MaxArgs)
		return nil
	})
	if err != nil {
		return err
	}
	return finish()
}

// goKeyName is the method name of a method-set key (goIdentityKey), or the
// whole key of an embedded type outside the unit (goExternalKey).
func goKeyName(key string) string {
	name, _, _ := strings.Cut(key, "\x00")
	return name
}

// readGoModule reads the go.mod at the unit's project root, the go.work
// governing it and its vendor manifest (readGoModuleInfo): the module path
// decides which import paths name the unit's own packages, the rest where
// each package outside the unit is read from (goExternal).
func (h *hierarchy) readGoModule(ctx context.Context, u *unit) error {
	h.goRoot = path.Join(".", u.root)
	if u.family != FamilyGo {
		return nil
	}
	info, err := readGoModuleInfo(ctx, u)
	if err != nil {
		return err
	}
	h.goInfo, h.goModule = info, info.module
	return nil
}

// goPackages yields the scope (directory and package) of every Go package
// of the unit a Go import path names. An import path names a directory of
// the unit only through the unit's module path ("Import declarations"; the
// go command's module-aware import resolution): the module path itself
// names the project root, a path under it the directory below the root it
// continues with, and any other path the module's vendor directory's copy.
// A unit with no module path binds no import path.
func (h *hierarchy) goPackages(importPath string, yield func(string) error) error {
	if h.note != nil {
		if err := h.note(manifestKey(fieldGoModule)); err != nil {
			return err
		}
	}
	if h.goModule == "" {
		return nil
	}
	dir := path.Join(h.goRoot, "vendor", importPath)
	if importPath == h.goModule {
		dir = h.goRoot
	} else if rel, ok := strings.CutPrefix(importPath, h.goModule+"/"); ok {
		dir = path.Join(h.goRoot, rel)
	}
	return h.decls.lookup(keyGoDir+dir, func(e entry) error { return yield(dir + "\x00" + e.Aux) })
}
