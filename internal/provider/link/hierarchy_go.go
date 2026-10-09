package link

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/pagination"
)

// satisfy derives Go's implicit interface satisfaction: a type satisfies an
// interface when its methods include every method the interface declares,
// by name and signature. methodSets holds, under each name-and-signature
// key, the interfaces declaring it (tag 0) before the types declaring it
// (tag 1). Each interface's method keys are sorted under its own refKey,
// ahead of its pairs with types under refKey(interface) + refKey(type); a
// pair that matches as many distinct keys as the interface has is a
// subtype.
func (h *hierarchy) satisfy(dir string, links, methodSets *pagination.ExternalSort[entry]) error {
	run, err := methodSets.Sorted()
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

// goPackages yields the scope (directory and package) of every Go package
// in the unit a Go import path names: the packages of the longest suffix of
// the path, at a component boundary, that is a directory of the unit.
func (h *hierarchy) goPackages(importPath string, yield func(string) error) error {
	for p := importPath; ; {
		found := false
		err := h.decls.lookup(keyGoDir+p, func(e entry) error {
			found = true
			return yield(p + "\x00" + e.Aux)
		})
		if err != nil || found {
			return err
		}
		i := strings.IndexByte(p, '/')
		if i < 0 {
			return nil
		}
		p = p[i+1:]
	}
}
