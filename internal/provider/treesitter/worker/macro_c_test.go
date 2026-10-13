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
