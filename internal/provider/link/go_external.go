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

	"github.com/Sawmonabo/codectx/internal/gobuild"
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
// read: every non-test file, whatever its build constraints, each with the
// build condition its constraint and name state (gobuild.File,
// go_build.go), and a declaration, method or constant several files write
// alike is in the builds of any of them (goExtType.also, goExtMethod.also).
// A name two files declare differently (per platform or build tag,
// goExtType.variants) is each of its declarations: an interface declared so
// is one entity per declaration, under its condition, as the unit's
// conditional declarations are; a defined type declared so is one entity in
// the builds of any of its declarations, each declaration's embedded types
// under that declaration's condition and each method under its own file's. Declarations written
// differently are compared by their canonical spellings: an interface whose
// declarations spell one method set is that one, under any of their
// conditions; a method declared per build is each of its keys, under the
// conditions of the declarations that spell it; and a signature naming an
// alias or constant declared per build is one key per choice of
// declaration (goMethodKeys). An interface is
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
	// conds are the build conditions of the files read (fileCond).
	conds map[*goExtFile]goCond
}

// goExtPackage is one package's declarations.
type goExtPackage struct {
	path  string
	name  string
	files []goExtFile
	types map[string]*goExtType
	// consts are the package-level constants by name: each distinctly
	// written form of one's integer value (goASTConst, one per platform or
	// build tag that writes it differently) and its file.
	consts map[string][]goExtMethod
}

// goExtFile is one file's imports: by local name, and its dot imports;
// unnamed are the paths it imports without a name, and unresolved those of
// them whose package clause is not read (renameImports).
type goExtFile struct {
	// constraint is the file's build constraint (gobuild.File), "" when it
	// states none.
	constraint string
	imports    map[string][]string
	dot        []string
	unnamed    []string
	unresolved []string
}

// goExtType is one type name a package declares.
type goExtType struct {
	file int
	// alias is set for `type A = T`, its form T's (for a generic alias
	// `$$n` and T's form over its n parameters).
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
	// also are the other files declaring this same declaration (one per
	// platform or build tag that writes it alike).
	also []int
	// methods are an interface declaration's methods. recv, on a name's
	// first declaration, are the methods declared with the name as their
	// receiver's base type, each distinctly written signature of a name
	// declared once per platform or build tag (several entries of that
	// name): a defined type's in the builds it is one.
	methods []goExtMethod
	recv    []goExtMethod
	embeds  []goExtMethod
}

// goExtMethod is one method (name and form), constant (name and form) or
// embedded type (form), with the file its form is written in and the other
// files that declare it alike (one per platform or build tag).
type goExtMethod struct {
	name, form string
	file       int
	also       []int
}

// fileCond is the build condition of pkg's file i, 0 outside a hierarchy
// build.
func (x *goExternal) fileCond(pkg *goExtPackage, i int) goCond {
	c := x.h.goConds
	if c == nil || i < 0 || i >= len(pkg.files) {
		return 0
	}
	f := &pkg.files[i]
	if id, ok := x.conds[f]; ok {
		return id
	}
	id := c.parse(f.constraint)
	x.conds[f] = id
	return id
}

// declCond is the build condition of the type declaration t: the
// disjunction of the conditions of every file that declares it.
func (x *goExternal) declCond(pkg *goExtPackage, t *goExtType) goCond {
	return x.filesCond(pkg, t.file, t.also)
}

// memberCond is the build condition of the method, constant or embedded
// type m: the disjunction of the conditions of every file that declares it.
func (x *goExternal) memberCond(pkg *goExtPackage, m goExtMethod) goCond {
	return x.filesCond(pkg, m.file, m.also)
}

func (x *goExternal) filesCond(pkg *goExtPackage, file int, also []int) goCond {
	cond := x.fileCond(pkg, file)
	for _, f := range also {
		cond = x.h.goConds.or(cond, x.fileCond(pkg, f))
	}
	return cond
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
	x := &goExternal{h: h, info: h.goInfo, content: u.content, pkgs: map[string]*goExtPackage{}, refs: map[string]Ref{},
		conds: map[*goExtFile]goCond{}}
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
			key.WriteString("snap\x00")
			key.WriteString(p)
			for _, e := range listed {
				if e.Returns == "" {
					key.Reset()
					break
				}
				key.WriteByte(0)
				key.WriteString(e.Aux)
				key.WriteByte(0)
				key.WriteString(e.Returns)
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
	dataOf := map[*ast.File][]byte{}
	names := map[string]int{}
	for _, f := range files {
		af, err := parser.ParseFile(fset, f.name, f.data, parser.SkipObjectResolution)
		if err != nil || af.Name == nil {
			continue
		}
		parsed = append(parsed, af)
		dataOf[af] = f.data
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
		pkg.addFile(af, std, gobuild.File(path.Base(fset.File(af.Pos()).Name()), dataOf[af]))
	}
	if id != "" {
		writeGoDecls(id, pkg)
	}
	return pkg, nil
}

// addFile reads one file's imports and declarations into p; std reports a
// standard-library package, whose imports of packages it vendors name them
// under vendor/ (GOROOT/src/vendor).
func (p *goExtPackage) addFile(af *ast.File, std bool, constraint string) {
	fi := len(p.files)
	f := goExtFile{constraint: constraint, imports: map[string][]string{}}
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
			t := p.typeNamed(base)
			form := goASTSignature(d.Type, params)
			t.recv = addExtMember(t.recv, goExtMethod{name: d.Name.Name, form: form, file: fi})
		}
	}
}

