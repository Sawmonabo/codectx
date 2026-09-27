package worker

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_go "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// grammar binds one pinned language to its compiled parser and the small set
// of syntax decisions the shared query vocabulary cannot express: where a
// declaration's name sits when it is buried in a declarator, which ancestor
// nodes carry a declaration's doc comment and export marker, and how each
// language spells exports, tests and docstrings.
type grammar struct {
	language func() unsafe.Pointer
	// wrappers are ancestor kinds a doc comment or export statement attaches
	// to instead of the declaration node itself; comments are the kinds a doc
	// comment is. Both are names, resolved to kind ids by kindIDs.
	wrappers []string
	comments []string
	// refine fixes a declaration's kind, name and flags after capture.
	refine func(d *decl, src []byte)
	// docstring returns the language's in-body documentation, if any.
	docstring func(d *decl) (start, end uint, ok bool)
	isTest    func(d *decl, path string, src []byte) bool
	// importNames derives the local names an import path introduces when the
	// query captured none.
	importNames func(path string) []string

	// fields are the field ids the extraction reads, resolved on first use
	// by fieldIDs.
	fieldsOnce sync.Once
	fields     fieldIDs
	// imports is an ECMAScript grammar's import-clause table, resolved on
	// first use by importSyntax.
	importsOnce sync.Once
	imports     importSyntax
	// kinds are the node kind ids the extraction and the hooks compare,
	// resolved on first use by kindIDs.
	kindsOnce sync.Once
	kinds     kindIDs
}

// kindIDs are the ids of the node kinds the extraction and the grammar hooks
// compare, resolved once per grammar, so no node's kind is converted to a
// string to be compared. It is one table for every grammar, since each hook
// reads only its own entries; a kind the grammar does not define is 0, which
// no named node has.
type kindIDs struct {
	wrappers, comments, declaratorChain kindSet
	identifier, funcLiteral             uint16
	// Go: the type a type_spec declares, and a method receiver's type.
	structType, interfaceType, typeIdentifier, parameterDeclaration uint16
	// Python docstrings.
	expressionStatement, str uint16
	// Java and Rust modifiers, Rust macros and attributes.
	modifiers, visibilityModifier, macroDefinition, attributeItem uint16
	// ECMAScript function values.
	arrowFunction, functionExpression uint16
	// C and C++ macros and declarator chains.
	preprocDef, preprocFunctionDef, functionDeclarator, qualifiedIdentifier uint16
	// errorKind is the kind of an ERROR node, the library's one error symbol,
	// which its name lookup answers for "ERROR" in every grammar.
	errorKind uint16
}

// kindSet is a set of named node kinds, indexed by kind id.
type kindSet []bool

// has reports whether n is a named node of one of the set's kinds. An error
// node's id lies outside the grammar's kind count.
func (s kindSet) has(n *ts.Node) bool {
	id := n.KindId()
	return n.IsNamed() && int(id) < len(s) && s[id]
}

// kindIDs is the kind table of g, resolved once.
func (g *grammar) kindIDs() *kindIDs {
	g.kindsOnce.Do(func() {
		tl := g.tsLanguage()
		id := func(name string) uint16 { return tl.IdForNodeKind(name, true) }
		kinds := func(names ...string) kindSet {
			s := make(kindSet, tl.NodeKindCount())
			for _, n := range names {
				if v := id(n); v != 0 {
					s[v] = true
				}
			}
			return s
		}
		g.kinds = kindIDs{
			wrappers: kinds(g.wrappers...),
			comments: kinds(g.comments...),
			declaratorChain: kinds("pointer_declarator", "array_declarator", "init_declarator", "attributed_declarator",
				"reference_declarator", "parenthesized_declarator"),
			identifier:           id("identifier"),
			funcLiteral:          id("func_literal"),
			structType:           id("struct_type"),
			interfaceType:        id("interface_type"),
			typeIdentifier:       id("type_identifier"),
			parameterDeclaration: id("parameter_declaration"),
			expressionStatement:  id("expression_statement"),
			str:                  id("string"),
			modifiers:            id("modifiers"),
			visibilityModifier:   id("visibility_modifier"),
			macroDefinition:      id("macro_definition"),
			attributeItem:        id("attribute_item"),
			arrowFunction:        id("arrow_function"),
			functionExpression:   id("function_expression"),
			preprocDef:           id("preproc_def"),
			preprocFunctionDef:   id("preproc_function_def"),
			functionDeclarator:   id("function_declarator"),
			qualifiedIdentifier:  id("qualified_identifier"),
			errorKind:            id("ERROR"),
		}
	})
	return &g.kinds
}

