package worker

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
	tree_sitter_c "github.com/tree-sitter/tree-sitter-c/bindings/go"
	tree_sitter_cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"
	tree_sitter_java "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tree_sitter_javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tree_sitter_python "github.com/tree-sitter/tree-sitter-python/bindings/go"
	tree_sitter_rust "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tree_sitter_typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	tree_sitter_go "github.com/Sawmonabo/codectx/internal/provider/treesitter/gogrammar"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
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
	// exported decides whether a top-level declaration is exported, for a
	// language whose export rule is the file's, not the declaration's own
	// syntax; it runs once containment is known, so it sees only top-level
	// declarations.
	exported func(e *extraction, d *decl) bool
	// decorators lists a declaration's decorators, annotations or
	// attributes in source order, for a language that has them.
	decorators func(e *extraction, d *decl) []wire.Decorator
	// blank is the name that declares nothing in the language, or empty
	// when every name it extracts binds one. A declaration of it is not
	// extracted, and neither is any declaration nested inside it.
	blank string
	// flatFields are the names of the fields the lowerings resolve, the
	// grammar's field registry, resolved on first use by fieldRegistry.
	flatFields []string

	// registryOnce resolves registry.
	registryOnce sync.Once
	registry     fieldRegistry
	// fields are the field ids the extraction reads, resolved on first use
	// by fieldIDs.
	fieldsOnce sync.Once
	fields     fieldIDs
	// imports is an ECMAScript grammar's import-clause table, resolved on
	// first use by importSyntax.
	importsOnce sync.Once
	imports     importSyntax
	// deco is the grammar's decorator table, resolved on first use by
	// decoSyntax.
	decoOnce sync.Once
	deco     decoSyntax
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
	// Python docstrings and the `__main__` guard.
	expressionStatement, str, comparisonOperator, stringStart, stringContent, parenthesized uint16
	// Java and Rust modifiers, Rust macros and attributes.
	modifiers, visibilityModifier, macroDefinition, attributeItem uint16
	// ECMAScript function values and the bindings a function-valued
	// property is stored under.
	arrowFunction, functionExpression, pair, object, assignmentExpression uint16
	// The bindings of a `require(...)` declarator.
	variableDeclarator, objectPattern, shorthandPattern, objectAssignmentPattern uint16
	pairPattern, assignmentPattern, propertyIdentifier                           uint16
	// C and C++ macros and declarator chains, and what decides a C
	// definition's linkage.
	preprocDef, preprocFunctionDef, functionDeclarator, qualifiedIdentifier uint16
	functionDefinition, storageClassSpecifier, initDeclarator               uint16
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
			identifier:              id("identifier"),
			funcLiteral:             id("func_literal"),
			structType:              id("struct_type"),
			interfaceType:           id("interface_type"),
			typeIdentifier:          id("type_identifier"),
			parameterDeclaration:    id("parameter_declaration"),
			expressionStatement:     id("expression_statement"),
			str:                     id("string"),
			comparisonOperator:      id("comparison_operator"),
			stringStart:             id("string_start"),
			stringContent:           id("string_content"),
			parenthesized:           id("parenthesized_expression"),
			modifiers:               id("modifiers"),
			visibilityModifier:      id("visibility_modifier"),
			macroDefinition:         id("macro_definition"),
			attributeItem:           id("attribute_item"),
			arrowFunction:           id("arrow_function"),
			functionExpression:      id("function_expression"),
			pair:                    id("pair"),
			variableDeclarator:      id("variable_declarator"),
			objectPattern:           id("object_pattern"),
			shorthandPattern:        id("shorthand_property_identifier_pattern"),
			objectAssignmentPattern: id("object_assignment_pattern"),
			pairPattern:             id("pair_pattern"),
			assignmentPattern:       id("assignment_pattern"),
			propertyIdentifier:      id("property_identifier"),
			object:                  id("object"),
			assignmentExpression:    id("assignment_expression"),
			preprocDef:              id("preproc_def"),
			preprocFunctionDef:      id("preproc_function_def"),
			functionDeclarator:      id("function_declarator"),
			qualifiedIdentifier:     id("qualified_identifier"),
			functionDefinition:      id("function_definition"),
			storageClassSpecifier:   id("storage_class_specifier"),
			initDeclarator:          id("init_declarator"),
			errorKind:               id("ERROR"),
		}
	})
	return &g.kinds
}

