package sqlite

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SearchColumn names one indexed column of search_fts, in its declared order.
type SearchColumn string

const (
	ColumnName          SearchColumn = "name"
	ColumnQualifiedName SearchColumn = "qualified_name"
	ColumnSignature     SearchColumn = "signature"
	ColumnPath          SearchColumn = "path"
	ColumnBody          SearchColumn = "body"
)

// MaxTermOffsets bounds the offsets one TermOccurrence carries.
const MaxTermOffsets = 512

// TermOccurrence is one term's instances in one column of one visible
// document. Offsets ascend, truncated at MaxTermOffsets; a phrase count from
// truncated offsets is a lower bound, which Truncated says.
type TermOccurrence struct {
	RowID     int64
	Column    SearchColumn
	Count     int64
	Offsets   []int64
	Truncated bool
}

// SearchDocument is a visible lexical document WITHOUT its body: Section 14.2
// forbids source bodies in generic results. Ranking and hydration use it.
type SearchDocument struct {
	RowID         int64
	ID            string
	NodeID        model.NodeID
	FileID        model.FileID
	Path          string
	Kind          model.NodeKind
	Name          string
	QualifiedName string
	Signature     string
	Bytes         model.ByteRange
	TokenCount    int64
}

// FileByPath resolves a normalized path to its file identity; unknown is
// CTX_ARGUMENT_INVALID, not an empty result.
func (r *PinnedReader) FileByPath(ctx context.Context, path string) (model.FileID, error) {
	return "", &model.Error{Code: model.CodeInternal, Message: "sqlite.PinnedReader.FileByPath is not implemented"}
}

// NodesInFile pages visible node facts declared in file, keyset on
// (start_byte, node_id), through idx_nodes_file.
func (r *PinnedReader) NodesInFile(ctx context.Context, file model.FileID, afterStart int64, after model.NodeID, limit int) ([]StoredNode, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "sqlite.PinnedReader.NodesInFile is not implemented"}
}

// DocumentFrequency returns, per term in order, how many visible documents
// contain it; the caller caps len(terms) at resources.max_query_terms.
func (r *PinnedReader) DocumentFrequency(ctx context.Context, terms []string) ([]int64, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "sqlite.PinnedReader.DocumentFrequency is not implemented"}
}

// TermOccurrences pages visible documents containing term, keyset on rowid,
// one row per (document, column), ordered by (rowid, column).
func (r *PinnedReader) TermOccurrences(ctx context.Context, term string, after int64, limit int) ([]TermOccurrence, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "sqlite.PinnedReader.TermOccurrences is not implemented"}
}

// SearchDocuments hydrates visible documents by rowid in one bounded query
// (len(rowids) <= model.MaxPageItems). Missing rowids are omitted.
func (r *PinnedReader) SearchDocuments(ctx context.Context, rowids []int64) ([]SearchDocument, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "sqlite.PinnedReader.SearchDocuments is not implemented"}
}
