package link

import (
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
)

// openImportMetadata is an open import site's metadata with fragments, the
// path's literal parts in source order: every non-empty one, from the first,
// while the node's metadata bound holds them and each is within a path's
// bound, and an empty list, never a missing one, when none is.
func openImportMetadata(fragments []string) ([]byte, error) {
	m := model.OpenImportSiteMetadata{Resolution: model.ResolutionUnresolved, Reason: model.ReasonComputed,
		Fragments: []string{}}
	meta, err := json.Marshal(m)
	if err != nil {
		return nil, internalErr("open import site metadata: " + err.Error())
	}
	for _, frag := range fragments {
		if frag == "" {
			continue
		}
		if len(frag) > model.MaxPathBytes {
			break
		}
		m.Fragments = append(m.Fragments, frag)
		wider, err := json.Marshal(m)
		if err != nil {
			return nil, internalErr("open import site metadata: " + err.Error())
		}
		if len(wider) > model.MaxMetadataBytes {
			break
		}
		meta = wider
	}
	return meta, nil
}
