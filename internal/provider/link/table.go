package link

import (
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Table is one file's link tables as the structural provider stores them with
// its unit, under TableKind: the worker's wire.Link joined to the
// declarations and call sites the parent validated, with every identity the
// parent resolved. The linker reads nothing else of the file.
//
// Indices are file-local: Decl.Parent, Site.Caller and Value.Caller index
// Decls (-1 is the file's module), Site.Candidates index Decls, a wire.Type
// of kind TypeResult indexes Sites and one of kind TypeUnion indexes Unions.
type Table struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	// Package is the file's package or module clause as written (Go's
	// package name, Java's package), empty when the language has none.
	Package string `json:"package,omitempty"`
	// Imports are the names the file's imports introduce, in source order:
	// one per local name, ImportAll for an import of every member of a
	// path, and one with an empty Name for an import that binds no name (an
	// include, a side-effect import).
	Imports []Import `json:"imports,omitempty"`
	// Module is the file's module node, the caller of a site outside every
	// declaration.
	Module Endpoint `json:"module"`
	// Decls are every declaration of the file, in the worker's declaration
	// order.
	Decls      []Decl           `json:"decls"`
	Supertypes []wire.Supertype `json:"supertypes,omitempty"`
	// Sites are every call site of the file, in the order of the worker's
	// call references.
	Sites  []Site  `json:"sites"`
	Values []Value `json:"values,omitempty"`
	// Reexports are the names the module exports under a name of its own
	// or from another module (wire.Reexport), ECMAScript's `export default`
	// included.
	Reexports []wire.Reexport `json:"reexports,omitempty"`
	// Unions are the type sets a wire.Type of kind TypeUnion names.
	Unions [][]wire.Type `json:"unions,omitempty"`
	// Handlers are the route handlers written as function literals that the
	// structural provider publishes as function nodes of their own: a call in
	// a handler's body is made from it (Site.Handler).
	Handlers []Endpoint `json:"handlers,omitempty"`
	// DynamicImports are the file's imports whose path is an expression
	// that does not fold to a constant (wire.DynamicImport); one that folds
	// is an ordinary entry of Imports.
	DynamicImports []DynamicImport `json:"dynamic_imports,omitempty"`
	// Dropped counts the worker's entries the parent could not join to a
	// declaration or a call reference of the file, or whose ranges did not
	// fit the pinned bytes, and each callable whose analysis failed in the
	// worker. They are dropped, never fatal to the unit, and make the file's
	// `calls` partial.
	Dropped int `json:"dropped,omitempty"`
}

// Import is one name an import introduces into the file. Path is what the
// name denotes as the language writes it: a Go package's import path (Name
// its local name, "_" for a blank import); the qualified name of a Java type
// or static member; the `::` path a C++ using-declaration names; the full
// `::` path of one leaf of a Rust use tree; the module specifier of an
// ECMAScript or Python import as written; an include's header. For an
// ImportAll import it is the package, type, namespace or module whose
// members it imports.
type Import struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
	// Original is the name Name denotes in Path's module as written, when
	// the import renames it (wire.Import.Originals): `a` for `import { a as
	// b }`, "default" for a default import, "*" for a namespace import and
	// for a Python `import a.b as c`, whose name is the module Path itself.
	Original string `json:"original,omitempty"`
	// Scope is the declaration the import is written in, -1 the module: a
	// Rust `use` inside an inline module or a block is that scope's alone,
	// and its relative path (`super::*`) starts there.
	Scope int32 `json:"scope"`
	// Range is the import statement, directive or call, the range of the
	// evidence of each `imports` it resolves to.
	Range model.SourceRange `json:"range"`
}

// DynamicImport is one import whose path is an expression that does not
// fold to a constant (`require("./x/" + name)`, `import(expr)`,
// `importlib.import_module(expr)`, `__import__(expr)`, `#include MACRO`),
// with the path's literal fragments in source order, none when it has none.
// Caller is the declaration it is written in, -1 the module.
type DynamicImport struct {
	Caller    int32             `json:"caller"`
	Range     model.SourceRange `json:"range"`
	Fragments []string          `json:"fragments"`
}

