package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// macroWant is what one declaration of a macro-prefixed header publishes:
// its kind, the macros proven among its specifiers, the candidate set for
// its return type when the type cannot be told from a macro, and the
// export's return type name ("" when the set is published instead).
type macroWant struct {
	kind    string
	macros  []string
	types   []string
	returns string
}

// macroDecls parses src and returns each declaration and the return type
// name of each export, keyed by name.
func macroDecls(t *testing.T, language, src string) (map[string]wire.Decl, map[string]string) {
	t.Helper()
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	_, fail, err := w.parse(&out, wire.Request{Language: language, Path: "x.h", SourceBytes: uint64(len(src))}, []byte(src))
	if err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	decls, returns := map[string]wire.Decl{}, map[string]string{}
	for {
		kind, payload, err := wire.ReadMessage(&out, 0)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case wire.KindDecl:
			var d wire.Decl
			if err := json.Unmarshal(payload, &d); err != nil {
				t.Fatal(err)
			}
			decls[d.Name] = d
		case wire.KindLink:
			l, err := wire.DecodeLink(payload)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range l.Exports {
				returns[src[e.Name.Start:e.Name.End]] = e.Returns.Name
			}
		}
	}
	return decls, returns
}

// TestCMacroPrefixedDeclarations checks that a declaration written with
// export or attribute macros the parser cannot expand keeps its own name,
// kind and return type, and publishes the macros it was written with. When
// a type name and a macro cannot be told apart, the identifiers are
// published as the return type's candidate set and no single type is
// claimed. Both grammars are checked: C++ reads `M T f(void)` as `T::f`
// with the `::` missing.
//
// Mutation: skip the re-parse without the proven macros -> in C++
// sqlite3_memory_used is a method returning SQLITE_API.
// Mutation: drop the MISSING check on the declarator path -> the same C++
// failure.
// Mutation: keep the candidates in the macro list -> g_foo publishes
// GLIB_AVAILABLE_IN_ALL as a macro.
// Mutation: pick one candidate as the return type -> g_foo returns
// GLIB_AVAILABLE_IN_ALL or gboolean.
// Mutation: scan only the gap before the declaration -> f loses the
// trailing G_GNUC_CONST.
// Mutation: treat every specifier identifier as a macro -> g, and the
// sqlite3_int64 type, are reported as macros.
// Mutation: skip a macro invocation's argument list -> xmlFoo loses
// LIBXML_ATTR_FORMAT.
// Mutation: ignore the file's own object-like `#define` -> the defined API
// is a candidate type of api_f instead of a macro.
// Mutation: ignore `#undef` -> the undefined UNDEF is a macro of undef_f
// and its return type is claimed as VAL.
func TestCMacroPrefixedDeclarations(t *testing.T) {
	cases := []struct {
		src  string
		want map[string]macroWant
	}{
		{
			src: "#define SQLITE_API\nSQLITE_API int sqlite3_open(const char *f);\nSQLITE_API sqlite3_int64 sqlite3_memory_used(void);\nstatic SQLITE_API int helper(void){ return 0; }\n",
			want: map[string]macroWant{
				"sqlite3_open":        {kind: "function", macros: []string{"SQLITE_API"}, returns: "int"},
				"sqlite3_memory_used": {kind: "function", macros: []string{"SQLITE_API"}, returns: "sqlite3_int64"},
				"helper":              {kind: "function", macros: []string{"SQLITE_API"}, returns: "int"},
			},
		},
		{
			src: "GLIB_AVAILABLE_IN_ALL gboolean g_foo (void);\n",
			want: map[string]macroWant{
				"g_foo": {kind: "function", types: []string{"GLIB_AVAILABLE_IN_ALL", "gboolean"}},
			},
		},
		{
			src: "void f(void) G_GNUC_CONST;\nLIBXML_ATTR_FORMAT(2,3) XMLPUBFUN int xmlFoo(int);\nint g(void);\n",
			want: map[string]macroWant{
				"f":      {kind: "function", macros: []string{"G_GNUC_CONST"}},
				"xmlFoo": {kind: "function", macros: []string{"LIBXML_ATTR_FORMAT", "XMLPUBFUN"}, returns: "int"},
				"g":      {kind: "function", returns: "int"},
			},
		},
		{
			src: "XMLPUBFUN xmlNodePtr XMLCALL xmlNewNode(int x);\nAPI struct s *mk(void);\nAPI unsigned long h(void);\n",
			want: map[string]macroWant{
				"xmlNewNode": {kind: "function", types: []string{"XMLPUBFUN", "xmlNodePtr", "XMLCALL"}},
				"mk":         {kind: "function", macros: []string{"API"}, returns: "s"},
				"h":          {kind: "function", macros: []string{"API"}, returns: "unsignedlong"},
			},
		},
		{
			src: "#define API\nAPI VAL api_f(void);\n#define UNDEF\n#undef UNDEF\nUNDEF VAL undef_f(void);\n",
			want: map[string]macroWant{
				"api_f":   {kind: "function", macros: []string{"API"}, returns: "VAL"},
				"undef_f": {kind: "function", types: []string{"UNDEF", "VAL"}},
			},
		},
	}
	for _, language := range []string{"c", "cpp"} {
		for _, c := range cases {
			decls, returns := macroDecls(t, language, c.src)
			for name, want := range c.want {
				d, ok := decls[name]
				if !ok {
					t.Errorf("%s %q: no declaration %s; got %v", language, c.src, name, decls)
					continue
				}
				if d.Kind != want.kind || !slices.Equal(d.SpecifierMacros, want.macros) || !slices.Equal(d.ReturnTypes, want.types) {
					t.Errorf("%s %s: kind %q macros %q types %q, want %q %q %q", language, name, d.Kind, d.SpecifierMacros, d.ReturnTypes, want.kind, want.macros, want.types)
				}
				if got := returns[name]; got != want.returns {
					t.Errorf("%s %s: export returns %q, want %q", language, name, got, want.returns)
				}
			}
		}
	}
}

