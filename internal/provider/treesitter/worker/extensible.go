package worker

import (
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	ts "github.com/tree-sitter/go-tree-sitter"
)

// Whether code outside the indexed project can override a method (the
// open-world disclosure of a call walk): a method is extensible when its
// language lets code in another package, module, crate or translation unit
// override or implement it, so the overrides an index of the project lists
// for it may not be all of them. Each language's rule reads the declaration
// and the types enclosing it; a type is open when code outside can subtype
// it, and a method only inside an open type.
//
//   - Java (JLS §8.1.1.2, §8.4.3, §8.9, §8.10, §9.1.1.4): a class open to
//     subclassing is public or protected (or a member of an interface,
//     implicitly public), neither final nor sealed, and not an enum, record
//     or annotation interface; an interface is open when public and not
//     sealed. A method of an open class is extensible when public or
//     protected and neither static, private nor final; a method of an open
//     interface when neither static nor private. A package-private type or
//     method is closed: another package cannot subclass or override it.
//   - Go (the specification's "Exported identifiers", "Interface types"): an
//     exported interface of an importable package (not `main`, not under an
//     `internal` directory) has its exported methods extensible, since any
//     type of another package with those methods implements it without
//     naming it. An unexported method is closed: another package cannot
//     declare it, and a type that embeds the interface to obtain it runs a
//     body the project declares. A concrete type's method is never
//     overridden.
//   - TypeScript and JavaScript: a method of an exported class or interface,
//     not private (`private`, `#name`) and not static.
//   - Python (PEP 591): a method of a class at module or class level, unless
//     the class or the method is decorated `final` (`typing.final`) or the
//     method's name is private by mangling (`__name`).
//   - Rust: a method of a trait declared `pub`; a method of an impl
//     implements, and is never overridden.
//   - C++ ([class.virtual], [class.pre], [basic.link]): a member of a class
//     a header declares outside an unnamed namespace and that is not final,
//     unless static or final itself; a class in a source file or an unnamed
//     namespace has no name another translation unit can derive from.
//   - C has no overriding.

// declOpen reports whether code outside the project can subtype the type
// declaration d, or override the method d, given its parent declaration
// (nil at the top level) and whether that parent is open.
func (e *extraction) declOpen(d *decl, w *wire.Decl, parent *wire.Decl, parentOpen bool) bool {
	isType := typeLike[d.kind]
	if !isType && d.kind != "method" {
		return false
	}
	// A type inside a function, or a method of anything but an open type, is
	// out of reach.
	if parent != nil && (!typeLike[parent.Kind] || !parentOpen) {
		return false
	}
	if !isType && parent == nil {
		return false
	}
	switch e.l.Name {
	case "java":
		return javaOpen(d, isType, e.src)
	case "go":
		return goOpen(d, isType, e.pkg, e.path)
	case "typescript", "tsx", "javascript":
		return ecmaOpen(d, w, isType, e.src)
	case "python":
		return pythonOpen(d, w, isType)
	case "rust":
		return rustOpen(d, isType, e.src)
	case "cpp":
		return cppOpen(d, isType, e.path, e.src)
	}
	return false
}

// keywords is the anonymous tokens among n's children, as written: a
// modifier list's keywords, never its annotations.
func keywords(n *ts.Node, src []byte) map[string]bool {
	out := map[string]bool{}
	if n == nil {
		return out
	}
	for i := uint(0); i < n.ChildCount(); i++ {
		if c := n.Child(i); c != nil && !c.IsNamed() {
			out[c.Utf8Text(src)] = true
		}
	}
	return out
}

// namedChild is n's first named child of the kind, or nil.
func namedChild(n *ts.Node, kind string) *ts.Node {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && c.Kind() == kind {
			return c
		}
	}
	return nil
}