// ImportAll is the Name of an import of every member of its Path: a Go dot
// import, a Java on-demand import, a C++ using-directive, a Rust glob, a
// Python `from m import *`.
const ImportAll = "*"

// Endpoint is a node of the structural provider as the parent resolved it:
// what the linker publishes an occurrence of when a `calls` edge names it, so
// the edge's endpoint is the structural node itself and no second identity.
type Endpoint struct {
	Node         model.NodeID      `json:"node"`
	CanonicalKey string            `json:"canonical_key"`
	Kind         model.NodeKind    `json:"kind"`
	Name         string            `json:"name"`
	Qualified    string            `json:"qualified"`
	Range        model.SourceRange `json:"range"`
	// NameRange is the name token's range inside Range, when the
	// structural provider found one: the node's own NameRange.
	NameRange *model.SourceRange `json:"name_range,omitempty"`
}

// Decl is one declaration and the type facts the worker sent for it
// (wire.Export). Described is false for a declaration the worker sent no
// Export for; its type fields are then zero.
type Decl struct {
	Endpoint
	Parent    int32  `json:"parent"`
	Exported  bool   `json:"exported,omitempty"`
	Described bool   `json:"described,omitempty"`
	Receiver  string `json:"receiver,omitempty"`
	Signature string `json:"signature,omitempty"`
	Params    uint32 `json:"params,omitempty"`
	Optional  uint32 `json:"optional,omitempty"`
	Variadic  bool   `json:"variadic,omitempty"`
	Static    bool   `json:"static,omitempty"`
	Abstract  bool   `json:"abstract,omitempty"`
	Virtual   bool   `json:"virtual,omitempty"`
	Final     bool   `json:"final,omitempty"`
	Internal  bool   `json:"internal,omitempty"`
	// Prototype is a C or C++ function declaration without a body: it
	// announces a definition and yields to one wherever one is declared.
	Prototype bool `json:"prototype,omitempty"`
	// Macro is a macro definition: a Rust `macro_rules!`, which only a
	// macro invocation names and no call reaches, or a C or C++
	// function-like macro, which a call does.
	Macro   bool      `json:"macro,omitempty"`
	Returns wire.Type `json:"returns"`
	// Override reports a method the language marks as overriding or
	// implementing a supertype's (wire.Export.Override).
	Override bool `json:"override,omitempty"`
	// Alias is, for a C or C++ object-like macro whose replacement is one
	// identifier, that identifier (wire.Export.Alias): a call of the macro's
	// name calls what the identifier names where the macro is defined.
	Alias string `json:"alias,omitempty"`
	// Parameters are the callable's declared parameters, in declaration
	// order (wire.Link.Parameters).
	Parameters []Parameter `json:"parameters,omitempty"`
	// Property reports an ECMAScript function stored under a property path
	// (wire.Export.Property): no bare name denotes it.
	Property bool `json:"property,omitempty"`
}

// Parameter is one declared parameter of a callable. Endpoint is the
// parameter's variable node as the structural provider resolved it, keyed by
// its callable's identity and its position (ADR-0014), so an argument's flow
// into it survives an edit of the callee's body. Endpoint.Name is the
// parameter's name as written, which a keyword argument names; Endpoint.Node
// is empty when the structural provider minted no node for it (its
// callable's analysis failed, or its key did not fit), and such a parameter
// receives no flow.
type Parameter struct {
	Endpoint
	// Index is the parameter's position: 0 an explicit receiver or self
	// parameter, 1 the first declared parameter after it (wire.Parameter).
	Index       uint32 `json:"index"`
	Variadic    bool   `json:"variadic,omitempty"`
	KeywordOnly bool   `json:"keyword_only,omitempty"`
}