// fieldIDs are the ids of the fields the extraction looks children up by,
// resolved once per grammar, since a lookup by name converts the name to a
// C string on every call. A field the grammar does not define is 0, which
// ChildByFieldId answers with no child, as a lookup by an unknown name does.
type fieldIDs struct {
	body, declarator, name, receiver, scope, typ uint16
}

// fieldIDs is the field table of g, resolved once.
func (g *grammar) fieldIDs() *fieldIDs {
	g.fieldsOnce.Do(func() {
		tl := g.tsLanguage()
		g.fields = fieldIDs{
			body:       tl.FieldIdForName("body"),
			declarator: tl.FieldIdForName("declarator"),
			name:       tl.FieldIdForName("name"),
			receiver:   tl.FieldIdForName("receiver"),
			scope:      tl.FieldIdForName("scope"),
			typ:        tl.FieldIdForName("type"),
		}
	})
	return &g.fields
}

// importSyntax is the import-clause table of g, the ECMAScript grammar
// registered as language, resolved once.
func (g *grammar) importSyntax(language string) *importSyntax {
	g.importsOnce.Do(func() { g.imports = resolveImportSyntax(g.tsLanguage(), language) })
	return &g.imports
}

func set(kinds ...string) map[string]bool {
	m := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		m[k] = true
	}
	return m
}

var (
	goTestName   = regexp.MustCompile(`^(Test|Benchmark|Example|Fuzz)`)
	rustTestAttr = regexp.MustCompile(`\btest\b`)
	javaTestAnno = regexp.MustCompile(`@(org\.junit\.(jupiter\.api\.)?)?Test\b`)
)

