package link

import (
	"cmp"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// The incremental relink's vocabulary (ADR-0014 decisions 2 and 3). Each
// file's table has two summaries, its interface and its own sites, each with
// its own digest. The interface is a set of entries, every one something
// another file's resolution reads; a refresh diffs it entry by entry. Every
// site's resolution records a trace: each key it read, with that key's hash
// at the time, so a refresh reruns exactly the sites whose reads changed.

// EntryKind is what one interface entry describes.
type EntryKind uint8

const (
	// EntryDeclaration is a declaration and its signature.
	EntryDeclaration EntryKind = 1
	// EntrySupertype is one declared subtype relation.
	EntrySupertype EntryKind = 2
	// EntryExport is one name the file exports or re-exports.
	EntryExport EntryKind = 3
	// EntryImport is one import of the file.
	EntryImport EntryKind = 4
	// EntryModule is the file's module identity: its package, module or
	// namespace.
	EntryModule EntryKind = 5
	// EntryInferred is one function's inferred return and parameter values,
	// where resolution follows values.
	EntryInferred EntryKind = 6
	// EntryFieldWrite is the values the file writes to one field, where
	// resolution is field-based.
	EntryFieldWrite EntryKind = 7
)

// Valid reports whether k is a known entry kind.
func (k EntryKind) Valid() bool { return k >= EntryDeclaration && k <= EntryFieldWrite }

// SummaryEntry is one entry of a file's interface summary. Key is the entry's
// identity within the file, stable across edits that do not change it (a
// declaration's descriptor path and disambiguator, an export's name, a
// field's name); Hash is the digest of its content. Two summaries differ
// exactly in the entries whose Key is in one only or whose Hash differs.
type SummaryEntry struct {
	Kind EntryKind
	Key  string
	Hash string
}

// TraceKeyKind is what one recorded read looked up.
type TraceKeyKind uint8

const (
	// TraceScopeLookup is a name looked up in a scope: the result is what
	// that scope declares under the name, so a declaration added or removed
	// under it changes the read.
	TraceScopeLookup TraceKeyKind = 1
	// TraceInterfaceEntry is one entry of another file's interface summary.
	TraceInterfaceEntry TraceKeyKind = 2
	// TraceInferredSummary is one function's inferred values.
	TraceInferredSummary TraceKeyKind = 3
	// TraceManifestField is one field of a manifest's linker-read
	// projection.
	TraceManifestField TraceKeyKind = 4
)

// Valid reports whether k is a known trace key kind.
func (k TraceKeyKind) Valid() bool { return k >= TraceScopeLookup && k <= TraceManifestField }

// TraceKey names one read a site's resolution made. Scope is where the read
// looked: the scope, file or manifest; Name is what it looked up there: the
// name, the interface entry's Key, the function's descriptor, the manifest
// field.
type TraceKey struct {
	Kind  TraceKeyKind
	Scope string
	Name  string
}

// TraceRead is one recorded read: the key and its hash when the site read it.
// A site reruns when any of its reads now hashes differently.
type TraceRead struct {
	Key  TraceKey
	Hash string
}

// The hash domains of the two summaries and of one interface entry.
const (
	interfaceDigestDomain = "link.interface"
	sitesDigestDomain     = "link.sites"
	entryHashDomain       = "link.interface_entry"
)

// Interface is t's interface summary: one entry for each thing another
// file's resolution reads of t, sorted by kind, then key, then hash. Its
// module identity (path, language, package, module node and the unions its
// types name), each import, declaration (its node identity, kind, names,
// parent, signature, flags, argument counts and returned type), supertype
// row, re-export, and each value it stores: a function taken as a value or
// written to a field, which the address-taken cross-product and field-based
// resolution read, and the values the flow-based inference starts from.
//
// No byte range is in it. Each entry's hash includes the entry's position in
// its list, because a binding names a declaration and a site by index into
// the table: an inserted declaration changes the hashes of those after it.
func (t *Table) Interface() ([]SummaryEntry, error) {
	var out []SummaryEntry
	var err error
	add := func(kind EntryKind, key string, pos int, v any) {
		if err != nil {
			return
		}
		var b []byte
		if b, err = json.Marshal(v); err == nil {
			out = append(out, SummaryEntry{Kind: kind, Key: key, Hash: model.H(entryHashDomain, strconv.Itoa(pos), string(b))})
		}
	}
	add(EntryModule, "", 0, struct {
		Path, Language, Package string
		Module                  Endpoint
		Unions                  [][]wire.Type
	}{t.Path, t.Language, t.Package, Endpoint{Node: t.Module.Node, Kind: t.Module.Kind, Name: t.Module.Name,
		Qualified: t.Module.Qualified}, t.Unions})
	for i, imp := range t.Imports {
		add(EntryImport, imp.Name+"\x00"+imp.Path, i, imp)
	}
	for i, d := range t.Decls {
		d.Range, d.CanonicalKey = model.SourceRange{}, ""
		add(EntryDeclaration, string(d.Node), i, d)
	}
	for i, st := range t.Supertypes {
		add(EntrySupertype, st.Type+"\x00"+st.Super, i, st)
	}
	for i, x := range t.Reexports {
		add(EntryExport, x.Name+"\x00"+x.From, i, x)
	}
	for i, v := range t.Values {
		v.Range = model.SourceRange{}
		if v.Field != "" {
			add(EntryFieldWrite, v.Field, i, v)
			continue
		}
		caller := ""
		if v.Caller >= 0 && int(v.Caller) < len(t.Decls) {
			caller = string(t.Decls[v.Caller].Node)
		}
		add(EntryInferred, caller, i, v)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b SummaryEntry) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Hash, b.Hash))
	})
	return out, nil
}

// InterfaceDigest is the digest of t's interface summary: two tables have one
// exactly when their Interface entries are equal.
func (t *Table) InterfaceDigest() (string, error) {
	entries, err := t.Interface()
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, 3*len(entries))
	for _, e := range entries {
		parts = append(parts, strconv.Itoa(int(e.Kind)), e.Key, e.Hash)
	}
	return model.H(interfaceDigestDomain, parts...), nil
}

// SitesDigest is the digest of t's sites summary: every call site in order,
// with its caller, callee, shape, qualifier, receiver, signature, arguments
// and in-file candidates, and the unions their types name, but no byte range
// or callee offset. A body edit that adds, removes or rewrites a call changes
// it; one that only moves code does not.
func (t *Table) SitesDigest() (string, error) {
	sites := make([]Site, len(t.Sites))
	for i, s := range t.Sites {
		s.Range, s.NameStart, s.NameEnd = model.SourceRange{}, 0, 0
		sites[i] = s
	}
	b, err := json.Marshal(struct {
		Sites  []Site
		Unions [][]wire.Type
	}{sites, t.Unions})
	if err != nil {
		return "", err
	}
	return model.H(sitesDigestDomain, string(b)), nil
}