// fieldIDs are the ids of the fields the extraction looks children up by,
// resolved once per grammar, since a lookup by name converts the name to a
// C string on every call. A field the grammar does not define is 0, which
// ChildByFieldId answers with no child, as a lookup by an unknown name does.
type fieldIDs struct {
	body, declarator, name, receiver, scope, typ, condition uint16
	// ECMAScript pairs and assignments.
	key, value, left, right uint16
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
			condition:  tl.FieldIdForName("condition"),
			key:        tl.FieldIdForName("key"),
			value:      tl.FieldIdForName("value"),
			left:       tl.FieldIdForName("left"),
			right:      tl.FieldIdForName("right"),
		}
	})
	return &g.fields
}

// fieldRegistry is a grammar's field registry: the fields the flat array
// records for every node (Flatten), which are exactly the fields a lowering
// may resolve, since mustField panics on any other. A lowering reads a field
// the registry lists from the array, and one it does not list nowhere.
type fieldRegistry struct {
	// ids are the registered field ids, ascending, each once.
	ids []uint16
	// known is indexed by field id and marks the registered ones.
	known []bool
}

// fieldRegistry is the field registry of g, resolved once. A name the
// grammar does not define is a registry defect and panics.
func (g *grammar) fieldRegistry() *fieldRegistry {
	g.registryOnce.Do(func() {
		tl := g.tsLanguage()
		g.registry.known = make([]bool, tl.FieldCount()+1)
		for _, name := range g.flatFields {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: a field registry names " + name + ", which its grammar does not define")
			}
			g.registry.known[v] = true
		}
		for id, ok := range g.registry.known {
			if ok {
				g.registry.ids = append(g.registry.ids, uint16(id))
			}
		}
	})
	return &g.registry
}

// noFields is the empty registry, for an array no field is read from.
var noFields fieldRegistry

// The field registries, one per lowering: the names each language's
// lowering resolves with mustField, the import-clause table's included.
var (
	goFields = []string{"alias", "alternative", "body", "communication", "condition", "consequence", "function",
		"initializer", "label", "left", "name", "operand", "operator", "parameters", "receiver", "result", "right",
		"type", "update", "value"}
	jsFields = []string{"alias", "alternative", "argument", "arguments", "body", "condition", "consequence",
		"declaration", "finalizer", "function", "handler", "increment", "index", "initializer", "key", "kind",
		"label", "left", "name", "object", "operator", "optional_chain", "parameter", "parameters", "pattern",
		"property", "right", "value"}
	pythonFields = []string{"alias", "alternative", "arguments", "body", "condition", "consequence", "definition",
		"function", "guard", "left", "module_name", "name", "object", "parameters", "return_type", "right",
		"superclasses", "type", "value"}
	javaFields = []string{"alternative", "arguments", "array", "body", "condition", "consequence", "declarator",
		"index", "init", "left", "name", "object", "operand", "operator", "parameters", "pattern", "resources",
		"right", "update", "value"}
	rustFields = []string{"alternative", "arguments", "body", "condition", "consequence", "function", "left",
		"name", "operator", "parameters", "pattern", "right", "type", "value"}
	cFields = []string{"alternative", "argument", "body", "condition", "consequence", "declarator", "filter",
		"goto_labels", "initializer", "label", "left", "name", "operator", "parameters", "right", "size", "type",
		"update", "value"}
	cppFields = slices.Concat(cFields, []string{"arguments", "captures", "function"})
)

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
		importNames: func(p string) []string { return []string{goImportName(p)} },
		// The Go specification, Blank identifier and Declarations and scope:
		// the blank identifier "does not introduce a binding and thus is not
		// declared", in a var, const, function, method, type or field
		// position alike.
		blank:      "_",
		flatFields: goFields,
	},
	"javascript": {language: tree_sitter_javascript.Language, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine, decorators: esDecorators, flatFields: jsFields},
	"typescript": {language: tree_sitter_typescript.LanguageTypescript, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine, decorators: esDecorators, flatFields: jsFields},
	"tsx":        {language: tree_sitter_typescript.LanguageTSX, wrappers: jsWrappers, comments: []string{"comment"}, refine: jsRefine, decorators: esDecorators, flatFields: jsFields},
	"python": {
		language:   tree_sitter_python.Language,
		wrappers:   []string{"decorated_definition"},
		comments:   []string{"comment"},
		refine:     pyMainGuard,
		decorators: pyDecorators,
		docstring: func(d *decl) (uint, uint, bool) {
			if d.body == nil || d.kind == "endpoint" {
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
		exported:    pyExported,
		flatFields:  pythonFields,
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
		decorators:  javaDecorators,
		flatFields:  javaFields,
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
				if rustTestAttribute(p, d.ex.g.tsLanguage(), src) {
					return true
				}
			}
			return false
		},
		importNames: rustUseNames,
		decorators:  rustDecorators,
		// The Rust Reference, Constant items: a free constant may be unnamed
		// by writing an underscore for its name, and such a constant is not
		// nameable. No other item the extraction reads may be named `_`.
		blank:      "_",
		flatFields: rustFields,
	},
	"c":   {language: tree_sitter_c.Language, wrappers: cWrappers, comments: []string{"comment"}, refine: cRefine, decorators: cDecorators, flatFields: cFields},
	"cpp": {language: tree_sitter_cpp.Language, wrappers: cWrappers, comments: []string{"comment"}, refine: cRefine, decorators: cDecorators, importNames: func(p string) []string { return []string{lastSegment(p, "::")} }, flatFields: cppFields},
}

