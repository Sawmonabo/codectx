package scip

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// The call-site join of Section 11.3.
//
// A call site is a syntax fact. SCIP cannot produce one: no indexer
// distinguishes a call-site reference from a reference to a function value,
// and none sets a write role (docs/research/08-scip-empirical-six-indexers.md,
// confirmed for all six). The structural provider finds every call site and
// stores it in its file's link table with the byte range of its callee token
// and its caller; the call linker binds the site from that table. This
// importer reads the same table for each changed document (callSites): a
// reference occurrence whose bytes are exactly a site's callee token is the
// compiler's resolution of that call, and joinCall publishes `calls` from the
// site's caller to the resolved symbol at compiler precision, keyed by the
// call-site key below, beside the occurrence's `references` edge. The
// linker's `calls` for the same site keeps its own evidence, so the call
// carries both rows: an equal target is one relation with two rows, a
// different one two relations from one caller at one key.
//
// The join is exact-range equality on one file version: the table and the
// index describe the same pinned bytes, and an occurrence over any other
// bytes than a callee token joins nothing.
//
// callsiteKey is where this importer spells the key. Section 11.3 defines the
// range as the **1-based inclusive** byte range of the callee identifier,
// while model.SourceRange is the zero-based half-open byte range of Section
// 9.3, so the conversion is start+1 and end. Worked example: the identifier
// `Bar` occupying zero-based bytes [34,37) of `pkg/a.go` is
//
//	callsite:pkg/a.go:35-37
//
// It must equal the linker's spelling byte for byte, so that both evidence
// rows of one call carry one key.
//
// ok is false only when the key does not fit: a path may be up to
// model.MaxPathBytes long while a native key is bounded at
// model.MaxNativeKeyBytes. The caller then counts the occurrence under
// callsite_keys_skipped.
func callsiteKey(p string, rng *model.SourceRange) (string, bool) {
	key := "callsite:" + p + ":" + strconv.FormatUint(rng.Start.Byte+1, 10) + "-" + strconv.FormatUint(rng.End.Byte, 10)
	if len(key) > model.MaxNativeKeyBytes {
		return "", false
	}
	return key, true
}

// fileScope is the file-local alias scope of Section 11.3.
func fileScope(p string) string { return "file:" + p }

// linkTable is the link table the structural provider stored for fv in this
// generation, nil when there is none: no lowering for its language, a failed
// link pass (which that unit reports), or an import built without tables.
func (im *importer) linkTable(ctx context.Context, fv model.FileVersion) (*link.Table, error) {
	if im.p.tables == nil {
		return nil, nil
	}
	t, ok, err := im.p.tables.Table(ctx, im.req.Binding.GenerationID, fv)
	if err != nil || !ok {
		return nil, err
	}
	return t, nil
}

// bindTable records the declarations of t by the bytes of each name token,
// for adopt. A token two declarations share keeps the first, in the table's
// order.
func (ds *docSource) bindTable(t *link.Table) {
	if t == nil {
		return
	}
	ds.language = t.Language
	for i := range t.Decls {
		e := t.Decls[i].Endpoint
		if e.NameRange == nil {
			continue
		}
		if ds.names == nil {
			ds.names = make(map[[2]uint64]link.Endpoint, len(t.Decls))
		}
		k := [2]uint64{e.NameRange.Start.Byte, e.NameRange.End.Byte}
		if _, taken := ds.names[k]; !taken {
			ds.names[k] = e
		}
	}
}

// adopt is the structural declaration whose name token is exactly rng, the
// bytes of a definition occurrence. A definition there is that declaration:
// the compiler and the structural provider name one function, so the index's
// definition takes the structural identity rather than minting its own, and
// the function's callers, callees and references are one node's
// (docs/providers-scip.md, "Call-site join").
func (ds *docSource) adopt(rng *model.SourceRange) (link.Endpoint, bool) {
	e, ok := ds.names[[2]uint64{rng.Start.Byte, rng.End.Byte}]
	return e, ok
}

// structuralNode is this unit's description of a structural node e of the
// file fv: the identity, kind and attributes the structural provider resolved
// (as the linker's occurrence of it has), located at the declaration, and the
// binding's metadata. It is the one description, so the definition that
// adopts e and the caller occurrence of e repeat one fact.
func (im *importer) structuralNode(fv model.FileVersion, language string, e link.Endpoint) (model.Node, error) {
	rng := e.Range
	n := model.Node{ID: e.Node, Kind: e.Kind, Language: language, Name: e.Name, QualifiedName: e.Qualified,
		FileID: fv.ID, ContentHash: fv.ContentHash, Range: &rng}
	if e.NameRange != nil {
		nr := *e.NameRange
		n.NameRange = &nr
	}
	if im.unverify {
		raw, err := json.Marshal(map[string]any{"source_binding": string(model.SourceBindingUnverified)})
		if err != nil {
			return model.Node{}, internal("scip metadata: " + err.Error())
		}
		n.Metadata = raw
	}
	return n, nil
}

// joinDigestDomain frames joinDigest.
const joinDigestDomain = "scip-join"

// joinDigest is the digest of what this importer reads of a document's link
// table: the module, every declaration's and route handler's identity and
// attributes with their ranges and name ranges (adopt, the callers of
// joinCall), and every call site's caller and callee token (callSites). It
// is part of the document hash, so a document whose index and bytes are
// unchanged while its structural identities moved is republished rather
// than kept with calls from a caller that no longer exists. A file with no
// table hashes apart from one with an empty table.
func joinDigest(t *link.Table) string {
	h := model.NewHasher(joinDigestDomain)
	if t == nil {
		h.AddString("none")
		return h.Sum()
	}
	h.AddString(t.Language)
	endpoint := func(e link.Endpoint) {
		h.AddString(string(e.Node))
		h.AddString(e.CanonicalKey)
		h.AddString(string(e.Kind))
		h.AddString(e.Name)
		h.AddString(e.Qualified)
		h.AddString(strconv.FormatUint(e.Range.Start.Byte, 10) + "-" + strconv.FormatUint(e.Range.End.Byte, 10))
		if e.NameRange != nil {
			h.AddString(strconv.FormatUint(e.NameRange.Start.Byte, 10) + "-" + strconv.FormatUint(e.NameRange.End.Byte, 10))
		} else {
			h.AddString("")
		}
	}
	endpoint(t.Module)
	h.AddString(strconv.Itoa(len(t.Decls)))
	for i := range t.Decls {
		endpoint(t.Decls[i].Endpoint)
	}
	h.AddString(strconv.Itoa(len(t.Handlers)))
	for _, e := range t.Handlers {
		endpoint(e)
	}
	h.AddString(strconv.Itoa(len(t.Sites)))
	for i := range t.Sites {
		s := &t.Sites[i]
		h.AddString(strconv.FormatInt(int64(s.Caller), 10) + " " + strconv.FormatInt(int64(s.Handler), 10) + " " +
			strconv.FormatUint(uint64(s.NameStart), 10) + " " + strconv.FormatUint(uint64(s.NameEnd), 10) + " " +
			strconv.FormatBool(s.Computed))
	}
	return h.Sum()
}
