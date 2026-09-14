package search

import "github.com/Sawmonabo/codectx/internal/storage/sqlite"

// columnWeight is the BM25F per-column weighting of digest §4: a name or
// qualified-name match outweighs a signature or path match, which outweighs a
// body match.
var columnWeight = map[sqlite.SearchColumn]float64{
	sqlite.ColumnName:          5,
	sqlite.ColumnQualifiedName: 5,
	sqlite.ColumnSignature:     2,
	sqlite.ColumnPath:          2,
	sqlite.ColumnBody:          1,
}

// bm25K1 and bm25B are the term-saturation and length-normalization constants
// of digest §4.
const bm25K1, bm25B = 1.2, 0.75
