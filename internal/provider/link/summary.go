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
// its own digest (Interface, InterfaceDigest, SitesDigest). The interface is
// a set of stable-keyed entries, every one something another file's
// resolution reads. Every member's resolution records the keys it read
// (TraceKey, trace.go); the state keeps, per key, the hash of what the
// indexes hold under it, so a refresh re-runs exactly the members a changed
// key was read by.

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
	// EntryInferred is one value a declaration stores outside a field: a
	// function taken as a value, which the cross-product reads, or a value
	// the flow-based inference starts from.
	EntryInferred EntryKind = 6
	// EntryFieldWrite is the values the file writes to one field, where
	// resolution is field-based.
	EntryFieldWrite EntryKind = 7
	// EntryRouting is the file's open routing (Routing): its objects, their
	// routes and mounts, and the settings modules it names, which another
	// file's route walk reads.
	EntryRouting EntryKind = 8
)

// Valid reports whether k is a known entry kind.
func (k EntryKind) Valid() bool { return k >= EntryDeclaration && k <= EntryRouting }

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
	// TraceScopeLookup is a key looked up in a scope: an index key of the
	// unit's hierarchy, whose answer is what the index holds under it, or a
	// signature, whose answer is the functions taken as values under it.
	// A declaration added or removed under it changes the read.
	TraceScopeLookup TraceKeyKind = 1
	// TraceManifestField is one field of a manifest's linker-read
	// projection (manifestFields).
	TraceManifestField TraceKeyKind = 2
)

// Valid reports whether k is a known trace key kind.
func (k TraceKeyKind) Valid() bool { return k >= TraceScopeLookup && k <= TraceManifestField }