// addConsts reads one constant declaration: each name's value is its own
// spec's, or the nearest earlier spec's with values, at iota the spec's
// index ("Constant declarations", "Iota"). A name two files declare with
// different forms keeps each (goExtPackage.consts).
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
				p.consts = map[string][]goExtMethod{}
			}
			p.consts[id.Name] = addExtMember(p.consts[id.Name], goExtMethod{name: id.Name, form: form, file: fi})
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
func (x *goExternal) constant(p, name string) []goDecl {
	pkg := x.pkgs[p]
	if pkg == nil {
		return nil
	}
	var decls []goDecl
	for _, c := range pkg.consts[name] {
		decls = append(decls, goDecl{form: c.form, names: pkg.names(c.file), cond: x.memberCond(pkg, c)})
	}
	return decls
}

// typeNamed is the record of the type named name, made for a method whose
// type's declaration is in a later file.
func (p *goExtPackage) typeNamed(name string) *goExtType {
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
		n.alias, n.form = true, goASTForm(ts.Type, params)
		if len(params) > 0 && n.form != goIdentityUnknown {
			// A generic alias (the worker's rule, aliasExport).
			n.form = "$$" + itoaInt(len(params)) + " " + n.form
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
		n.recv = old.recv
		*old = n
	case sameExtDecl(old, &n):
		old.also = append(old.also, fi)
	default:
		if i := slices.IndexFunc(old.variants, func(v *goExtType) bool { return sameExtDecl(v, &n) }); i >= 0 {
			old.variants[i].also = append(old.variants[i].also, fi)
			return
		}
		old.conflict = true
		old.variants = append(old.variants, &n)
	}
}

// addExtMember adds m to ms, or, when one of ms has its name and form, m's
// file to that one's.
func addExtMember(ms []goExtMethod, m goExtMethod) []goExtMethod {
	i := slices.IndexFunc(ms, func(o goExtMethod) bool { return sameExtMethod(o, m) })
	if i < 0 {
		return append(ms, m)
	}
	if ms[i].file != m.file && !slices.Contains(ms[i].also, m.file) {
		ms[i].also = append(ms[i].also, m.file)
	}
	return ms
}

