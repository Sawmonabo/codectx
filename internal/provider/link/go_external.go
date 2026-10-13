package link

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"math"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// Go declarations outside the unit (ADR-0015 decision 2): the interfaces of
// the standard library, of the unit's dependencies and of the repository's
// other modules, and the method sets of the types the unit embeds from
// them, read from read-only source so that a type of the unit that
// satisfies an interface outside it `implements` that interface's external
// node and its methods `override` the interface's.
//
// The packages read are the transitive closure of the unit's imports outside
// the unit: what can flow into the program, so an interface of a package the
// unit reaches only through another (fmt.Stringer through log) is read too.
// Each package's source is where the go command reads it (goModuleInfo.source):
// GOROOT/src for the standard library (goRoot), the snapshot for a workspace
// module or a local replacement, the module cache at the exact required
// version for any other module (goModuleDir); the go command is never run,
// nothing is downloaded and nothing is written. A package with no readable
// source is counted (goExternal.unavailable): the unit's implements and
// overrides toward it are then only those its conversions prove
// (goConversions), and its capability rows say so (goExternalCoverage).
//
// A package's files are parsed with go/parser and only their declarations
// read: every non-test file, whatever its build constraints. A name two
// files declare differently (per platform or build tag, goExtType.variants)
// is each of its declarations: an interface declared so is one entity per
// declaration, as the unit's conditional declarations are, and a type that
// satisfies one implements the interface; an alias declared so, and a
// method of a type declared with two signatures, make the forms naming them
// unknown, since which declaration a signature means depends on the build. An interface is
// materialized only when its whole method set is known: no type parameters,
// no type elements, no unexported method (which only its own package can
// implement), every embedded interface materialized too.

// goExternalFile is the File of a materialized external type's Ref: no
// member's ordinal, so the method-set records of external types never name
// a declaration of the unit.
const goExternalFile int32 = math.MinInt32

// goExternal is the unit's read of Go packages outside it.
type goExternal struct {
	h       *hierarchy
	info    *goModuleInfo
	content model.SnapshotView
	// cache is the module cache, goroot the standard library and release
	// its version, all empty when absent.
	cache, goroot, release string
	// pkgs are the packages read, by import path, nil for one with no
	// readable source.
	pkgs map[string]*goExtPackage
	// dirs indexes the snapshot's Go files under the workspace modules'
	// and local replacements' directories (keyed by directory), nil when
	// the unit has none.
	dirs *index
	// refs are the materialized types by qualified name, and names their
	// qualified names by Ref.Index.
	refs  map[string]Ref
	names []string
	// read and unavailable count the packages read and those with no
	// readable source.
	read, unavailable int
}

// goExtPackage is one package's declarations.
type goExtPackage struct {
	path  string
	name  string
	files []goExtFile
	types map[string]*goExtType
	// consts are the package-level constants by name: the form of each
	// one's integer value (goASTConst) and its file.
	consts map[string]goExtMethod
}

// goExtFile is one file's imports: by local name, and its dot imports;
// unnamed are the paths it imports without a name, and unresolved those of
// them whose package clause is not read (renameImports).
type goExtFile struct {
	imports    map[string][]string
	dot        []string
	unnamed    []string
	unresolved []string
}

// goExtType is one type name a package declares.
type goExtType struct {
	file int
	// alias is set for `type A = T`, its form T's (goIdentityUnknown for a
	// generic alias).
	alias bool
	form  string
	iface bool
	// ineligible marks an interface whose method set cannot be known
	// exactly: type parameters, type elements, an unexported method.
	ineligible bool
	// conflict marks a name two files declare differently, and variants
	// are its declarations after the first that differ from every one
	// before them (a declaration per platform or build tag).
	conflict bool
	variants []*goExtType
	methods  []goExtMethod
	embeds   []goExtMethod
}

// goExtMethod is one method (name and form) or embedded type (form), with
// the file its form is written in.
type goExtMethod struct {
	name, form string
	file       int
}

// names is the goNames of the package's file i.
func (p *goExtPackage) names(i int) goNames { return goExtNames{p: p, f: &p.files[i]} }

type goExtNames struct {
	p *goExtPackage
	f *goExtFile
}

func (n goExtNames) pkg() string { return n.p.path }

func (n goExtNames) imported(name string) (string, bool, error) {
	paths := n.f.imports[name]
	if len(paths) != 1 || paths[0] == "C" {
		return "", false, nil
	}
	return paths[0], true, nil
}

