package treesitter

import (
	"cmp"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// pendingFastAPI records, for the linker, a FastAPI handler parameter whose
// annotation's class this file does not decide (link.PendingInput): the
// annotation, past a None member of a union and an Optional, is one class
// named through an import (`item: Item` after `from .schemas import Item`,
// `schemas.Item` after `from . import schemas`), or a class of this file
// whose kind a base it imports decides. Any other annotation the file does
// not decide publishes nothing.
func (s *inputScan) pendingFastAPI(owner int, p, typ paramRange, name string, path bool) {
	m, ok := s.soleMember(typ)
	if !ok {
		return
	}
	ref, ok := s.classRef(m)
	if !ok {
		return
	}
	rng, err := s.b.rangeOf(s.toks[p.lo].start, s.toks[p.hi-1].end)
	if err != nil {
		return
	}
	text := collapse(string(s.b.src[s.toks[p.lo].start:s.toks[p.hi-1].end]))
	s.b.pendingInputs = append(s.b.pendingInputs, link.PendingInput{Owner: int32(owner), Range: *rng,
		Name: text, Framework: fwFastAPI, Param: name, Path: path, Class: ref})
}

// soleMember is the one member of an annotation that is not None, through
// `X | None`, Optional[X] and Union[X, None].
func (s *inputScan) soleMember(r paramRange) (paramRange, bool) {
	var members []paramRange
	lo := r.lo
	for k := r.lo; k <= r.hi; k++ {
		if k < r.hi && s.match[k] > 0 {
			k = int(s.match[k])
			continue
		}
		if k == r.hi || s.punct(k, '|') {
			if k > lo {
				members = append(members, paramRange{lo, k})
			}
			lo = k + 1
		}
	}
	var out []paramRange
	for _, m := range members {
		head, open := s.pyHead(m)
		switch {
		case head == "None" && m.hi == m.lo+1:
		case (head == "Optional" || head == "Union") && open >= 0 && int(s.match[open]) == m.hi-1:
			for _, a := range s.subscript(open) {
				if inner, ok := s.soleMember(a); ok {
					out = append(out, inner)
				} else if h, _ := s.pyHead(a); h != "None" {
					return paramRange{}, false
				}
			}
		default:
			out = append(out, m)
		}
	}
	if len(out) != 1 {
		return paramRange{}, false
	}
	return out[0], true
}

// classRef is the class a name or dotted name annotation names, as the
// linker looks it up (link.PendingInput.Class): through the import that
// binds its head, or, for a class this file declares once at module level,
// this file's own module.
func (s *inputScan) classRef(m paramRange) (string, bool) {
	var parts []string
	for k := m.lo; k < m.hi; k += 2 {
		if !s.ident(k) || k+1 < m.hi && !s.punct(k+1, '.') {
			return "", false
		}
		parts = append(parts, s.text(k))
	}
	if len(parts) == 0 {
		return "", false
	}
	if len(parts) == 1 {
		if d, ok := s.moduleClass(parts[0]); ok {
			return s.ownModule() + "\x00" + s.b.decls[d].Name, true
		}
	}
	return s.importedName(parts)
}

// importedName is the module and name a dotted name whose head an import
// binds denotes, joined by a NUL: `C` after `from m import C`, `m.C` after
// `import m`, `views.C` after `from pkg import views`.
func (s *inputScan) importedName(parts []string) (string, bool) {
	imp, ok := s.rs.imports[parts[0]]
	if !ok || imp.path == "" {
		return "", false
	}
	from := s.rs.fromImport(parts[0])
	if len(parts) == 1 {
		if !from {
			return "", false
		}
		return imp.path + "\x00" + cmp.Or(imp.original, parts[0]), true
	}
	mod := imp.path
	if from {
		name := cmp.Or(imp.original, parts[0])
		if strings.Trim(mod, ".") == "" {
			mod += name
		} else {
			mod += "." + name
		}
	} else if imp.original != "" && imp.original != "*" {
		return "", false
	}
	if middle := parts[1 : len(parts)-1]; len(middle) > 0 {
		mod += "." + strings.Join(middle, ".")
	}
	return mod + "\x00" + parts[len(parts)-1], true
}

// ownModule is this file's module as a relative import from the file
// itself writes it: `.stem`, or `.` for a package's `__init__.py`.
func (s *inputScan) ownModule() string {
	stem := strings.TrimSuffix(path.Base(s.b.fv.Path), path.Ext(s.b.fv.Path))
	if stem == "__init__" {
		return "."
	}
	return "." + stem
}

// moduleClass is the one class this file declares at module level under
// name.
func (s *inputScan) moduleClass(name string) (int, bool) {
	found := -1
	for _, i := range s.b.byName[name] {
		d := &s.b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeClass {
			continue
		}
		if found >= 0 {
			return -1, false
		}
		found = i
	}
	return found, found >= 0
}

// pyClassKinds is what each module-level class of a Python file says, by
// itself and through the classes of the file it extends, of how FastAPI
// reads a parameter it types (link.ClassKind), from its decorators and the
// bases the worker sent: a dataclass, a Pydantic model, a TypedDict or a
// NamedTuple is a body; FastAPI's and Starlette's Request, WebSocket and
// HTTPConnection the request; UploadFile a file; a Response or
// BackgroundTasks a special type; Enum, Generic, Protocol, ABC and object
// say nothing, nor do a builtin scalar or exception; a builtin sequence or
// mapping is a body. A base another module declares is left to the linker
// (Bases); a class with a base the file cannot read, or whose bases
// disagree, is not recorded, so a parameter it types publishes nothing.
func (b *builder) pyClassKinds() []link.ClassKind {
	if b.lang.Name != "python" || b.ex.link == nil || len(b.ex.link.Supertypes) == 0 && !b.hasClassDecorators() {
		return nil
	}
	s := &inputScan{b: b, rs: newImportScan(b)}
	bases := map[string][]string{}
	for _, st := range b.ex.link.Supertypes {
		bases[st.Type] = append(bases[st.Type], st.Super)
	}
	memo := map[int]*link.ClassKind{}
	var out []link.ClassKind
	for i := range b.decls {
		d := &b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeClass {
			continue
		}
		if _, ok := s.moduleClass(d.Name); !ok || s.classesNamed(d.Name) != 1 {
			// The worker names a base's class by its name alone: two
			// classes of one name cannot be told apart.
			continue
		}
		if c := s.classKind(i, bases, memo, nil); c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// classesNamed is how many classes of the file, at any depth, are named
// name.
func (s *inputScan) classesNamed(name string) int {
	n := 0
	for _, i := range s.b.byName[name] {
		if s.b.decls[i].kind == model.NodeClass {
			n++
		}
	}
	return n
}

// hasClassDecorators reports a decorated class, which may be a dataclass
// with no base.
func (b *builder) hasClassDecorators() bool {
	for i := range b.decls {
		if b.decls[i].kind == model.NodeClass && len(b.decls[i].Decorators) > 0 {
			return true
		}
	}
	return false
}

// newImportScan is a route scan holding only the file's imports.
func newImportScan(b *builder) *routeScan {
	rs := &routeScan{b: b, link: b.ex.link, imports: map[string]routeImport{}, frameworks: map[string]bool{},
		objs: map[int]*routeObj{}}
	rs.routeImports()
	return rs
}

// classKind is class i's ClassKind, nil when it is not decidable here.
func (s *inputScan) classKind(i int, bases map[string][]string, memo map[int]*link.ClassKind, way []int) *link.ClassKind {
	if c, ok := memo[i]; ok {
		return c
	}
	if slices.Contains(way, i) {
		return nil
	}
	d := &s.b.decls[i]
	c := &link.ClassKind{Decl: int32(i), Kind: link.ClassOpen}
	for _, dec := range d.Decorators {
		if dec.Name == "dataclass" || strings.HasSuffix(dec.Name, ".dataclass") {
			c.Kind = link.ClassBody
		}
	}
	ok := true
	join := func(k uint8) {
		switch {
		case k == link.ClassOpen:
		case c.Kind == link.ClassOpen || c.Kind == k:
			c.Kind = k
		default:
			ok = false
		}
	}
	for _, sup := range bases[d.Name] {
		parts := strings.Split(sup, ".")
		if len(parts) == 1 {
			if j, in := s.moduleClass(sup); in && s.classesNamed(sup) == 1 {
				sub := s.classKind(j, bases, memo, append(way, i))
				if sub == nil {
					ok = false
					break
				}
				join(sub.Kind)
				c.Bases = append(c.Bases, sub.Bases...)
				continue
			}
			if _, imported := s.rs.imports[sup]; !imported {
				if k, known := pyBuiltinBase(sup); known {
					join(k)
					continue
				}
				ok = false
				break
			}
		}
		ref, imported := s.importedName(parts)
		if !imported {
			ok = false
			break
		}
		if k, known := pyLibraryBase(ref); known {
			join(k)
			continue
		}
		c.Bases = append(c.Bases, ref)
	}
	if !ok {
		c = nil
	} else {
		slices.Sort(c.Bases)
		c.Bases = slices.Compact(c.Bases)
	}
	memo[i] = c
	return c
}

// pyBuiltinBase is the kind of a builtin base class: a sequence or a
// mapping is a body, a scalar, an exception and object say nothing.
func pyBuiltinBase(name string) (uint8, bool) {
	switch name {
	case "list", "tuple", "set", "frozenset", "dict":
		return link.ClassBody, true
	case "object", "str", "int", "float", "bool", "bytes", "Exception", "BaseException", "ValueError":
		return link.ClassOpen, true
	}
	return 0, false
}

// pyLibraryBase is the kind of a base a library module declares, by its
// module and name (ref, NUL-joined as importedName spells it): Pydantic's
// models, TypedDict and NamedTuple bodies, FastAPI's and Starlette's
// request, file and special types; the typing, abc and enum classes that
// add no kind. Any other base is the linker's to look up.
func pyLibraryBase(ref string) (uint8, bool) {
	mod, name, _ := strings.Cut(ref, "\x00")
	root, _, _ := strings.Cut(mod, ".")
	switch {
	case root == "pydantic":
		return link.ClassBody, true
	case root == "typing" || root == "typing_extensions":
		switch name {
		case "TypedDict", "NamedTuple":
			return link.ClassBody, true
		case "Generic", "Protocol":
			return link.ClassOpen, true
		}
	case root == "abc" && (name == "ABC" || name == "ABCMeta"):
		return link.ClassOpen, true
	case root == "enum":
		return link.ClassOpen, true
	case root == "fastapi" || root == "starlette":
		switch {
		case name == "Request" || name == "WebSocket" || name == "HTTPConnection":
			return link.ClassRequest, true
		case name == "UploadFile":
			return link.ClassFile, true
		case strings.HasSuffix(name, "Response") || name == "BackgroundTasks" || name == "SecurityScopes":
			return link.ClassSkip, true
		}
	}
	return 0, false
}
