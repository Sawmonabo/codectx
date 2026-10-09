package link

import (
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// tableDigestDomain is the hash domain of a table's digest.
const tableDigestDomain = "link.table"

// Table is one file's link tables as the structural provider stores them with
// its unit, under TableKind: the worker's wire.Link joined to the
// declarations and call sites the parent validated, with every identity the
// parent resolved. The linker reads nothing else of the file.
//
// Indices are file-local: Decl.Parent, Site.Caller and Value.Caller index
// Decls (-1 is the file's module), Site.Candidates index Decls, and a
// wire.Type of kind TypeResult indexes Sites.
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
	// Dropped counts the worker's entries the parent could not join to a
	// declaration or a call reference of the file, or whose ranges did not
	// fit the pinned bytes. They are dropped, never fatal to the unit, and
	// make the file's `calls` partial.
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
	Variadic  bool   `json:"variadic,omitempty"`
	Static    bool   `json:"static,omitempty"`
	Abstract  bool   `json:"abstract,omitempty"`
	// Prototype is a C or C++ function declaration without a body: it
	// announces a definition and yields to one wherever one is declared.
	Prototype bool `json:"prototype,omitempty"`
	// Default is an ECMAScript declaration its module exports as
	// `default` (`export default class C {}`).
	Default bool      `json:"default,omitempty"`
	Returns wire.Type `json:"returns"`
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
	// Candidates are the declarations of this file the callee name can
	// mean, the structural provider's own in-file targets.
	Candidates []int32 `json:"candidates,omitempty"`
}

// Value is one value stored where a call can reach it without naming its
// declaration (wire.Value), in the declaration Caller encloses it in.
type Value struct {
	Caller int32             `json:"caller"`
	Range  model.SourceRange `json:"range"`
	Field  string            `json:"field,omitempty"`
	Type   wire.Type         `json:"type"`
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

// Digest is the digest of what the linker's resolution reads of t: every
// name, kind, shape, type fact, candidate and index, the package and the
// imports, and the order of every list, but no byte range, position, node
// identity or dropped count. A structural node's identity is derived from
// its range, and an edit that shifts a range moves neither a name nor a
// type, so two tables with one digest resolve every site
// to the same declarations by index; only the ranges and identities the
// edges are published with differ, and those are read from the fresh table.
// That is what lets an edit inside a function body that changes neither
// table relink nothing.
func (t *Table) Digest() (string, error) {
	c := Table{Path: t.Path, Language: t.Language, Package: t.Package, Imports: t.Imports, Supertypes: t.Supertypes,
		Values: make([]Value, len(t.Values))}
	c.Module = Endpoint{Kind: t.Module.Kind, Name: t.Module.Name, Qualified: t.Module.Qualified}
	c.Decls = make([]Decl, len(t.Decls))
	for i, d := range t.Decls {
		d.Endpoint = Endpoint{Kind: d.Kind, Name: d.Name, Qualified: d.Qualified}
		c.Decls[i] = d
	}
	c.Sites = make([]Site, len(t.Sites))
	for i, s := range t.Sites {
		s.Range, s.NameStart, s.NameEnd = model.SourceRange{}, 0, 0
		c.Sites[i] = s
	}
	for i, v := range t.Values {
		v.Range = model.SourceRange{}
		c.Values[i] = v
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return "", err
	}
	return model.H(tableDigestDomain, string(b)), nil
}