func (n goExtNames) dots() ([]string, error) { return n.f.dot, nil }

func (n goExtNames) unresolved() ([]string, error) { return n.f.unresolved, nil }

// newGoExternal locates the unit's sources and reads the closure of its
// imports.
func newGoExternal(ctx context.Context, h *hierarchy, u *unit) (*goExternal, error) {
	x := &goExternal{h: h, info: h.goInfo, content: u.content, pkgs: map[string]*goExtPackage{}, refs: map[string]Ref{}}
	x.cache = goModCache()
	x.goroot, x.release = goRoot(x.cache, x.info.goVersion, x.info.toolchain)
	if err := x.indexDirs(ctx, u.dir); err != nil {
		return nil, err
	}
	var queue []string
	seen := map[string]bool{}
	err := h.decls.each(func(e entry) error {
		if !strings.HasPrefix(e.Key, keyImport) || e.Aux == "" || e.Aux == "C" || seen[e.Aux] {
			return nil
		}
		seen[e.Aux] = true
		queue = append(queue, e.Aux)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(queue)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := queue[0]
		queue = queue[1:]
		inUnit, err := h.inUnit(p)
		if err != nil {
			return nil, err
		}
		if inUnit {
			continue
		}
		pkg, err := x.load(ctx, p)
		if err != nil {
			return nil, err
		}
		x.pkgs[p] = pkg
		if pkg == nil {
			x.unavailable++
			continue
		}
		x.read++
		for _, f := range pkg.files {
			for _, paths := range f.imports {
				for _, q := range paths {
					if !seen[q] && q != "C" {
						seen[q] = true
						queue = append(queue, q)
					}
				}
			}
			for _, q := range f.dot {
				if !seen[q] {
					seen[q] = true
					queue = append(queue, q)
				}
			}
		}
	}
	return x, nil
}

// close releases the snapshot directory index.
func (x *goExternal) close() {
	if x != nil && x.dirs != nil {
		x.dirs.close()
	}
}

// indexDirs indexes the snapshot's Go files under every directory a
// workspace module or a local replacement names, in one pass over the
// snapshot, so a package there is listed by one index lookup.
func (x *goExternal) indexDirs(ctx context.Context, dir string) error {
	var roots []string
	for _, m := range x.info.uses {
		roots = append(roots, m.dir)
	}
	for _, r := range x.info.replaces {
		if r.dir != "" {
			roots = append(roots, r.dir)
		}
	}
	if len(roots) == 0 || x.content == nil {
		return nil
	}
	s, err := newEntrySort(dir)
	if err != nil {
		return err
	}
	defer s.Close()
	err = x.content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || !goSourceFile(path.Base(fv.Path)) {
			return nil
		}
		d := path.Dir(fv.Path)
		for _, r := range roots {
			if _, ok := relDir(r, d); ok {
				return s.Add(entry{Key: d, Aux: path.Base(fv.Path), Sig: string(fv.ID), Returns: fv.ContentHash})
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	x.dirs, err = writeIndex(dir, s)
	return err
}

// eachEntry calls yield with each entry of es in order.
func eachEntry(es []entry, yield func(entry) error) error {
	for _, e := range es {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// goSourceFile reports a file name the go command builds into a package:
// a .go file that is no test and that it does not ignore ("Package
// lists": names starting with "." or "_").
func goSourceFile(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") &&
		!strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_")
}

// load reads the package at import path p, nil when no source holds it.
func (x *goExternal) load(ctx context.Context, p string) (*goExtPackage, error) {
	type file struct {
		name string
		data []byte
	}
	var files []file
	readDir := func(dir string) error {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, e := range ents {
			if e.IsDir() || !goSourceFile(e.Name()) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			files = append(files, file{e.Name(), data})
		}
		return nil
	}
	std := !strings.Contains(strings.SplitN(p, "/", 2)[0], ".")
	// id is the identity of a read-only source, under which its
	// declarations are cached (goDeclCacheFile); empty for the snapshot's.
	id := ""
	switch {
	case std:
		if x.goroot == "" {
			return nil, nil
		}
		id = "std\x00" + x.release + "\x00" + x.goroot + "\x00" + p
		if pkg, ok := readGoDecls(id, p); ok {
			return pkg, nil
		}
		if err := readDir(filepath.Join(x.goroot, "src", filepath.FromSlash(p))); err != nil {
			return nil, err
		}
	default:
		src := x.info.source(p)
		switch {
		case src.missing:
			return nil, nil
		case src.dir != "":
			if x.dirs == nil {
				return nil, nil
			}
			// A snapshot package is cached by its files' names and content
			// hashes, so every unit of the build that reaches it parses it
			// once.
			var listed []entry
			if err := x.dirs.lookup(path.Clean(src.dir), func(e entry) error {
				listed = append(listed, e)
				return nil
			}); err != nil {
				return nil, err
			}
			var key strings.Builder
			key.WriteString("snap\x00" + p)
			for _, e := range listed {
				if e.Returns == "" {
					key.Reset()
					break
				}
				key.WriteString("\x00" + e.Aux + "\x00" + e.Returns)
			}
			if id = key.String(); id != "" {
				if pkg, ok := readGoDecls(id, p); ok {
					return pkg, nil
				}
			}
			err := eachEntry(listed, func(e entry) error {
				rc, fv, err := x.content.Open(ctx, model.FileID(e.Sig))
				if err != nil {
					return err
				}
				defer rc.Close()
				data, err := io.ReadAll(io.LimitReader(rc, fv.Size))
				if err != nil {
					return err
				}
				files = append(files, file{e.Aux, data})
				return nil
			})
			if err != nil {
				return nil, err
			}
		default:
			dir := goModuleDir(x.cache, src.module)
			if dir == "" {
				return nil, nil
			}
			id = "mod\x00" + dir + "\x00" + src.sub
			if pkg, ok := readGoDecls(id, p); ok {
				return pkg, nil
			}
			if err := readDir(filepath.Join(dir, filepath.FromSlash(src.sub))); err != nil {
				return nil, err
			}
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	slices.SortFunc(files, func(a, b file) int { return strings.Compare(a.name, b.name) })
	fset := token.NewFileSet()
	var parsed []*ast.File
	names := map[string]int{}
	for _, f := range files {
		af, err := parser.ParseFile(fset, f.name, f.data, parser.SkipObjectResolution)
		if err != nil || af.Name == nil {
			continue
		}
		parsed = append(parsed, af)
		names[af.Name.Name]++
	}
	// The package's name is the one most of its files declare: a file of
	// another (a generator's `package main` behind `//go:build ignore`) is
	// no file of the package.
	name := ""
	for n, c := range names {
		if c > names[name] || c == names[name] && n < name {
			name = n
		}
	}
	if name == "" {
		return nil, nil
	}
	pkg := &goExtPackage{path: p, name: name, types: map[string]*goExtType{}}
	for _, af := range parsed {
		if af.Name.Name != name {
			continue
		}
		pkg.addFile(af, std)
	}
	if id != "" {
		writeGoDecls(id, pkg)
	}
	return pkg, nil
}

// addFile reads one file's imports and declarations into p; std reports a
// standard-library package, whose imports of packages it vendors name them
// under vendor/ (GOROOT/src/vendor).
func (p *goExtPackage) addFile(af *ast.File, std bool) {
	fi := len(p.files)
	f := goExtFile{imports: map[string][]string{}}
	for _, spec := range af.Imports {
		ip := strings.Trim(spec.Path.Value, "\"`")
		if std && strings.Contains(strings.SplitN(ip, "/", 2)[0], ".") {
			ip = "vendor/" + ip
		}
		switch {
		case spec.Name == nil:
			f.imports[goPackageName(ip)] = append(f.imports[goPackageName(ip)], ip)
			if ip != "C" && !slices.Contains(f.unnamed, ip) {
				f.unnamed = append(f.unnamed, ip)
			}
		case spec.Name.Name == ".":
			f.dot = append(f.dot, ip)
		case spec.Name.Name == "_":
		default:
			f.imports[spec.Name.Name] = append(f.imports[spec.Name.Name], ip)
		}
	}
	p.files = append(p.files, f)
	for _, d := range af.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			if d.Tok == token.CONST {
				p.addConsts(d, fi)
				continue
			}
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok {
					p.addType(ts, fi)
				}
			}
		case *ast.FuncDecl:
			// A method with type parameters of its own satisfies no
			// interface (Go 1.27 release notes: interface methods are never
			// implemented by generic methods).
			if d.Recv == nil || len(d.Recv.List) != 1 || !d.Name.IsExported() || d.Type.TypeParams != nil {
				continue
			}
			base, params := goReceiverBase(d.Recv.List[0].Type)
			if base == "" {
				continue
			}
			t := p.typeNamed(base, fi)
			form := goASTSignature(d.Type, params)
			if i := slices.IndexFunc(t.methods, func(m goExtMethod) bool { return m.name == d.Name.Name }); i >= 0 {
				if t.methods[i].form != form {
					t.methods[i].form = goIdentityUnknown
				}
				continue
			}
			t.methods = append(t.methods, goExtMethod{name: d.Name.Name, form: form, file: fi})
		}
	}
}

// addConsts reads one constant declaration: each name's value is its own
// spec's, or the nearest earlier spec's with values, at iota the spec's
// index ("Constant declarations", "Iota"). A name two files declare with
// different values is "?".
func (p *goExtPackage) addConsts(d *ast.GenDecl, fi int) {
	var values []ast.Expr
	for i, spec := range d.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		if len(vs.Values) > 0 {
			values = vs.Values
		}
		for j, id := range vs.Names {
			if id.Name == "_" {
				continue
			}
			form := goIdentityUnknown
			if j < len(values) {
				var b []string
				if goASTConst(&b, values[j], i) {
					form = strings.Join(b, " ")
				}
			}
			if p.consts == nil {
				p.consts = map[string]goExtMethod{}
			}
			if old, ok := p.consts[id.Name]; ok && old.form != form {
				form = goIdentityUnknown
			}
			p.consts[id.Name] = goExtMethod{name: id.Name, form: form, file: fi}
		}
	}
}

// goASTConst appends the form of the integer constant expression e at
// iota (the worker's rule, goLink.constForm); false when the form cannot
// spell it.
func goASTConst(b *[]string, e ast.Expr, iota int) bool {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return false
		}
		v, ok := new(big.Int).SetString(e.Value, 0)
		if !ok {
			return false
		}
		*b = append(*b, v.String())
		return true
	case *ast.Ident:
		if e.Name == "iota" && iota >= 0 {
			*b = append(*b, itoaInt(iota))
			return true
		}
		*b = append(*b, "."+e.Name)
		return true
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		*b = append(*b, ":"+x.Name+"."+e.Sel.Name)
		return true
	case *ast.ParenExpr:
		*b = append(*b, "(")
		if !goASTConst(b, e.X, iota) {
			return false
		}
		*b = append(*b, ")")
		return true
	case *ast.UnaryExpr:
		switch e.Op {
		case token.ADD, token.SUB, token.XOR:
		default:
			return false
		}
		*b = append(*b, "u"+e.Op.String())
		return goASTConst(b, e.X, iota)
	case *ast.BinaryExpr:
		switch e.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.SHL, token.SHR, token.AND, token.OR,
			token.XOR, token.AND_NOT:
		default:
			return false
		}
		*b = append(*b, "(")
		if !goASTConst(b, e.X, iota) {
			return false
		}
		*b = append(*b, e.Op.String())
		if !goASTConst(b, e.Y, iota) {
			return false
		}
		*b = append(*b, ")")
		return true
	}
	return false
}