// TraceKey names one read a member's resolution made. Scope is where the
// read looked (scopeDecls, scopeLinks, scopeSignature, scopeManifest); Name
// is what it looked up there, spelled stably: an index key with its member
// ordinals and table indices spelled as the files and declarations they
// denote (idTable.stableKey), a signature, a manifest field's key.
type TraceKey struct {
	Kind  TraceKeyKind
	Scope string
	Name  string
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
// row, Go conversion (a proven subtype relation), re-export, its routing with
// the argument each mount passes, and each value
// it stores: a function taken as a value or written to a field, which the
// address-taken cross-product and field-based resolution read, and the values
// the flow-based inference starts from.
//
// No byte range is in it. Each entry's hash includes the entry's position in
// its list, because a binding names a declaration and a site by index into
// the table: an inserted declaration changes the hashes of those after it.
func (t *Table) Interface() ([]SummaryEntry, error) {
	// The argument each mount passes is read from t itself: the copy
	// clears the site indices that locate it.
	args := make([]*wire.Type, len(t.Routing.Mounts))
	for i := range t.Routing.Mounts {
		if m := &t.Routing.Mounts[i]; m.Join == JoinNone {
			// A Go mount's call (MountCall) is hashed with the routing;
			// its argument's type names a site by index.
			if a, ok := mountArgument(t, m); ok {
				args[i] = &a
			}
		}
	}
	t = t.positionless()
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
	}{t.Path, t.Language, t.Package, t.Module, t.Unions})
	for i, imp := range t.Imports {
		add(EntryImport, imp.Name+"\x00"+imp.Path, i, imp)
	}
	for i, d := range t.Decls {
		add(EntryDeclaration, string(d.Node), i, d)
	}
	for i, st := range t.Supertypes {
		add(EntrySupertype, st.Type+"\x00"+st.Super, i, st)
	}
	for i, c := range t.Conversions {
		add(EntrySupertype, "\x00"+c.Type+"\x00"+c.Interface, i, c)
	}
	for i, x := range t.Reexports {
		add(EntryExport, x.Name+"\x00"+x.From, i, x)
	}
	for i, v := range t.Values {
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
	if r := t.Routing; len(r.Objects)+len(r.Routes)+len(r.Mounts)+len(r.Settings)+len(t.Inputs)+len(t.Classes)+
		len(t.RouteHandlers) > 0 || t.ArgumentResolvers {
		// The argument each mount passes is read with the routing
		// (mountKey), though it is written in a site; the pending inputs,
		// the classes and the handlers named through imports are read with
		// it (route_input.go, route_handler.go).
		add(EntryRouting, "", 0, struct {
			Routing
			Arguments []*wire.Type
			Inputs    []PendingInput
			Classes   []ClassKind
			Resolvers bool
			Handlers  []RouteHandler
		}{r, args, t.Inputs, t.Classes, t.ArgumentResolvers, t.RouteHandlers})
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
// (their types, keywords and spreads) and in-file candidates, and the unions
// their types name, but no byte range, callee offset, argument read or
// result: the variables an argument reads and a result is written into are
// file-scoped nodes keyed by their ranges, and the return a result reaches is
// the site's own file's, all published only from the site's own file, which a
// content change republishes. A body edit that adds, removes or rewrites a call changes it;
// one that only moves code does not.
func (t *Table) SitesDigest() (string, error) {
	t = t.positionless()
	for i := range t.Sites {
		s := &t.Sites[i]
		s.Results, s.Into = nil, 0
		for j := range s.Arguments {
			s.Arguments[j].Reads = nil
		}
	}
	b, err := json.Marshal(struct {
		Sites  []Site
		Unions [][]wire.Type
	}{t.Sites, t.Unions})
	if err != nil {
		return "", err
	}
	return model.H(sitesDigestDomain, string(b)), nil
}

// positionless is a copy of t, sharing nothing t's summaries change, with
// every byte position in it cleared: each range, name range and canonical
// key (which spells a range) of its module, imports, declarations, their
// returns and parameters, sites, their callee offsets, arguments, the
// variables those read and the results they write, values, handlers,
// dynamic imports, routing mounts and pending inputs, and each Java anonymous body's name
// (javaAnonymous). Both summaries read t through it, so an edit that only
// moves code changes neither, and a field added to the table with a range
// in it is cleared here once for both.
func (t *Table) positionless() *Table {
	c := *t
	anon := anonymousNames(t)
	c.Module = c.Module.positionless()
	c.Imports = slices.Clone(t.Imports)
	for i := range c.Imports {
		c.Imports[i].Range = model.SourceRange{}
	}
	c.Decls = slices.Clone(t.Decls)
	for i := range c.Decls {
		d := &c.Decls[i]
		d.Endpoint = d.Endpoint.positionless()
		if d.Return != nil {
			r := d.Return.positionless()
			d.Return = &r
		}
		d.Parameters = slices.Clone(d.Parameters)
		for j := range d.Parameters {
			d.Parameters[j].Endpoint = d.Parameters[j].Endpoint.positionless()
		}
	}
	c.Supertypes = slices.Clone(t.Supertypes)
	c.Sites = slices.Clone(t.Sites)
	for i := range c.Sites {
		s := &c.Sites[i]
		s.Range, s.NameStart, s.NameEnd = model.SourceRange{}, 0, 0
		s.Args = slices.Clone(s.Args)
		s.Results = positionlessEndpoints(s.Results)
		s.Arguments = slices.Clone(s.Arguments)
		for j := range s.Arguments {
			s.Arguments[j].Range = model.SourceRange{}
			s.Arguments[j].Reads = positionlessEndpoints(s.Arguments[j].Reads)
		}
	}
	c.Values = slices.Clone(t.Values)
	for i := range c.Values {
		c.Values[i].Range = model.SourceRange{}
	}
	c.Unions = slices.Clone(t.Unions)
	for i := range c.Unions {
		c.Unions[i] = slices.Clone(c.Unions[i])
	}
	c.Handlers = positionlessEndpoints(t.Handlers)
	c.DynamicImports = slices.Clone(t.DynamicImports)
	for i := range c.DynamicImports {
		c.DynamicImports[i].Range = model.SourceRange{}
	}
	c.Routing = t.Routing.summary()
	c.Inputs = slices.Clone(t.Inputs)
	for i := range c.Inputs {
		c.Inputs[i].Range = model.SourceRange{}
	}
	if anon.decl != nil || anon.rank != nil {
		c.eachTypeName(func(s *string) { *s = anon.stable(*s) })
	}
	return &c
}

// positionless is e with its range, name range and canonical key cleared.
func (e Endpoint) positionless() Endpoint {
	e.Range, e.NameRange, e.CanonicalKey = model.SourceRange{}, nil, ""
	return e
}

func positionlessEndpoints(es []Endpoint) []Endpoint {
	if es == nil {
		return nil
	}
	out := make([]Endpoint, len(es))
	for i, e := range es {
		out[i] = e.positionless()
	}
	return out
}