// jsWrappers are the statements a declaration's doc comment and export
// marker attach to: an assignment's doc comment precedes its expression
// statement.
var jsWrappers = []string{"export_statement", "lexical_declaration", "variable_declaration", "expression_statement"}

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

// pyMainGuard keeps a module-level `if` the query captured as a `__main__`
// guard only when its condition is exactly `__name__ == "__main__"`, in either
// operand order and inside any number of parentheses: one comparison of two
// operands with the operator `==`, the identifier `__name__` and a string
// that is not a bytes literal whose one content is `__main__`. Its name is
// that content, wherever the parentheses put it. Anything else (`!=`, `is`,
// a chained comparison, `b"__main__"`) is no guard and declares nothing.
func pyMainGuard(d *decl, src []byte) {
	if d.kind != "endpoint" {
		return
	}
	k, f := d.ex.k, d.ex.f
	d.name = ""
	cond := d.node.ChildByFieldId(f.condition)
	for cond != nil && cond.KindId() == k.parenthesized {
		cond = soleOperand(cond)
	}
	if cond == nil || cond.KindId() != k.comparisonOperator || !pyEqualsGuard(cond, k, src) {
		return
	}
	var name *ts.Node
	ident := false
	for i := uint(0); i < cond.NamedChildCount(); i++ {
		n := cond.NamedChild(i)
		switch {
		case n == nil || n.IsExtra():
		case n.KindId() == k.identifier:
			ident = n.Utf8Text(src) == "__name__"
		case n.KindId() == k.str:
			if c := childOfKind(*n, k.stringContent); c != nil && c.Utf8Text(src) == "__main__" && n.NamedChildCount() == 3 {
				name = c
			}
		}
	}
	if ident && name != nil {
		d.name, d.nameStart, d.nameEnd = "__main__", name.StartByte(), name.EndByte()
	}
}

// soleOperand is the one expression a parenthesized expression holds,
// comments aside, or nil.
func soleOperand(n *ts.Node) *ts.Node {
	var out *ts.Node
	for i := uint(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(i)
		if c == nil || c.IsExtra() {
			continue
		}
		if out != nil {
			return nil
		}
		out = c
	}
	return out
}

// pyEqualsGuard reports a comparison of exactly two operands joined by one
// `==`, whose string operand is not a bytes literal.
func pyEqualsGuard(cond *ts.Node, k *kindIDs, src []byte) bool {
	cur := cond.Walk()
	defer cur.Close()
	operands, operators := 0, 0
	for ok := cur.GotoFirstChild(); ok; ok = cur.GotoNextSibling() {
		n := cur.Node()
		switch {
		case n.IsExtra():
		case n.IsNamed():
			operands++
			if n.KindId() == k.str {
				if start := childOfKind(*n, k.stringStart); start == nil || strings.ContainsAny(start.Utf8Text(src), "bB") {
					return false
				}
			}
		default:
			operators++
			if n.Utf8Text(src) != "==" {
				return false
			}
		}
	}
	return operands == 2 && operators == 1
}