// constant is goPackageTypes.constant over the packages read.
func (x *goExternal) constant(p, name string) (string, goNames, bool) {
	pkg := x.pkgs[p]
	if pkg == nil {
		return "", nil, false
	}
	c, ok := pkg.consts[name]
	if !ok {
		return "", nil, false
	}
	return c.form, pkg.names(c.file), true
}

// typeNamed is the record of the type named name, made for a method whose
// type's declaration is in a later file.
func (p *goExtPackage) typeNamed(name string, fi int) *goExtType {
	t := p.types[name]
	if t == nil {
		t = &goExtType{file: -1}
		p.types[name] = t
	}
	return t
}

// addType reads one type declaration.
func (p *goExtPackage) addType(ts *ast.TypeSpec, fi int) {
	n := goExtType{file: fi}
	var params map[string]int
	if ts.TypeParams != nil {
		params = map[string]int{}
		for _, f := range ts.TypeParams.List {
			for _, id := range f.Names {
				params[id.Name] = len(params)
			}
		}
	}
	switch {
	case ts.Assign.IsValid():
		n.alias, n.form = true, goIdentityUnknown
		if params == nil {
			n.form = goASTForm(ts.Type, nil)
		}
	default:
		switch t := ts.Type.(type) {
		case *ast.InterfaceType:
			n.iface = true
			n.ineligible = params != nil
			for _, m := range t.Methods.List {
				ft, isMethod := m.Type.(*ast.FuncType)
				switch {
				case isMethod && len(m.Names) == 1:
					if !m.Names[0].IsExported() {
						n.ineligible = true
					}
					n.methods = append(n.methods, goExtMethod{name: m.Names[0].Name, form: goASTSignature(ft, params), file: fi})
				case len(m.Names) == 0 && goEmbeddable(m.Type):
					n.embeds = append(n.embeds, goExtMethod{form: goASTForm(m.Type, params), file: fi})
				default:
					// A type element (`~int`, `A | B`) makes a constraint,
					// which no method set satisfies.
					n.ineligible = true
				}
			}
		case *ast.StructType:
			for _, f := range t.Fields.List {
				if len(f.Names) == 0 {
					typ := f.Type
					if s, ok := typ.(*ast.StarExpr); ok {
						typ = s.X
					}
					n.embeds = append(n.embeds, goExtMethod{form: goASTForm(typ, params), file: fi})
				}
			}
		}
	}
	old := p.types[ts.Name.Name]
	switch {
	case old == nil:
		p.types[ts.Name.Name] = &n
	case old.file < 0:
		// Methods read before the declaration: keep them.
		n.methods = append(n.methods, old.methods...)
		*old = n
	case !sameExtDecl(old, &n) && !slices.ContainsFunc(old.variants, func(v *goExtType) bool { return sameExtDecl(v, &n) }):
		old.conflict = true
		old.variants = append(old.variants, &n)
	}
}

