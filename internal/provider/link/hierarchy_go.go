package link

import (
	"context"
	"io"
	"path"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The records addLinks writes to methodSets for a Go table, each under the
// refKey of the type it describes, in this order within one type.
const (
	// goTypeRecord marks a type declaration; flagAbstract in Flags says it
	// is an interface.
	goTypeRecord byte = iota
	// goMethodRecord is a method the type itself declares: its name in Aux,
	// its signature in Sig, its flags in Flags.
	goMethodRecord
	// goFieldRecord is a field the type itself declares, its name in Aux:
	// a named field, or an embedded field, named by its type's unqualified
	// name ("Struct types").
	goFieldRecord
	// goEmbedRecord is a type the type embeds, in Ref.
	goEmbedRecord
)

// addGoMethod records that typ itself declares the method e (keyed under
// typ by name in links).
func addGoMethod(methodSets *pagination.ExternalSort[entry], typ Ref, name string, e entry) error {
	return methodSets.Add(entry{Key: refKey(typ), Tag: goMethodRecord, Ref: e.Ref, Aux: name, Sig: e.Sig, Flags: e.Flags})
}

// goEmbeddings records each type declaration of a Go table, the fields
// each declares and what each embeds. An embedded field or an embedded interface promotes the embedded
// type's methods into the embedding type's method set (The Go Programming
// Language Specification, "Struct types", "Interface types"): it is walked
// upward from the embedding type, as a supertype, and never makes the
// embedding type a subtype a call on the embedded one dispatches into.
func (h *hierarchy) goEmbeddings(ctx context.Context, links, methodSets *pagination.ExternalSort[entry], fc fileCtx, t *Table) error {
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
		f := entry{Key: refKey(Ref{File: fc.file, Index: d.Parent}), Tag: goFieldRecord, Aux: d.Name}
		if err := methodSets.Add(f); err != nil {
			return err
		}
	}
	for _, st := range t.Supertypes {
		err := h.typesIn(ctx, fc, st.Type, func(sub entry) error {
			return h.typesIn(ctx, fc, st.Super, func(super entry) error {
				if sub.Ref == super.Ref {
					return nil
				}
				if err := links.Add(entry{Key: keySuper + refKey(sub.Ref), Ref: super.Ref}); err != nil {
					return err
				}
				field := entry{Key: refKey(sub.Ref), Tag: goFieldRecord, Aux: lastComponent(typeName(st.Super), ".")}
				if err := methodSets.Add(field); err != nil {
					return err
				}
				return methodSets.Add(entry{Key: refKey(sub.Ref), Tag: goEmbedRecord, Ref: super.Ref})
			})
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
// promote (goMethodSet); each promoted method is added to links under the
// type, so a call on the type, or on an interface it satisfies, reaches the
// promoted declaration. A type satisfies an interface when its method set
// includes every method of the interface's, by name and signature
// ("Interface types", "Method sets"). sets holds, under each
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
		return goMethodSet(ix, typ, tag == 0, func(name string, m entry, promoted bool) error {
			if promoted {
				err := links.Add(entry{Key: keyMethod + refKey(typ) + name, Ref: m.Ref, Sig: m.Sig, Flags: m.Flags})
				if err != nil {
					return err
				}
			}
			return sets.Add(entry{Key: name + "\x00" + m.Sig, Tag: tag, Ref: typ})
		})
	})
	if err != nil {
		return err
	}
	return pairSatisfied(dir, links, sets)
}

// goMethodSet yields the method set of the Go type typ ("Method sets",
// "Selectors"): every method typ declares, then, depth by depth through its
// embedded types, each method of a name no shallower depth declares as a
// method or a field. At one depth a name a struct reaches along two or more
// embedding paths, or as a field, is in no method set, and hides the name at
// every deeper depth; an interface's method set is the union of its embedded
// interfaces' and admits a name from several of them. An interface embeds
// only interfaces. Only typ's own walk is held.
func goMethodSet(ix *index, typ Ref, iface bool, yield func(name string, m entry, promoted bool) error) error {
	have := map[string]bool{}
	// level counts the embedding paths reaching each type at the current
	// depth, two meaning two or more.
	level := map[Ref]int{}
	err := ix.lookup(refKey(typ), func(e entry) error {
		switch e.Tag {
		case goMethodRecord:
			have[e.Aux] = true
			return yield(e.Aux, e, false)
		case goFieldRecord:
			have[e.Aux] = true
		case goEmbedRecord:
			level[e.Ref] = min(level[e.Ref]+1, 2)
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
		type reach struct {
			methods []entry
			paths   int
		}
		byName := map[string]*reach{}
		next := map[Ref]int{}
		for embedded, paths := range level {
			skip := false
			err := ix.lookup(refKey(embedded), func(e entry) error {
				switch {
				case e.Tag == goTypeRecord:
					skip = iface && e.Flags&flagAbstract == 0
				case skip:
				case e.Tag == goMethodRecord || e.Tag == goFieldRecord:
					r := byName[e.Aux]
					if r == nil {
						r = &reach{}
						byName[e.Aux] = r
					}
					if e.Tag == goMethodRecord {
						r.methods = append(r.methods, e)
					}
					r.paths = min(r.paths+paths, 2)
				case e.Tag == goEmbedRecord && !visited[e.Ref]:
					next[e.Ref] = min(next[e.Ref]+paths, 2)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		for name, r := range byName {
			if have[name] {
				continue
			}
			have[name] = true
			if !iface && r.paths > 1 || len(r.methods) == 0 {
				continue
			}
			for _, m := range r.methods {
				if err := yield(name, m, true); err != nil {
					return err
				}
			}
		}
		level = next
	}
	return nil
}

// pairSatisfied adds every type whose method set includes an interface's
// whole method set as that interface's subtype; sets is satisfy's.
func pairSatisfied(dir string, links, sets *pagination.ExternalSort[entry]) error {
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
			if n := len(ifaces); n > 0 && ifaces[n-1] == e.Ref {
				return nil
			}
			ifaces = append(ifaces, e.Ref)
			return pairs.Add(entry{Key: refKey(e.Ref), Sig: e.Key})
		}
		for _, i := range ifaces {
			if err := pairs.Add(entry{Key: refKey(i) + refKey(e.Ref), Sig: e.Key}); err != nil {
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
	var iface, pair, method string
	want, n := 0, 0
	return counted.Each(func(e entry) error {
		if len(e.Key) == len(refKey(Ref{})) {
			if e.Key != iface {
				iface, want, method = e.Key, 0, ""
			}
			if e.Sig != method {
				method = e.Sig
				want++
			}
			return nil
		}
		if e.Key != pair {
			pair, n, method = e.Key, 0, ""
		}
		if e.Sig == method {
			return nil
		}
		method = e.Sig
		if n++; pair[:8] != iface || n != want {
			return nil
		}
		return addSubtype(links, refOf(pair[8:]), refOf(pair[:8]))
	})
}

// readGoModule reads the module path of the go.mod at the unit's project
// root, which decides which import paths name the unit's own packages. A
// unit with no go.mod there has no module path.
func (h *hierarchy) readGoModule(ctx context.Context, u *unit) error {
	h.goRoot = path.Join(".", u.root)
	if u.content == nil {
		return nil
	}
	return u.content.EachFile(ctx, model.FileSelection{Paths: []string{path.Join(u.root, "go.mod")}}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		rc, fv, err := u.content.Open(ctx, fv.ID)
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, fv.Size))
		if err != nil {
			return err
		}
		h.goModule = modfile.ModulePath(data)
		return nil
	})
}

// goPackages yields the scope (directory and package) of every Go package
// of the unit a Go import path names. An import path names a directory of
// the unit only through the unit's module path ("Import declarations"; the
// go command's module-aware import resolution): the module path itself
// names the project root, a path under it the directory below the root it
// continues with, and any other path the module's vendor directory's copy.
// A unit with no module path binds no import path.
func (h *hierarchy) goPackages(importPath string, yield func(string) error) error {
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