// Visible reports that declaration cand can be named by a bare name in code
// that declaration caller encloses (-1 the module), as ECMAScript and Python
// read a bare name: it is declared at module level, or in a function that
// encloses caller. A member of a class is never named bare; neither is a
// declaration nested in a function that does not enclose the caller.
func (t *Table) Visible(cand, caller int32) bool {
	if cand < 0 || int(cand) >= len(t.Decls) {
		return false
	}
	p := t.Decls[cand].Parent
	if p < 0 {
		return true
	}
	if int(p) >= len(t.Decls) || isType(t.Decls[p].Kind) {
		return false
	}
	for i := caller; i >= 0 && int(i) < len(t.Decls); i = t.Decls[i].Parent {
		if i == p {
			return true
		}
	}
	return false
}

// Site is one call site: the structural provider's call reference joined to
// the worker's wire.Site, with the declarations of this file the parent
// found its callee name could mean.
type Site struct {
	// Caller is the declaration enclosing the site, -1 for the module.
	Caller int32 `json:"caller"`
	// Range is the call expression, the range of the site's evidence.
	Range model.SourceRange `json:"range"`
	// NameStart and NameEnd bound the callee token, from which the
	// call-site key is spelled.
	NameStart uint32     `json:"name_start"`
	NameEnd   uint32     `json:"name_end"`
	Callee    string     `json:"callee"`
	Shape     wire.Shape `json:"shape"`
	Qualifier string     `json:"qualifier,omitempty"`
	// QualifierIsImport reports that the qualifier is a name an import of
	// this file introduced.
	QualifierIsImport bool        `json:"qualifier_is_import,omitempty"`
	Receiver          wire.Type   `json:"receiver"`
	Signature         string      `json:"signature,omitempty"`
	Args              []wire.Type `json:"args,omitempty"`
	// Described reports that the worker sent a wire.Site for the call, so
	// Args is its whole argument list (empty for a call of no argument);
	// a site no wire.Site joined carries no argument facts.
	Described bool `json:"described,omitempty"`
	// Arguments has one entry per argument, in order, when the worker sent
	// argument positions: Arguments[i] and Args[i] are the same argument.
	Arguments []Argument `json:"arguments,omitempty"`
	// Computed reports a call whose callee is an expression that names
	// nothing (wire.Link.Computed): no call reference names it, Callee is
	// the expression as written, and no analysis binds it.
	Computed bool `json:"computed,omitempty"`
	// Handler, when not 0, is one more than the index in Table.Handlers of
	// the route handler literal the call is made from: the site's `calls`
	// are published from that handler's node, as the structural provider's
	// are, while Caller stays the declaration whose scope the call's names
	// are read in.
	Handler int32 `json:"handler,omitempty"`
	// Candidates are the declarations of this file the callee name can
	// mean, the structural provider's own in-file targets.
	Candidates []int32 `json:"candidates,omitempty"`
}

// Argument is one argument of a call site (wire.Argument): where it is,
// the keyword it names, how it spreads, and the variable nodes of the named
// variables it reads, each the structural provider's variable node, the
// sources of its flow into the parameter it binds. A read whose variable has
// no node is left out.
type Argument struct {
	Range   model.SourceRange `json:"range"`
	Keyword string            `json:"keyword,omitempty"`
	Spread  wire.Spread       `json:"spread,omitempty"`
	Reads   []Endpoint        `json:"reads,omitempty"`
}

// Value is one value stored where a call can reach it without naming its
// declaration (wire.Value), in the declaration Caller encloses it in.
type Value struct {
	Caller int32             `json:"caller"`
	Range  model.SourceRange `json:"range"`
	Field  string            `json:"field,omitempty"`
	Type   wire.Type         `json:"type"`
	// Declares, when not 0, is one more than the index in Decls of the
	// function the value's own expression declares (wire.Value.Declares):
	// the value is that declaration, whatever its name.
	Declares int32 `json:"declares,omitempty"`
}

// Encode is t's stored form.
func (t *Table) Encode() ([]byte, error) { return json.Marshal(t) }

// DecodeTable reads a stored table.
func DecodeTable(b []byte) (*Table, error) {
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