// sameExtDecl reports two declarations of one type name that declare the
// same type: alias or not, the same form, and for an interface the same
// methods and embedded interfaces.
func sameExtDecl(a, b *goExtType) bool {
	return a.alias == b.alias && a.iface == b.iface && a.form == b.form &&
		(!b.iface || slices.Equal(a.embeds, b.embeds) && slices.EqualFunc(a.methods, b.methods, sameExtMethod))
}

func sameExtMethod(a, b goExtMethod) bool { return a.name == b.name && a.form == b.form }

// goEmbeddable reports an interface element that embeds a named interface.
func goEmbeddable(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	case *ast.ParenExpr:
		return goEmbeddable(e.X)
	}
	return false
}

// goReceiverBase is a method receiver's base type name and the type
// parameters its receiver names, by position.
func goReceiverBase(e ast.Expr) (string, map[string]int) {
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	if p, ok := e.(*ast.ParenExpr); ok {
		return goReceiverBase(p.X)
	}
	var params map[string]int
	var args []ast.Expr
	switch t := e.(type) {
	case *ast.IndexExpr:
		e, args = t.X, []ast.Expr{t.Index}
	case *ast.IndexListExpr:
		e, args = t.X, t.Indices
	}
	for _, a := range args {
		if id, ok := a.(*ast.Ident); ok {
			if params == nil {
				params = map[string]int{}
			}
			params[id.Name] = len(params)
		}
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name, params
	}
	return "", nil
}