var grammars = map[string]*grammar{
	"go": {
		language: tree_sitter_go.Language,
		wrappers: []string{"type_declaration", "var_declaration", "const_declaration"},
		comments: []string{"comment"},
		refine: func(d *decl, src []byte) {
			k := d.ex.k
			if d.body != nil {
				switch d.body.KindId() {
				case k.structType:
					d.kind = "struct"
				case k.interfaceType:
					d.kind = "interface"
				}
			}
			r, _ := utf8.DecodeRuneInString(d.name)
			d.exported = unicode.IsUpper(r)
			// A method's receiver type qualifies it: (s *Server) Start is
			// Server.Start, whether or not Server is declared in this file.
			f := d.ex.f
			if recv := d.node.ChildByFieldId(f.receiver); recv != nil {
				for n := recv.NamedChild(0); n != nil; n = n.NamedChild(0) {
					if n.KindId() == k.typeIdentifier {
						d.impl = n.Utf8Text(src)
						break
					}
					if n.KindId() == k.parameterDeclaration {
						n = n.ChildByFieldId(f.typ)
						if n == nil {
							break
						}
						if n.KindId() == k.typeIdentifier {
							d.impl = n.Utf8Text(src)
							break
						}
					}
				}
			}
		},
		isTest: func(d *decl, path string, _ []byte) bool {
			return d.kind == "function" && strings.HasSuffix(path, "_test.go") && goTestName.MatchString(d.name)
		},
		importNames: func(p string) []string { return []string{lastSegment(p, "/")} },
	},
	"javascript": {language: tree_sitter_javascript.Language, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine},
	"typescript": {language: tree_sitter_typescript.LanguageTypescript, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine},
	"tsx":        {language: tree_sitter_typescript.LanguageTSX, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine},
	"python": {
		language: tree_sitter_python.Language,
		wrappers: []string{"decorated_definition"},
		comments: []string{"comment"},
		docstring: func(d *decl) (uint, uint, bool) {
			if d.body == nil {
				return 0, 0, false
			}
			k := d.ex.k
			first := d.body.NamedChild(0)
			if first == nil || first.KindId() != k.expressionStatement {
				return 0, 0, false
			}
			if s := first.NamedChild(0); s != nil && s.KindId() == k.str {
				return s.StartByte(), s.EndByte(), true
			}
			return 0, 0, false
		},
		isTest: func(d *decl, _ string, _ []byte) bool {
			return (d.kind == "function" || d.kind == "method") && strings.HasPrefix(d.name, "test_") ||
				d.kind == "class" && strings.HasPrefix(d.name, "Test")
		},
		importNames: func(p string) []string { return []string{firstSegment(p, ".")} },
	},
	"java": {
		language: tree_sitter_java.Language,
		comments: []string{"line_comment", "block_comment"},
		refine: func(d *decl, src []byte) {
			if m := childOfKind(d.node, d.ex.k.modifiers); m != nil {
				text := m.Utf8Text(src)
				d.exported = strings.Contains(text, "public")
				d.test = d.kind == "method" && javaTestAnno.MatchString(text)
			}
		},
		isTest:      func(d *decl, _ string, _ []byte) bool { return d.test },
		importNames: func(p string) []string { return []string{lastSegment(p, ".")} },
	},
	"rust": {
		language: tree_sitter_rust.Language,
		wrappers: []string{"attribute_item"},
		comments: []string{"line_comment", "block_comment"},
		refine: func(d *decl, src []byte) {
			d.exported = childOfKind(d.node, d.ex.k.visibilityModifier) != nil
			d.macro = d.node.KindId() == d.ex.k.macroDefinition
		},
		isTest: func(d *decl, _ string, src []byte) bool {
			if d.kind != "function" {
				return false
			}
			for p := d.node.PrevNamedSibling(); p != nil && p.KindId() == d.ex.k.attributeItem; p = p.PrevNamedSibling() {
				if rustTestAttr.MatchString(p.Utf8Text(src)) {
					return true
				}
			}
			return false
		},
		importNames: rustUseNames,
	},
	"c":   {language: tree_sitter_c.Language, wrappers: cWrappers, comments: []string{"comment"}, refine: cRefine},
	"cpp": {language: tree_sitter_cpp.Language, wrappers: cWrappers, comments: []string{"comment"}, refine: cRefine, importNames: func(p string) []string { return []string{lastSegment(p, "::")} }},
}

var jsWrappers = []string{"export_statement", "lexical_declaration", "variable_declaration"}

// jsRefine: a declaration is exported when it or a wrapper ancestor was an
// export target. An export clause naming a top-level declaration is applied
// after containment is known, in emit.
func jsRefine(d *decl, _ []byte) {
	if d.kind == "test" {
		return
	}
	// `const f = (a) => {...}`: the signature runs to the function's own body,
	// not to the start of the function expression.
	k := d.ex.k
	if d.body != nil && (d.body.KindId() == k.arrowFunction || d.body.KindId() == k.functionExpression) {
		if inner := d.body.ChildByFieldId(d.ex.f.body); inner != nil {
			d.body = inner
		}
	}
	for n := &d.node; n != nil; n = n.Parent() {
		if d.ex.exportRanges[span{n.StartByte(), n.EndByte()}] {
			d.exported = true
			return
		}
		if p := n.Parent(); p == nil || !k.wrappers.has(p) {
			break
		}
	}
}

var cWrappers = []string{"template_declaration", "declaration_list"}