// sameExtDecl reports two declarations of one type name that declare the
// same type as far as method sets go: alias or not, the same form, the same
// embedded types as written, and for an interface the same methods.
func sameExtDecl(a, b *goExtType) bool {
	return a.alias == b.alias && a.iface == b.iface && a.form == b.form &&
		slices.EqualFunc(a.embeds, b.embeds, sameExtMethod) &&
		(!b.iface || slices.EqualFunc(a.methods, b.methods, sameExtMethod))
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
		a := goTypeAnswer{found: true, alias: true, form: t.form, names: pkg.names(t.file), cond: x.declCond(pkg, t)}
		for _, v := range t.variants {
			if !v.alias {
				// An alias in one build and a defined type in another.
				return goTypeAnswer{found: true, alias: true, form: goIdentityUnknown, names: pkg.names(t.file)}, true
			}
			a.others = append(a.others, goDecl{form: v.form, names: pkg.names(v.file), cond: x.declCond(pkg, v)})
		}
		return a, true
	}
	if slices.ContainsFunc(t.variants, func(v *goExtType) bool { return v.alias }) {
		// A defined type in one build and an alias in another.
		return goTypeAnswer{found: true, alias: true, form: goIdentityUnknown, names: pkg.names(t.file)}, true
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
	// Declarations written differently that spell the same method set
	// canonically (a signature naming a type by an alias in one and by
	// the aliased type in another) are one declaration: merged marks an
	// interface all of whose declarations are, which is then q itself.
	type named struct {
		q, key          string
		pkg             *goExtPackage
		t               *goExtType
		variant, merged bool
		// iface marks an interface entity; decls are the declarations
		// whose members it holds, each under its own condition.
		iface bool
		decls []*goExtType
		// cond is the build condition of the declaration (of any of
		// them, merged), and defined that of the defined type's
		// declarations, whose receiver methods it holds (none, 1, for
		// an interface entity or a name some build declares as an alias).
		cond, defined goCond
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
			decls := append([]*goExtType{t}, t.variants...)
			alias := slices.ContainsFunc(decls, func(d *goExtType) bool { return d.alias })
			iface := slices.ContainsFunc(decls, func(d *goExtType) bool { return d.iface })
			if t.conflict && (iface || alias) {
				var seen []string
				var distinct []named
				for i, d := range decls {
					if !d.iface || d.alias {
						continue
					}
					set, spelled, err := x.methodSetSpelling(ctx, pkg, d)
					if err != nil {
						return err
					}
					if !spelled || !slices.Contains(seen, set) {
						seen = append(seen, set)
						distinct = append(distinct, named{q: q, key: q + "\x00" + itoaInt(i), pkg: pkg, t: d, variant: true,
							iface: true, decls: []*goExtType{d}, cond: x.declCond(pkg, d), defined: goCondNone})
					}
				}
				if len(distinct) == 1 && !slices.ContainsFunc(decls, func(d *goExtType) bool { return d.alias || !d.iface }) {
					distinct[0].key, distinct[0].variant, distinct[0].merged = q, false, true
					for _, v := range t.variants {
						distinct[0].cond = x.h.goConds.or(distinct[0].cond, x.declCond(pkg, v))
					}
				}
				all = append(all, distinct...)
				if !slices.ContainsFunc(decls, func(d *goExtType) bool { return !d.iface && !d.alias }) {
					continue
				}
				// A name that is a defined type in some builds and an
				// interface in others is, under q, the type a use of it
				// names in each build: the defined type's receiver methods
				// and embedded types in the builds of its declarations,
				// the interface's methods and embedded interfaces in those
				// of each interface declaration. Each interface
				// declaration is also an entity of its own, which types
				// implement. A name some build declares as an alias has no
				// one method set: only its embedded types stay.
				cond, defined := goCondNone, goCondNone
				var held []*goExtType
				for _, d := range decls {
					if d.alias {
						continue
					}
					cond = x.h.goConds.or(cond, x.declCond(pkg, d))
					if !d.iface {
						defined = x.h.goConds.or(defined, x.declCond(pkg, d))
					}
					held = append(held, d)
				}
				if alias {
					defined = goCondNone
				}
				all = append(all, named{q: q, key: q, pkg: pkg, t: t, decls: held, cond: cond, defined: defined})
				continue
			}
			if t.alias || !t.iface && len(t.recv) == 0 && !slices.ContainsFunc(decls, func(d *goExtType) bool { return len(d.embeds) > 0 }) {
				continue
			}
			// A defined type declared with a different structure per build
			// (each declaration's embedded types its own) is one type in
			// the builds of any of its declarations.
			cond := goCondNone
			for _, d := range decls {
				cond = x.h.goConds.or(cond, x.declCond(pkg, d))
			}
			n := named{q: q, key: q, pkg: pkg, t: t, iface: t.iface, decls: decls, cond: cond, defined: cond}
			if t.iface {
				n.decls, n.defined = []*goExtType{t}, goCondNone
			}
			all = append(all, n)
		}
	}
	slices.SortFunc(all, func(a, b named) int { return strings.Compare(a.key, b.key) })
	// Canonical embedded types first, so each interface's eligibility can
	// follow its embedded interfaces. A struct's are each declaration's,
	// under that declaration's condition; an interface's are its one
	// declaration's (each variant an entity of its own), under its.
	type embedded struct {
		canonical string
		cond      goCond
	}
	embeds := map[string][]embedded{}
	byName := map[string]named{}
	for _, n := range all {
		byName[n.key] = n
		for _, d := range n.decls {
			cond := n.cond
			if len(n.decls) > 1 {
				cond = x.declCond(n.pkg, d)
			}
			for _, e := range d.embeds {
				c, err := goIdentity(ctx, x.h.goTypes(), n.pkg.names(e.file), e.form)
				if err != nil {
					return err
				}
				embeds[n.key] = append(embeds[n.key], embedded{c, cond})
			}
		}
	}
	eligible := map[string]bool{"error": true}
	var check func(key string, depth int) bool
	check = func(key string, depth int) bool {
		if ok, done := eligible[key]; done {
			return ok
		}
		n, found := byName[key]
		if !found || !n.iface || n.t.ineligible || n.t.conflict && !n.variant && !n.merged || depth > goAliasDepth {
			return false
		}
		eligible[key] = false
		for _, e := range embeds[key] {
			if !check(e.canonical, depth+1) {
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
		if n.iface && !check(n.key, 0) {
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
		if n.iface {
			e.Flags = flagAbstract
		}
		if err := methodSets.Add(e); err != nil {
			return err
		}
		// A defined type's methods attach by receiver name, so a type
		// declared per build with different structures has them in the
		// builds of any of its declarations; a defined type declared as an
		// alias in another file has no one method set (defined none).
		// An interface declaration's are its own, under its condition.
		var keys []goKeyed
		if n.defined != goCondNone {
			ks, err := x.methodKeys(ctx, n.pkg, n.t.recv, true, r, n.defined)
			if err != nil {
				return err
			}
			keys = append(keys, ks...)
		}
		for _, d := range n.decls {
			if !d.iface {
				continue
			}
			cond := n.cond
			if len(n.decls) > 1 {
				cond = x.declCond(n.pkg, d)
			}
			ks, err := x.methodKeys(ctx, n.pkg, d.methods, false, r, cond)
			if err != nil {
				return err
			}
			keys = append(keys, ks...)
		}
		for _, m := range keys {
			err := methodSets.Add(entry{Key: refKey(r), Tag: goMethodRecord, Ref: r, Aux: m.name, Returns: m.key, MaxArgs: m.cond})
			if err != nil {
				return err
			}
		}
		for _, e := range embeds[n.key] {
			if er, ok := x.refs[e.canonical]; ok {
				if err := methodSets.Add(entry{Key: refKey(r), Tag: goEmbedRecord, Ref: er, MaxArgs: e.cond}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// methodKeys are the method-set keys (goMethodKeys) of methods, a defined
// type's receiver methods or an interface declaration's methods, at r, in
// declaration order, each with the build condition of the builds it is part
// of: the declaration's, cond, and for a defined type's method its own
// file's. A method declared once per platform or build tag is each of its
// keys under the conditions of the declarations that spell it, and a key no
// build holds is dropped.
func (x *goExternal) methodKeys(ctx context.Context, pkg *goExtPackage, methods []goExtMethod, defined bool, r Ref, cond goCond) ([]goKeyed, error) {
	c := x.h.goConds
	out := make([]goKeyed, 0, len(methods))
	// at indexes out by name and key once a type has more methods than a
	// scan of out costs less than a map's upkeep.
	var at map[[2]string]int
	if len(methods) > 32 {
		at = make(map[[2]string]int, len(methods))
	}
	for _, m := range methods {
		base := cond
		if defined {
			base = c.and(cond, x.memberCond(pkg, m))
			if !c.holds(base) {
				continue
			}
		}
		err := x.h.goMethodKeys(ctx, x.h.goTypes(), pkg.names(m.file), m.name, m.form, r,
			func(key string, _ goSpelling, keyCond goCond) error {
				kc := c.and(base, keyCond)
				if !c.holds(kc) {
					return nil
				}
				i, seen := -1, false
				if at != nil {
					i, seen = at[[2]string{m.name, key}]
				} else if i = slices.IndexFunc(out, func(o goKeyed) bool { return o.name == m.name && o.key == key }); i >= 0 {
					seen = true
				}
				if seen {
					out[i].cond = c.or(out[i].cond, kc)
					return nil
				}
				if at != nil {
					at[[2]string{m.name, key}] = len(out)
				}
				out = append(out, goKeyed{name: m.name, key: key, cond: kc})
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// goKeyed is a method's name and method-set key, and the build condition
// under which the method has that key.
type goKeyed struct {
	name, key string
	cond      goCond
}

// methodSetSpelling is the canonical spelling of the interface declaration
// t's method set: its methods' canonical signatures and its embedded
// types', each sorted; spelled is false when one cannot be spelled, which
// makes the declaration equal to no other.
func (x *goExternal) methodSetSpelling(ctx context.Context, pkg *goExtPackage, t *goExtType) (set string, spelled bool, err error) {
	var ms, es []string
	for _, m := range t.methods {
		id, err := goIdentity(ctx, x.h.goTypes(), pkg.names(m.file), m.form)
		if err != nil {
			return "", false, err
		}
		if id == goIdentityUnknown {
			return "", false, nil
		}
		ms = append(ms, m.name+"\x00"+id)
	}
	for _, e := range t.embeds {
		c, err := goIdentity(ctx, x.h.goTypes(), pkg.names(e.file), e.form)
		if err != nil {
			return "", false, err
		}
		if c == goIdentityUnknown {
			return "", false, nil
		}
		es = append(es, c)
	}
	slices.Sort(ms)
	slices.Sort(es)
	return strings.Join(ms, "\x01") + "\x02" + strings.Join(es, "\x01") + "\x02" + flagString(t.ineligible), true, nil
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
