package link

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// The declarations read from a package of the standard library or the
// module cache are a function of its source alone, and that source never
// changes under its identity: a module cache directory is read-only and
// named by its exact version ("Module cache"), a GOROOT's standard library
// by its release, a package of the snapshot by its files' names and content
// hashes. So they are kept, once read, in the user cache under the
// product's directory (paced.ProductDirName, the location every process of
// the user agrees on), keyed by that identity, and every later unit and
// build reads them from there instead of parsing the package again. A cache
// entry that cannot be read or written costs only the parse; nothing in it
// is ever needed for correctness.

// goDeclarationsFormat names the encoding of a cache entry, so an entry
// written in another encoding is never decoded as this one.
const goDeclarationsFormat = "go-declarations\x00name files(imports dot unnamed) types(name file alias form iface ineligible conflict methods embeds variants) consts(name form file) forms(struct interface array-constant generic-alias) declarations(each-distinct-method each-distinct-const)"

type goDeclPackage struct {
	Name   string
	Files  []goDeclFile
	Types  []goDeclType
	Consts []goDeclMethod
}

type goDeclFile struct {
	Imports map[string][]string
	Dot     []string
	Unnamed []string
}

type goDeclType struct {
	Name                               string
	File                               int
	Alias, Iface, Ineligible, Conflict bool
	Form                               string
	Methods, Embeds                    []goDeclMethod
	Variants                           []goDeclType
}

type goDeclMethod struct {
	Name, Form string
	File       int
}

// goDeclCacheFile is the cache entry of the package of identity id, empty
// when the user cache has no location.
func goDeclCacheFile(id string) string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(goDeclarationsFormat + "\x00" + id))
	return filepath.Join(base, paced.ProductDirName, "go-declarations", hex.EncodeToString(sum[:])+".gob")
}

// readGoDecls is the cached package of identity id at import path p.
func readGoDecls(id, p string) (*goExtPackage, bool) {
	file := goDeclCacheFile(id)
	if file == "" {
		return nil, false
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var d goDeclPackage
	if err := gob.NewDecoder(f).Decode(&d); err != nil {
		return nil, false
	}
	pkg := &goExtPackage{path: p, name: d.Name, types: make(map[string]*goExtType, len(d.Types))}
	for _, df := range d.Files {
		imports := df.Imports
		if imports == nil {
			imports = map[string][]string{}
		}
		pkg.files = append(pkg.files, goExtFile{imports: imports, dot: df.Dot, unnamed: df.Unnamed})
	}
	methods := func(ms []goDeclMethod) []goExtMethod {
		var out []goExtMethod
		for _, m := range ms {
			if m.File < 0 || m.File >= len(pkg.files) {
				return nil
			}
			out = append(out, goExtMethod{name: m.Name, form: m.Form, file: m.File})
		}
		return out
	}
	for _, c := range d.Consts {
		if c.File < 0 || c.File >= len(pkg.files) {
			return nil, false
		}
		if pkg.consts == nil {
			pkg.consts = map[string][]goExtMethod{}
		}
		pkg.consts[c.Name] = append(pkg.consts[c.Name], goExtMethod{name: c.Name, form: c.Form, file: c.File})
	}
	var decode func(t goDeclType) (*goExtType, bool)
	decode = func(t goDeclType) (*goExtType, bool) {
		if t.File >= len(pkg.files) {
			return nil, false
		}
		et := &goExtType{file: t.File, alias: t.Alias, form: t.Form, iface: t.Iface,
			ineligible: t.Ineligible, conflict: t.Conflict, methods: methods(t.Methods), embeds: methods(t.Embeds)}
		for _, v := range t.Variants {
			ev, ok := decode(v)
			if !ok {
				return nil, false
			}
			et.variants = append(et.variants, ev)
		}
		return et, true
	}
	for _, t := range d.Types {
		et, ok := decode(t)
		if !ok {
			return nil, false
		}
		pkg.types[t.Name] = et
	}
	return pkg, true
}

// writeGoDecls keeps pkg under identity id, replacing the entry whole so a
// reader never sees part of one.
func writeGoDecls(id string, pkg *goExtPackage) {
	file := goDeclCacheFile(id)
	if file == "" {
		return
	}
	d := goDeclPackage{Name: pkg.name}
	for _, f := range pkg.files {
		d.Files = append(d.Files, goDeclFile{Imports: f.imports, Dot: f.dot, Unnamed: f.unnamed})
	}
	methods := func(ms []goExtMethod) []goDeclMethod {
		var out []goDeclMethod
		for _, m := range ms {
			out = append(out, goDeclMethod{Name: m.name, Form: m.form, File: m.file})
		}
		return out
	}
	for _, cs := range pkg.consts {
		for _, c := range cs {
			d.Consts = append(d.Consts, goDeclMethod{Name: c.name, Form: c.form, File: c.file})
		}
	}
	var encode func(name string, t *goExtType) goDeclType
	encode = func(name string, t *goExtType) goDeclType {
		dt := goDeclType{Name: name, File: t.file, Alias: t.alias, Form: t.form, Iface: t.iface,
			Ineligible: t.ineligible, Conflict: t.conflict, Methods: methods(t.methods), Embeds: methods(t.embeds)}
		for _, v := range t.variants {
			dt.Variants = append(dt.Variants, encode(name, v))
		}
		return dt
	}
	for name, t := range pkg.types {
		d.Types = append(d.Types, encode(name, t))
	}
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	err = gob.NewEncoder(tmp).Encode(&d)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), file)
	}
	if err != nil {
		_ = paced.Remove(tmp.Name())
	}
}