func javaOpen(d *decl, isType bool, src []byte) bool {
	n := &d.node
	mods := keywords(namedChild(n, "modifiers"), src)
	inInterface := false
	if p := n.Parent(); p != nil && p.Kind() == "interface_body" {
		inInterface = true
	}
	if isType {
		switch n.Kind() {
		case "class_declaration":
			return (mods["public"] || mods["protected"] || inInterface) && !mods["final"] && !mods["sealed"]
		case "interface_declaration":
			return (mods["public"] || mods["protected"] || inInterface) && !mods["sealed"]
		}
		return false
	}
	if n.Kind() != "method_declaration" || mods["static"] || mods["private"] {
		return false
	}
	if inInterface {
		return true
	}
	return (mods["public"] || mods["protected"]) && !mods["final"]
}

// goOpen reads an interface's exported methods as open in an importable
// package (cmd/go "Internal Directories": a package under `internal` is
// importable only within the tree rooted at its parent).
func goOpen(d *decl, isType bool, pkg, file string) bool {
	if pkg == "main" || !exportedName(d.name) {
		return false
	}
	for _, seg := range strings.Split(path.Dir(file), "/") {
		if seg == "internal" {
			return false
		}
	}
	if isType {
		return d.kind == "interface"
	}
	return true
}

func exportedName(name string) bool {
	return name != "" && strings.ToUpper(name[:1]) == name[:1] && strings.ToLower(name[:1]) != name[:1]
}

func ecmaOpen(d *decl, w *wire.Decl, isType bool, src []byte) bool {
	if isType {
		return w.Exported && w.Parent < 0
	}
	if strings.HasPrefix(d.name, "#") {
		return false
	}
	n := &d.node
	if keywords(n, src)["static"] {
		return false
	}
	if a := namedChild(n, "accessibility_modifier"); a != nil && a.Utf8Text(src) == "private" {
		return false
	}
	return true
}

func pythonOpen(d *decl, w *wire.Decl, isType bool) bool {
	for _, dec := range w.Decorators {
		switch dec.Name {
		case "final", "typing.final", "typing_extensions.final":
			return false
		}
	}
	if !isType && strings.HasPrefix(d.name, "__") && !strings.HasSuffix(d.name, "__") {
		return false
	}
	return true
}

// rustOpen reads a trait as open when its visibility is `pub` alone: a
// `pub(crate)`, `pub(super)` or `pub(in path)` trait is the crate's own.
func rustOpen(d *decl, isType bool, src []byte) bool {
	if !isType {
		return true
	}
	if d.node.Kind() != "trait_item" {
		return false
	}
	v := namedChild(&d.node, "visibility_modifier")
	return v != nil && v.Utf8Text(src) == "pub"
}

// cppHeaders are the file extensions a C++ header is written with.
var cppHeaders = set(".h", ".hh", ".hpp", ".hxx", ".h++", ".inl", ".ipp", ".tpp", "")

func cppOpen(d *decl, isType bool, file string, src []byte) bool {
	n := &d.node
	if isType {
		if !cppHeaders[strings.ToLower(path.Ext(file))] || namedChild(n, "virtual_specifier") != nil {
			return false
		}
		for p := n.Parent(); p != nil; p = p.Parent() {
			if p.Kind() == "namespace_definition" && p.ChildByFieldName("name") == nil {
				return false
			}
		}
		return true
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c.Kind() == "storage_class_specifier" && c.Utf8Text(src) == "static" {
			return false
		}
	}
	return !cppFinal(n.ChildByFieldName("declarator"), src)
}

// cppFinal reports a function declarator, or one nested in a declarator,
// that carries the `final` virt-specifier.
func cppFinal(n *ts.Node, src []byte) bool {
	for ; n != nil; n = n.ChildByFieldName("declarator") {
		if n.Kind() == "function_declarator" {
			for i := uint(0); i < n.NamedChildCount(); i++ {
				if c := n.NamedChild(i); c.Kind() == "virtual_specifier" && c.Utf8Text(src) == "final" {
					return true
				}
			}
			return false
		}
	}
	return false
}