// goPackageName is the name a package imported without one binds when its
// own clause is not read: its path's last element past a major version
// (the worker's rule, goImportName), which the closure's packages correct
// once read (goExternal.importedName).
func goPackageName(p string) string {
	elems := strings.Split(p, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = elems[len(elems)-2]
	}
	if strings.HasPrefix(p, "gopkg.in/") {
		if i := strings.LastIndex(name, ".v"); i > 0 && strings.Trim(name[i+2:], "0123456789") == "" {
			name = name[:i]
		}
	}
	return name
}

// renameImports rebinds, in every file read, each import written without a
// name to the name its package's own clause declares, now that the closure
// is read: the path's last element is only the convention. The imports
// whose clause is still not known are the file's unresolved ones.
func (x *goExternal) renameImports() error {
	for _, pkg := range x.pkgs {
		if pkg == nil {
			continue
		}
		for i := range pkg.files {
			f := &pkg.files[i]
			clauses := map[string]string{}
			f.unresolved = f.unresolved[:0]
			for _, ip := range f.unnamed {
				clause, ok, err := x.h.goClause(ip)
				if err != nil {
					return err
				}
				if ok {
					clauses[ip] = clause
				} else {
					f.unresolved = append(f.unresolved, ip)
				}
			}
			slices.Sort(f.unresolved)
			renamed := map[string][]string{}
			for name, paths := range f.imports {
				for _, ip := range paths {
					n := name
					if clause, ok := clauses[ip]; ok && name == goPackageName(ip) {
						n = clause
					}
					renamed[n] = append(renamed[n], ip)
				}
			}
			f.imports = renamed
		}
	}
	return nil
}

