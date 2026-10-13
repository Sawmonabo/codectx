package link

import (
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Every endpoint carries a search document, so a kind filter on endpoints
// finds it whichever provider publishes it (docs/providers-treesitter.md,
// Endpoints). The linker's endpoints (a route a walk across files resolves,
// a manifest's declared entry point, an inherited Java `main`) carry the
// document the structural provider gives an endpoint: its framework, method,
// route and path, or its name when it has none, located where the endpoint
// is.

// endpointSearchDomain is the structural provider's search document domain,
// so the document of an endpoint whose node is one whichever provider
// decides it (an inherited Java `main`) is one document too.
const endpointSearchDomain = "treesitter-search-v1"

// maxEndpointDocBytes is the structural provider's bound on one document's
// body.
const maxEndpointDocBytes = 8 << 10

// endpointDocument adds the search document of endpoint n, minted in this
// file with name and qualified name as its node has them, at rng, with
// metadata meta, once per node.
func (f *filePublication) endpointDocument(n model.NodeID, name, qualified string, rng model.SourceRange,
	meta model.EndpointMetadata) {
	id := model.H(endpointSearchDomain, string(f.fv.ID), string(n))
	for _, d := range f.search {
		if d.ID == id {
			return
		}
	}
	body := strings.Join(strings.Fields(strings.Join([]string{meta.Framework, meta.Method, meta.Route, meta.Path}, " ")), " ")
	if body == "" {
		body = name
	}
	if len(body) > maxEndpointDocBytes {
		cut := maxEndpointDocBytes
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut]
	}
	f.search = append(f.search, model.SearchUnit{ID: id, NodeID: n, FileID: f.fv.ID, Path: f.fv.Path,
		Kind: model.NodeEndpoint, Name: name, QualifiedName: qualified,
		Bytes: model.ByteRange{Start: rng.Start.Byte, End: rng.End.Byte}, Body: body})
}

// putSearch hands a file's search documents to the sink, after its nodes.
func (p *publisher) putSearch(docs []model.SearchUnit) error {
	if len(docs) == 0 {
		return nil
	}
	if err := p.sink.PutSearchUnits(p.ctx, docs); err != nil {
		return err
	}
	p.records += uint64(len(docs))
	return nil
}