// TestCVisibilityAsDeclared checks the visibility the worker reads from
// source: each object-like `#define` carries what its replacement states,
// one export per definition so each `#if` arm keeps its own value, a
// one-identifier replacement is followed (Alias) rather than classified, a
// replacement that is no specifier sequence carries nothing, and a
// declaration's own attributes, in every position the grammar places them,
// give its `visibility`. A declaration written with macros sends their
// names, with the type candidates, for the linker to read.
//
// Mutation: classify only the first definition of a name -> the
// unspecified and dllimport arms of API are lost.
// Mutation: drop the `__visibility__` spelling (trim no underscores) -> HID
// carries unspecified.
// Mutation: classify a one-identifier replacement -> LIB carries
// unresolved instead of following VIS.
// Mutation: treat a replacement naming a macro among keywords as
// unspecified -> EXT claims no visibility where VIS states default.
// Mutation: emit every macro -> N, whose replacement is a number, is sent.
// Mutation: read attributes only before the declarator -> q has none.
// Mutation: drop the candidates from the macros sent -> g_foo sends only
// what the file proves, and a header's GLIB_AVAILABLE_IN_ALL is never read.
func TestCVisibilityAsDeclared(t *testing.T) {
	const macros = "#ifdef BUILDING\n#define API __declspec(dllexport)\n#elif defined(STATIC)\n#define API\n#else\n" +
		"#define API __declspec(dllimport)\n#endif\n#define VIS __attribute__((visibility(\"default\")))\n" +
		"#define HID __attribute__ ((__visibility__ (\"hidden\")))\n#define LIB VIS\n#define EXT extern VIS\n" +
		"#define N 42\n#define G_H\nAPI int f(void);\nLIB int g(void);\n"
	const attributes = "__attribute__((visibility(\"hidden\"))) int h(void);\n" +
		"__declspec(dllexport) int d(void){ return 0; }\n[[gnu::visibility(\"protected\")]] int p(void);\n" +
		"int q(void) __attribute__((visibility(\"internal\")));\nint plain(void);\n"
	const undecided = "GLIB_AVAILABLE_IN_ALL gboolean g_foo (void);\n"
	type sent struct{ name, alias, visibility, macros string }
	for _, language := range []string{"c", "cpp"} {
		l, _ := linkOf(t, language, "x.h", macros)
		var got []sent
		for _, e := range l.Exports {
			got = append(got, sent{macros[e.Name.Start:e.Name.End], e.Alias, e.Visibility, e.Macros})
		}
		want := []sent{
			{"API", "", wire.VisibilityDllexport, ""},
			{"API", "", wire.VisibilityUnspecified, ""},
			{"API", "", wire.VisibilityDllimport, ""},
			{"VIS", "", wire.VisibilityDefault, ""},
			{"HID", "", wire.VisibilityHidden, ""},
			{"LIB", "VIS", "", ""},
			{"EXT", "", wire.VisibilityUnresolved, ""},
			{"G_H", "", wire.VisibilityUnspecified, ""},
			{"f", "", "", "API "},
			{"g", "", "", "LIB "},
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s exports:\n got %+v\nwant %+v", language, got, want)
		}
		decls, _ := macroDecls(t, language, attributes)
		for name, v := range map[string]string{"h": wire.VisibilityHidden, "d": wire.VisibilityDllexport,
			"p": wire.VisibilityProtected, "q": wire.VisibilityInternal, "plain": ""} {
			if decls[name].Visibility != v {
				t.Errorf("%s %s: visibility %q, want %q", language, name, decls[name].Visibility, v)
			}
		}
		l, _ = linkOf(t, language, "x.h", undecided)
		if len(l.Exports) != 1 || l.Exports[0].Macros != "GLIB_AVAILABLE_IN_ALL gboolean " {
			t.Errorf("%s g_foo exports %+v, want the macros \"GLIB_AVAILABLE_IN_ALL gboolean \"", language, l.Exports)
		}
	}
}
