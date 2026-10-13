package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// openImportSite mints d's open import site, located at the import, named
// "import" and keyed in the file's scope by "openimport:", the path and the
// import's bytes, and returns its identity.
//
// Its metadata is resolution `unresolved`, reason `computed` and fragments:
// every non-empty literal part of the path, in source order, an empty list
// when there is none. No fragment is dropped or cut: a fragment longer than
// any path matches no file, which is the exact answer, and keeping only the
// fragments before it would match files the import cannot name. The
// metadata is rendered by model.SplitMetadata, so a fragment list too long for
// the node row is stored as metadata rows on the site, one per fragment,
// with a "fragments_rows" marker; readers rebuild it from the rows.
func (f *filePublication) openImportSite(d *DynamicImport) (model.NodeID, error) {
	fragments := make([]string, 0, len(d.Fragments))
	for _, frag := range d.Fragments {
		if frag != "" {
			fragments = append(fragments, frag)
		}
	}
	meta, rows, err := model.SplitMetadata(map[string]any{
		model.MetaResolution: model.ResolutionUnresolved,
		model.MetaReason:     model.ReasonComputed,
		model.MetaFragments:  fragments,
	})
	if err != nil {
		return "", internalErr("open import site metadata: " + err.Error())
	}
	path := f.fv.Path
	c := model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + path,
		NativeKey: fitKey("openimport:" + path + ":" + byteSpan(d.Range)), Kind: model.NodeModule,
		Language: f.t.Language, Name: "import"}
	before := len(f.nodes)
	n, err := f.mint(c, d.Range, meta)
	if err != nil {
		return "", err
	}
	if len(f.nodes) == before {
		// The site was already published by this file; its rows were too.
		return n, nil
	}
	fact := &f.nodes[len(f.nodes)-1]
	for _, row := range rows {
		detail, err := row.Detail()
		if err != nil {
			return "", internalErr("open import site metadata row: " + err.Error())
		}
		fact.Evidence = append(fact.Evidence, f.p.evidence(n, "", f.fv.ID, f.fv.ContentHash, nil, c.NativeKey, detail, "", 0))
	}
	return n, nil
}