var cWrappers = []string{"template_declaration", "declaration_list"}

// cRefine finds the identifier below a declarator chain and decides whether a
// declaration declares a function (a prototype), a variable or a typedef.
//
// A declaration is extracted only at file scope (cLinkSyntax.fileScope): the
// queries capture one under any preprocessor conditional or C++ template,
// and such a node inside a function body declares a local, which is no
// declaration of the file. A declarator declares a function only when a
// function declarator is the type constructor nearest its name: `int g(int)`
// and `int *g(int)` are functions, `int (*fp)(int)` and `void (*tbl[4])(int)`
// are objects of pointer type.
func cRefine(d *decl, src []byte) {
	f, k := d.ex.f, d.ex.k
	if d.declarator == nil {
		if id := d.node.KindId(); id == k.preprocDef || id == k.preprocFunctionDef {
			d.macro = true
		}
		return
	}
	x := cLinkSyntaxIn(d.ex.l.Name, func(string) *ts.Language { return d.ex.g.tsLanguage() })
	if d.node.KindId() == x.declaration {
		p := d.node.Parent()
		for p != nil && x.in(x.conditional, p.KindId()) {
			p = p.Parent()
		}
		if p == nil || !x.fileScope(p.KindId()) {
			return
		}
	}
	n := d.declarator
	isFunc := false
	for n != nil {
		switch id := n.KindId(); {
		case id == k.functionDeclarator:
			isFunc = true
			n = n.ChildByFieldId(f.declarator)
		case k.declaratorChain.has(n):
			if x.in(x.object, id) {
				isFunc = false
			}
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
	if d.ex.l.Name == "c" {
		d.exported = cExternal(d, src)
	}
}

// cExternal reports a C definition with external linkage (C17 6.2.2), which
// is what a C module exports: a function definition, or a file-scope object
// definition, not declared `static`. A prototype and an `extern` object
// declaration without an initializer define nothing, so they export nothing
// here; the file that defines the name does. Linkage is not shared-library
// visibility, which compiler flags and attribute macros decide.
func cExternal(d *decl, src []byte) bool {
	k := d.ex.k
	switch {
	case d.prototype || d.macro:
		return false
	case d.node.KindId() == k.functionDefinition:
		static, _ := storageClass(d.node, k, src)
		return !static
	case d.kind == "variable":
		static, extern := storageClass(d.node, k, src)
		return !static && (!extern || d.declarator.KindId() == k.initDeclarator)
	}
	return false
}

// storageClass reports whether a C declaration or definition is declared
// `static` or `extern`.
func storageClass(n ts.Node, k *kindIDs, src []byte) (static, extern bool) {
	if k.storageClassSpecifier == 0 {
		return false, false
	}
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		if c == nil || c.KindId() != k.storageClassSpecifier {
			continue
		}
		switch c.Utf8Text(src) {
		case "static":
			static = true
		case "extern":
			extern = true
		}
	}
	return static, extern
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

// goImportName is the name a Go import binds when its spec names none. The
// imported package's own clause decides it, which a reader of one file
// cannot see, so it is the path's last element by the convention modules
// follow: a major-version element is not the name (`github.com/go-chi/chi/v5`
// binds chi), and neither is a gopkg.in version suffix (`gopkg.in/yaml.v3`
// binds yaml).
func goImportName(p string) string {
	p = strings.Trim(p, "\"`")
	elems := strings.Split(p, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && isMajorVersion(name) {
		name = elems[len(elems)-2]
	}
	if strings.HasPrefix(p, "gopkg.in/") {
		if i := strings.LastIndex(name, ".v"); i > 0 && isMajorVersion(name[i+1:]) {
			name = name[:i]
		}
	}
	return name
}

// isMajorVersion reports a module major-version element: "v" and digits.
func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func firstSegment(s, sep string) string {
	s = strings.Trim(s, "\"'")
	if i := strings.Index(s, sep); i >= 0 {
		return s[:i]
	}
	return s
}

// rustUseNames is the local names a use path binds: every leaf of its use
// tree (lang.RustUse) but a glob, which binds no name of its own.
func rustUseNames(p string) []string {
	var out []string
	lang.RustUse(p, func(name, _ string) {
		if name != lang.RustGlob {
			out = append(out, name)
		}
	})
	return out
}