// lookup is goPackageTypes over the packages read.
func (x *goExternal) lookup(p, name string) (goTypeAnswer, bool) {
	pkg, ok := x.pkgs[p]
	if !ok || pkg == nil {
		return goTypeAnswer{}, false
	}
	t := pkg.types[name]
	if t == nil || t.file < 0 {
		return goTypeAnswer{}, true
	}
	if t.alias {
		if t.conflict {
			return goTypeAnswer{found: true, alias: true, form: goIdentityUnknown, names: pkg.names(t.file)}, true
		}
		return goTypeAnswer{found: true, alias: true, form: t.form, names: pkg.names(t.file)}, true
	}
	return goTypeAnswer{found: true}, true
}

// materialize adds to methodSets the records of every external type the
// unit's method sets may read: every materializable interface of the
// packages read, and every exported type with methods or embedded fields,
// each under its own Ref (goExternalFile), its methods' keys canonical
// (goIdentity) and its embedded types those it embeds.
func (x *goExternal) materialize(ctx context.Context, methodSets *pagination.ExternalSort[entry]) error {
	if err := x.renameImports(); err != nil {
		return err
	}
	// An interface declared differently in several files (per platform or
	// build tag) is each of its declarations, an entity of its own under
	// its own key (q, NUL, the declaration's ordinal), as the unit's own
	// conditional declarations are: a type that satisfies one implements
	// q. Such an interface is never "the" read method set of q
	// (materialized), so a type embedding q counts it as unread.
	type named struct {
		q, key  string
		pkg     *goExtPackage
		t       *goExtType
		variant bool
	}
	var all []named
	for _, pkg := range x.pkgs {
		if pkg == nil {
			continue
		}
		for name, t := range pkg.types {
			if t.file < 0 || !token.IsExported(name) {
				continue
			}
			q := pkg.path + "." + name
			if t.conflict && t.iface || t.conflict && t.alias {
				for i, d := range append([]*goExtType{t}, t.variants...) {
					if d.iface && !d.alias {
						all = append(all, named{q: q, key: q + "\x00" + itoaInt(i), pkg: pkg, t: d, variant: true})
					}
				}
				continue
			}
			if t.alias || !t.iface && len(t.methods) == 0 && len(t.embeds) == 0 {
				continue
			}
			all = append(all, named{q: q, key: q, pkg: pkg, t: t})
		}
	}
	slices.SortFunc(all, func(a, b named) int { return strings.Compare(a.key, b.key) })
	// Canonical embedded types first, so each interface's eligibility can
	// follow its embedded interfaces.
	embeds := map[string][]string{}
	byName := map[string]named{}
	for _, n := range all {
		byName[n.key] = n
		for _, e := range n.t.embeds {
			c, err := goIdentity(ctx, x.h.goTypes(), n.pkg.names(e.file), e.form)
			if err != nil {
				return err
			}
			embeds[n.key] = append(embeds[n.key], c)
		}
	}
	eligible := map[string]bool{"error": true}
	var check func(key string, depth int) bool
	check = func(key string, depth int) bool {
		if ok, done := eligible[key]; done {
			return ok
		}
		n, found := byName[key]
		if !found || !n.t.iface || n.t.ineligible || n.t.conflict && !n.variant || depth > goAliasDepth {
			return false
		}
		eligible[key] = false
		for _, e := range embeds[key] {
			if !check(e, depth+1) {
				return false
			}
		}
		eligible[key] = true
		return true
	}
	ref := func(key, q string) Ref {
		r, ok := x.refs[key]
		if !ok {
			r = Ref{File: goExternalFile, Index: int32(len(x.names))}
			x.names = append(x.names, q)
			x.refs[key] = r
		}
		return r
	}
	// The predeclared interface error, Error() string ("Errors").
	errRef := ref("error", "error")
	for _, e := range []entry{
		{Key: refKey(errRef), Tag: goTypeRecord, Flags: flagAbstract, Aux: "error"},
		{Key: refKey(errRef), Tag: goMethodRecord, Ref: errRef, Aux: "Error", Returns: "Error\x00( ) ( string )"},
	} {
		if err := methodSets.Add(e); err != nil {
			return err
		}
	}
	for _, n := range all {
		if n.t.iface && !check(n.key, 0) {
			continue
		}
		ref(n.key, n.q)
	}
	for _, n := range all {
		r, ok := x.refs[n.key]
		if !ok {
			continue
		}
		e := entry{Key: refKey(r), Tag: goTypeRecord, Aux: n.q}
		if n.t.iface {
			e.Flags = flagAbstract
		}
		if err := methodSets.Add(e); err != nil {
			return err
		}
		for _, m := range n.t.methods {
			if n.t.conflict && !n.t.iface {
				continue
			}
			key, _, err := goMethodKey(ctx, x.h.goTypes(), n.pkg.names(m.file), m.name, m.form, r)
			if err != nil {
				return err
			}
			if err := methodSets.Add(entry{Key: refKey(r), Tag: goMethodRecord, Ref: r, Aux: m.name, Returns: key}); err != nil {
				return err
			}
		}
		for _, c := range embeds[n.key] {
			if er, ok := x.refs[c]; ok {
				if err := methodSets.Add(entry{Key: refKey(r), Tag: goEmbedRecord, Ref: er}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// materialized is the Ref of the external type of qualified name q whose
// method set the unit's records hold.
func (x *goExternal) materialized(q string) (Ref, bool) {
	if x == nil {
		return Ref{}, false
	}
	r, ok := x.refs[q]
	return r, ok
}

// qualifiedOf is the qualified name of the materialized type at r.
func (x *goExternal) qualifiedOf(r Ref) string {
	if x == nil || r.File != goExternalFile || r.Index < 0 || int(r.Index) >= len(x.names) {
		return ""
	}
	return x.names[r.Index]
}

// goASTSignature is the identity form of a function type (the worker's
// rule, goForm.signature).
func goASTSignature(ft *ast.FuncType, params map[string]int) string {
	var b []string
	ok := goASTList(&b, ft.Params, params)
	if ft.Results == nil || len(ft.Results.List) == 0 {
		b = append(b, "(", ")")
	} else if !goASTList(&b, ft.Results, params) {
		ok = false
	}
	if !ok {
		return goIdentityUnknown
	}
	return strings.Join(b, " ")
}

func goASTList(b *[]string, l *ast.FieldList, params map[string]int) bool {
	*b = append(*b, "(")
	ok := true
	first := true
	if l != nil {
		for _, f := range l.List {
			for range max(len(f.Names), 1) {
				if !first {
					*b = append(*b, ",")
				}
				first = false
				typ := f.Type
				if e, isVariadic := typ.(*ast.Ellipsis); isVariadic {
					*b = append(*b, "...")
					typ = e.Elt
				}
				if !goASTType(b, typ, params) {
					ok = false
				}
			}
		}
	}
	*b = append(*b, ")")
	return ok
}

// goASTForm is the identity form of the type e.
func goASTForm(e ast.Expr, params map[string]int) string {
	var b []string
	if !goASTType(&b, e, params) {
		return goIdentityUnknown
	}
	return strings.Join(b, " ")
}

func goASTType(b *[]string, e ast.Expr, params map[string]int) bool {
	add := func(t ...string) { *b = append(*b, t...) }
	switch t := e.(type) {
	case *ast.ParenExpr:
		return goASTType(b, t.X, params)
	case *ast.Ident:
		if i, ok := params[t.Name]; ok {
			add("$" + itoaInt(i))
			return true
		}
		add("." + t.Name)
		return true
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		if !ok {
			return false
		}
		add(":" + x.Name + "." + t.Sel.Name)
		return true
	case *ast.StarExpr:
		add("*")
		return goASTType(b, t.X, params)
	case *ast.ArrayType:
		switch l := t.Len.(type) {
		case nil:
			add("[", "]")
		case *ast.BasicLit:
			if l.Kind != token.INT {
				return false
			}
			v, ok := new(big.Int).SetString(l.Value, 0)
			if !ok {
				return false
			}
			add("[", v.String(), "]")
		case *ast.Ellipsis:
			return false
		default:
			add("[#")
			if !goASTConst(b, l, -1) {
				return false
			}
			add("]")
		}
		return goASTType(b, t.Elt, params)
	case *ast.MapType:
		add("map", "[")
		if !goASTType(b, t.Key, params) {
			return false
		}
		add("]")
		return goASTType(b, t.Value, params)
	case *ast.ChanType:
		switch t.Dir {
		case ast.SEND:
			add("chan", "<-")
		case ast.RECV:
			add("<-", "chan")
		default:
			add("chan")
		}
		return goASTType(b, t.Value, params)
	case *ast.FuncType:
		add("func")
		s := goASTSignature(t, params)
		if s == goIdentityUnknown {
			return false
		}
		add(strings.Split(s, " ")...)
		return true
	case *ast.StructType:
		return goASTStruct(b, t, params)
	case *ast.InterfaceType:
		return goASTInterface(b, t, params)
	case *ast.IndexExpr:
		return goASTGeneric(b, t.X, []ast.Expr{t.Index}, params)
	case *ast.IndexListExpr:
		return goASTGeneric(b, t.X, t.Indices, params)
	}
	return false
}

// goASTStruct appends the form of a struct type (the worker's rule,
// goForm.structType).
func goASTStruct(b *[]string, t *ast.StructType, params map[string]int) bool {
	if t.Fields == nil || len(t.Fields.List) == 0 {
		*b = append(*b, "struct{}")
		return true
	}
	first := true
	for _, f := range t.Fields.List {
		tag := ""
		if f.Tag != nil {
			tok, ok := goTagToken(f.Tag.Value)
			if !ok {
				return false
			}
			tag = tok
		}
		field := func(prefix ...string) bool {
			if first {
				*b = append(*b, "struct{")
			} else {
				*b = append(*b, ";")
			}
			first = false
			*b = append(*b, prefix...)
			typ := f.Type
			if len(f.Names) == 0 {
				if s, ok := typ.(*ast.StarExpr); ok {
					typ = s.X
				}
			}
			if !goASTType(b, typ, params) {
				return false
			}
			if tag != "" {
				*b = append(*b, tag)
			}
			return true
		}
		if len(f.Names) == 0 {
			prefix := []string{"^"}
			if _, ok := f.Type.(*ast.StarExpr); ok {
				prefix = append(prefix, "*")
			}
			if !field(prefix...) {
				return false
			}
			continue
		}
		for _, n := range f.Names {
			if !field("@" + n.Name) {
				return false
			}
		}
	}
	*b = append(*b, "}")
	return true
}

// goASTInterface appends the form of an interface type (the worker's rule,
// goForm.interfaceType).
func goASTInterface(b *[]string, t *ast.InterfaceType, params map[string]int) bool {
	if t.Methods == nil || len(t.Methods.List) == 0 {
		*b = append(*b, "interface{}")
		return true
	}
	for i, m := range t.Methods.List {
		if i == 0 {
			*b = append(*b, "interface{")
		} else {
			*b = append(*b, ";")
		}
		ft, isMethod := m.Type.(*ast.FuncType)
		switch {
		case isMethod && len(m.Names) == 1:
			*b = append(*b, "@"+m.Names[0].Name)
			s := goASTSignature(ft, params)
			if s == goIdentityUnknown {
				return false
			}
			*b = append(*b, strings.Split(s, " ")...)
		case len(m.Names) == 0 && !goTypeElement(m.Type):
			*b = append(*b, "^")
			if !goASTType(b, m.Type, params) {
				return false
			}
		default:
			return false
		}
	}
	*b = append(*b, "}")
	return true
}

// goTypeElement reports an interface element that is a constraint's type
// element (`~T`, `A | B`), not an embedded interface.
func goTypeElement(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.UnaryExpr, *ast.BinaryExpr:
		return true
	case *ast.ParenExpr:
		return goTypeElement(e.X)
	}
	return false
}

// goTagToken is the token of a struct field's tag written as the string
// literal lit (the worker's rule): `#` and its value quoted, spaces escaped
// so the token holds none.
func goTagToken(lit string) (string, bool) {
	v, err := strconv.Unquote(lit)
	if err != nil {
		return "", false
	}
	return "#" + strings.ReplaceAll(strconv.Quote(v), " ", `\x20`), true
}

func goASTGeneric(b *[]string, base ast.Expr, args []ast.Expr, params map[string]int) bool {
	if !goASTType(b, base, params) {
		return false
	}
	*b = append(*b, "[")
	for i, a := range args {
		if i > 0 {
			*b = append(*b, ",")
		}
		if !goASTType(b, a, params) {
			return false
		}
	}
	*b = append(*b, "]")
	return true
}

func itoaInt(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoaInt(i/10) + string(rune('0'+i%10))
}
