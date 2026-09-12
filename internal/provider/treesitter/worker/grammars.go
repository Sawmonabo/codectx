package worker

import (
	"regexp"
	"strings"
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
	// to instead of the declaration node itself.
	wrappers map[string]bool
	comments map[string]bool
	// refine fixes a declaration's kind, name and flags after capture.
	refine func(d *decl, src []byte)
	// docstring returns the language's in-body documentation, if any.
	docstring func(d *decl) (start, end uint, ok bool)
	isTest    func(d *decl, path string, src []byte) bool
	// importNames derives the local names an import path introduces when the
	// query captured none.
	importNames func(path string) []string
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
		wrappers: set("type_declaration", "var_declaration", "const_declaration"),
		comments: set("comment"),
		refine: func(d *decl, src []byte) {
			if d.body != nil {
				switch d.body.Kind() {
				case "struct_type":
					d.kind = "struct"
				case "interface_type":
					d.kind = "interface"
				}
			}
			r, _ := utf8.DecodeRuneInString(d.name)
			d.exported = unicode.IsUpper(r)
			// A method's receiver type qualifies it: (s *Server) Start is
			// Server.Start, whether or not Server is declared in this file.
			if recv := d.node.ChildByFieldName("receiver"); recv != nil {
				for n := recv.NamedChild(0); n != nil; n = n.NamedChild(0) {
					if n.Kind() == "type_identifier" {
						d.impl = n.Utf8Text(src)
						break
					}
					if n.Kind() == "parameter_declaration" {
						n = n.ChildByFieldName("type")
						if n == nil {
							break
						}
						if n.Kind() == "type_identifier" {
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
	"javascript": {language: tree_sitter_javascript.Language, wrappers: jsWrappers, comments: set("comment"), refine: jsRefine},
	"typescript": {language: tree_sitter_typescript.LanguageTypescript, wrappers: jsWrappers, comments: set("comment"), refine: jsRefine},
	"tsx":        {language: tree_sitter_typescript.LanguageTSX, wrappers: jsWrappers, comments: set("comment"), refine: jsRefine},
	"python": {
		language: tree_sitter_python.Language,
		wrappers: set("decorated_definition"),
		comments: set("comment"),
		docstring: func(d *decl) (uint, uint, bool) {
			if d.body == nil {
				return 0, 0, false
			}
			first := d.body.NamedChild(0)
			if first == nil || first.Kind() != "expression_statement" {
				return 0, 0, false
			}
			if s := first.NamedChild(0); s != nil && s.Kind() == "string" {
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
		comments: set("line_comment", "block_comment"),
		refine: func(d *decl, src []byte) {
			if m := childOfKind(d.node, "modifiers"); m != nil {
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
		wrappers: set("attribute_item"),
		comments: set("line_comment", "block_comment"),
		refine: func(d *decl, src []byte) {
			d.exported = childOfKind(d.node, "visibility_modifier") != nil
			d.macro = d.node.Kind() == "macro_definition"
		},
		isTest: func(d *decl, _ string, src []byte) bool {
			if d.kind != "function" {
				return false
			}
			for p := d.node.PrevNamedSibling(); p != nil && p.Kind() == "attribute_item"; p = p.PrevNamedSibling() {
				if rustTestAttr.MatchString(p.Utf8Text(src)) {
					return true
				}
			}
			return false
		},
		importNames: rustUseNames,
	},
	"c":   {language: tree_sitter_c.Language, wrappers: cWrappers, comments: set("comment"), refine: cRefine},
	"cpp": {language: tree_sitter_cpp.Language, wrappers: cWrappers, comments: set("comment"), refine: cRefine, importNames: func(p string) []string { return []string{lastSegment(p, "::")} }},
}

var jsWrappers = set("export_statement", "lexical_declaration", "variable_declaration")

// jsRefine: a declaration is exported when it or a wrapper ancestor was an
// export target. An export clause naming a top-level declaration is applied
// after containment is known, in emit.
func jsRefine(d *decl, _ []byte) {
	if d.kind == "test" {
		return
	}
	// `const f = (a) => {...}`: the signature runs to the function's own body,
	// not to the start of the function expression.
	if d.body != nil && (d.body.Kind() == "arrow_function" || d.body.Kind() == "function_expression") {
		if inner := d.body.ChildByFieldName("body"); inner != nil {
			d.body = inner
		}
	}
	for n := &d.node; n != nil; n = n.Parent() {
		if d.ex.exportRanges[span{n.StartByte(), n.EndByte()}] {
			d.exported = true
			return
		}
		if p := n.Parent(); p == nil || !jsWrappers[p.Kind()] {
			break
		}
	}
}

var cWrappers = set("template_declaration", "declaration_list")

// cRefine finds the identifier below a declarator chain and decides whether a
// declaration declares a function (a prototype), a variable or a typedef.
func cRefine(d *decl, src []byte) {
	if d.declarator == nil {
		if d.node.Kind() == "preproc_def" || d.node.Kind() == "preproc_function_def" {
			d.macro = true
		}
		return
	}
	n := d.declarator
	isFunc := false
	for n != nil {
		switch n.Kind() {
		case "function_declarator":
			isFunc = true
			n = n.ChildByFieldName("declarator")
		case "pointer_declarator", "array_declarator", "init_declarator", "attributed_declarator", "reference_declarator", "parenthesized_declarator":
			if c := n.ChildByFieldName("declarator"); c != nil {
				n = c
			} else {
				n = firstNamedChild(n)
			}
		case "qualified_identifier":
			if scope := n.ChildByFieldName("scope"); scope != nil {
				d.impl = scope.Utf8Text(src)
			}
			n = n.ChildByFieldName("name")
		case "destructor_name", "operator_name":
			d.name = n.Utf8Text(src)
			d.nameStart, d.nameEnd = n.StartByte(), n.EndByte()
			n = nil
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

func childOfKind(n ts.Node, kind string) *ts.Node {
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil && c.Kind() == kind {
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
// `as` alias, or every leaf of a `{a, b as c}` list.
func rustUseNames(p string) []string {
	if i := strings.IndexByte(p, '{'); i >= 0 {
		inner := strings.TrimSuffix(strings.TrimSpace(p[i+1:]), "}")
		var out []string
		for _, item := range strings.Split(inner, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, rustUseNames(item)...)
			}
		}
		return out
	}
	if _, alias, ok := strings.Cut(p, " as "); ok {
		return []string{strings.TrimSpace(alias)}
	}
	return []string{lastSegment(strings.TrimSpace(p), "::")}
}
