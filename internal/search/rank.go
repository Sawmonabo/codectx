package search

import "github.com/Sawmonabo/codectx/internal/model"

// ranked is one candidate in the bounded top-K heap: the Section 14.2 sort
// tuple, the rowid that hydrates it, and the folded occurrence count. Nothing
// else -- name/qualified name/signature would make 2000 entries ~30 MB against
// a 33 MB query_memory_bytes ceiling, and SearchDocuments exists to hydrate
// one page instead. Ordering compares only integers and exact strings; no
// float reaches a comparison (digest §4).
type ranked struct {
	Tier        model.SearchTier
	ScoreMicros int64
	Path        string
	StartByte   uint64
	NodeID      model.NodeID
	SearchKey   string
	RowID       int64
	Occurrences int64
}

// less is the total Section 14.2 tie-break order: tier rank, descending
// ScoreMicros, path, start byte, NodeID, search key.
func (a ranked) less(b ranked) bool {
	panic("search: ranked.less is not implemented")
}

// maxRankedHits bounds the heap (digest §6).
const maxRankedHits = 10 * model.MaxPageItems