// cRefine finds the identifier below a declarator chain and decides whether a
// declaration declares a function (a prototype), a variable or a typedef.
func cRefine(d *decl, src []byte) {
	f, k := d.ex.f, d.ex.k
	if d.declarator == nil {
		if id := d.node.KindId(); id == k.preprocDef || id == k.preprocFunctionDef {
			d.macro = true
		}
		return
	}
	n := d.declarator
	isFunc := false
	for n != nil {
		switch id := n.KindId(); {
		case id == k.functionDeclarator:
			isFunc = true
			n = n.ChildByFieldId(f.declarator)
		case k.declaratorChain.has(n):
			if c := n.ChildByFieldId(f.declarator); c != nil {
				n = c
			} else {
				n = firstNamedChild(n)
			}
		case id == k.qualifiedIdentifier:
			if scope := n.ChildByFieldId(f.scope); scope != nil {
				d.impl = scope.Utf8Text(src)
			}
			n = n.ChildByFieldId(f.name)
		default:
			d.name = n.Utf8Text(src)
			d.nameStart, d.nameEnd = n.StartByte(), n.EndByte()
			n = nil
		}
	}
	if isFunc && (d.kind == "variable" || d.kind == "field") {
		d.kind = "function"
		d.prototype = true
	}
	if d.impl != "" && d.kind == "function" {
		d.kind = "method"
	}
}

// childOfKind is n's first named child of the kind id; id 0 never matches.
func childOfKind(n ts.Node, id uint16) *ts.Node {
	if id == 0 {
		return nil
	}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && c.KindId() == id {
			return c
		}
	}
	return nil
}

func firstNamedChild(n *ts.Node) *ts.Node { return n.NamedChild(0) }

func lastSegment(s, sep string) string {
	s = strings.Trim(s, "\"'<>")
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[i+len(sep):]
	}
	return s
}

func firstSegment(s, sep string) string {
	s = strings.Trim(s, "\"'")
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// rustUseNames derives the local names of a use path: the last segment, an
// `as` alias, or every leaf of a `{a, b as c}` list, nested lists included. A
// list is split only at its own commas, so `{b::{c, d}, e}` binds c, d and e
// rather than a leaf that carries a brace. A list carries its prefix to its
// leaves, since `self` in a list names the prefix itself: `std::io::{self,
// Write}` binds io and Write, never "self", which would make every
// `self.m()` of the file read as a call through an import. A glob and an
// underscore alias bind nothing.
func rustUseNames(p string) []string { return rustUseLeaves("", p) }

// rustUseLeaves is rustUseNames for the use tree p inside a list whose path
// so far is prefix.
func rustUseLeaves(prefix, p string) []string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '{'); i >= 0 {
		if head := strings.TrimSuffix(strings.TrimSpace(p[:i]), "::"); head != "" {
			if prefix != "" {
				head = prefix + "::" + head
			}
			prefix = head
		}
		inner := strings.TrimSuffix(strings.TrimSpace(p[i+1:]), "}")
		var out []string
		depth, from := 0, 0
		for j := 0; j <= len(inner); j++ {
			if j < len(inner) {
				switch inner[j] {
				case '{':
					depth++
					continue
				case '}':
					depth--
					continue
				case ',':
					if depth > 0 {
						continue
					}
				default:
					continue
				}
			}
			if item := strings.TrimSpace(inner[from:j]); item != "" {
				out = append(out, rustUseLeaves(prefix, item)...)
			}
			from = j + 1
		}
		return out
	}
	if f := strings.Fields(p); len(f) >= 3 && f[len(f)-2] == "as" {
		if alias := f[len(f)-1]; alias != "_" {
			return []string{alias}
		}
		return nil
	}
	switch leaf := lastSegment(p, "::"); leaf {
	case "*":
		return nil
	case "self":
		// `self` alone in a list binds the list's own path by its last
		// segment; with no path before it there is nothing to bind.
		if prefix == "" {
			return nil
		}
		return []string{lastSegment(prefix, "::")}
	default:
		return []string{leaf}
	}
}
