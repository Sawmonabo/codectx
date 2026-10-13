package model

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
	"strconv"
)

// A node's metadata is one JSON object held on the node row, bounded by
// MaxMetadataBytes so that every answer that loads the node pays a bounded
// price for it. The bound is a row-shape bound, never a cap on the facts: a
// field whose value would push the object past it -- in practice a long list,
// such as a declaration's decorators, an open import site's fragments or a
// manifest's scripts -- is stored as child rows instead, one evidence row per
// element on the same node, and the object keeps a marker "<field>_rows"
// holding how many rows the field has. A reader that meets the marker pages
// the rows (MetadataRows) and rebuilds the value (JoinMetadataRows). Every
// metadata object that fits keeps every field inline and writes no row.

// metadataRowsSuffix is appended to a field's key to name its row-count
// marker.
const metadataRowsSuffix = "_rows"

// MetadataRowsKey is the metadata key of the marker that says field's value
// is stored as rows: its value is the number of rows.
func MetadataRowsKey(field string) string { return field + metadataRowsSuffix }

// MetadataRowShape says which part of the field's JSON value one row holds.
type MetadataRowShape string

const (
	// RowElement is one element of an array, at index Ordinal.
	RowElement MetadataRowShape = "element"
	// RowEntry is one member of an object, the Ordinal-th in the object's own
	// order, named by Key.
	RowEntry MetadataRowShape = "entry"
	// RowValue is the whole value of a field that is neither an array nor an
	// object; it is the field's only row.
	RowValue MetadataRowShape = "value"
)

// Valid reports whether s is a known row shape.
func (s MetadataRowShape) Valid() bool { return s == RowElement || s == RowEntry || s == RowValue }

// MetadataRow is one element of a node metadata field stored as its own row.
// It travels as the Detail of an evidence row on the node, so its identity is
// the evidence identity, which hashes the Detail: two equal elements of one
// list stay two rows through their ordinals.
type MetadataRow struct {
	Field   string           `json:"field"`
	Ordinal int64            `json:"ordinal"`
	Shape   MetadataRowShape `json:"shape"`
	Key     string           `json:"key,omitempty"`
	Value   json.RawMessage  `json:"value"`
}

// Validate enforces the row shape.
func (r MetadataRow) Validate() error {
	if err := requireField("metadata_row.field", r.Field, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireNonNegative("metadata_row.ordinal", r.Ordinal); err != nil {
		return err
	}
	if !r.Shape.Valid() {
		return invalid("metadata_row.shape %q is not a known row shape", truncateForMessage(string(r.Shape)))
	}
	if r.Shape != RowEntry && r.Key != "" {
		return invalid("metadata_row.key is set on a %s row; only an entry has a key", r.Shape)
	}
	if r.Shape == RowValue && r.Ordinal != 0 {
		return invalid("metadata_row.ordinal is %d on a value row; a value is its field's only row", r.Ordinal)
	}
	if len(r.Value) == 0 || !json.Valid(r.Value) {
		return invalid("metadata_row.value is not valid JSON")
	}
	return nil
}

// Detail renders r as the evidence detail that carries it. It is never cut:
// the element is source data bounded by its file, and a shortened element
// would be a different fact.
func (r MetadataRow) Detail() (string, error) {
	raw, err := encodeJSON(r)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ParseMetadataRow reads a metadata row from an evidence detail, and false
// when the detail is not one (an occurrence's own detail, or none).
func ParseMetadataRow(detail string) (MetadataRow, bool) {
	if detail == "" || detail[0] != '{' {
		return MetadataRow{}, false
	}
	var r MetadataRow
	dec := json.NewDecoder(bytes.NewReader([]byte(detail)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.Validate() != nil {
		return MetadataRow{}, false
	}
	return r, true
}

// SplitMetadata renders fields as a node's metadata object. When the whole
// object fits MaxMetadataBytes it is returned with no rows. Otherwise fields
// move to rows, the largest encoded value first (ties by key), each replaced
// by its "<field>_rows" marker, until the object fits; a field whose marker is
// no smaller than its value never moves, because moving it cannot help. It
// fails only when the markers themselves cannot fit, which takes hundreds of
// fields no producer writes.
//
// Values are encoded without HTML escaping, with object keys sorted, so a
// value such as ">=2" is stored as written and the same fields always give
// the same bytes.
func SplitMetadata(fields map[string]any) (json.RawMessage, []MetadataRow, error) {
	if len(fields) == 0 {
		return nil, nil, nil
	}
	values := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		raw, err := encodeJSON(v)
		if err != nil {
			return nil, nil, invalid("metadata field %q cannot be encoded: %s", truncateForMessage(k), err.Error())
		}
		values[k] = raw
	}
	object, err := encodeJSON(values)
	if err != nil {
		return nil, nil, invalid("metadata cannot be encoded: %s", err.Error())
	}
	if len(object) <= MaxMetadataBytes {
		return object, nil, nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b string) int {
		if d := cmp.Compare(len(values[b]), len(values[a])); d != 0 {
			return d
		}
		return cmp.Compare(a, b)
	})
	var rows []MetadataRow
	for _, k := range keys {
		raw := values[k]
		split, err := splitValue(k, raw)
		if err != nil {
			return nil, nil, err
		}
		marker := json.RawMessage(strconv.Itoa(len(split)))
		if len(MetadataRowsKey(k))+len(marker) >= len(k)+len(raw) {
			continue
		}
		delete(values, k)
		values[MetadataRowsKey(k)] = marker
		rows = append(rows, split...)
		if object, err = encodeJSON(values); err != nil {
			return nil, nil, invalid("metadata cannot be encoded: %s", err.Error())
		}
		if len(object) <= MaxMetadataBytes {
			return object, rows, nil
		}
	}
	return nil, nil, invalid("node metadata is %d bytes with every movable field stored as rows, limit %d", len(object), MaxMetadataBytes)
}

// splitValue cuts one field's encoded value into its rows: an array into its
// elements, an object into its members in the object's own order, anything
// else into one value row.
func splitValue(field string, raw json.RawMessage) ([]MetadataRow, error) {
	switch raw[0] {
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return nil, invalid("metadata field %q is not a JSON array: %s", truncateForMessage(field), err.Error())
		}
		rows := make([]MetadataRow, len(elems))
		for i, e := range elems {
			rows[i] = MetadataRow{Field: field, Ordinal: int64(i), Shape: RowElement, Value: e}
		}
		return rows, nil
	case '{':
		dec := json.NewDecoder(bytes.NewReader(raw))
		if _, err := dec.Token(); err != nil {
			return nil, invalid("metadata field %q is not a JSON object: %s", truncateForMessage(field), err.Error())
		}
		var rows []MetadataRow
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				return nil, invalid("metadata field %q is not a JSON object: %s", truncateForMessage(field), err.Error())
			}
			key, _ := tok.(string)
			var v json.RawMessage
			if err := dec.Decode(&v); err != nil {
				return nil, invalid("metadata field %q is not a JSON object: %s", truncateForMessage(field), err.Error())
			}
			rows = append(rows, MetadataRow{Field: field, Ordinal: int64(len(rows)), Shape: RowEntry, Key: key, Value: v})
		}
		return rows, nil
	}
	return []MetadataRow{{Field: field, Shape: RowValue, Value: raw}}, nil
}

// JoinMetadataRows rebuilds one field's value from its rows, which may come in
// any order. It fails when the rows are not exactly one field's ordinals
// 0..n-1 of one shape: a gap is a lost element, not a shorter list.
func JoinMetadataRows(rows []MetadataRow) (json.RawMessage, error) {
	if len(rows) == 0 {
		return nil, invalid("a metadata field stored as rows has no rows")
	}
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b MetadataRow) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	field, shape := sorted[0].Field, sorted[0].Shape
	for i, r := range sorted {
		if r.Field != field || r.Shape != shape {
			return nil, invalid("metadata rows mix fields or shapes")
		}
		if r.Ordinal != int64(i) {
			return nil, invalid("metadata field %q is missing row %d", truncateForMessage(field), i)
		}
	}
	var buf bytes.Buffer
	switch shape {
	case RowValue:
		if len(sorted) != 1 {
			return nil, invalid("metadata field %q has %d value rows; a value has one", truncateForMessage(field), len(sorted))
		}
		return sorted[0].Value, nil
	case RowElement:
		buf.WriteByte('[')
		for i, r := range sorted {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(r.Value)
		}
		buf.WriteByte(']')
	case RowEntry:
		buf.WriteByte('{')
		for i, r := range sorted {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, err := encodeJSON(r.Key)
			if err != nil {
				return nil, invalid("metadata row key cannot be encoded: %s", err.Error())
			}
			buf.Write(key)
			buf.WriteByte(':')
			buf.Write(r.Value)
		}
		buf.WriteByte('}')
	}
	return buf.Bytes(), nil
}

// EncodeMetadataValue renders one metadata value the way SplitMetadata
// does -- without HTML escaping, so ">=2" is stored as written -- for a
// producer that builds its rows itself.
func EncodeMetadataValue(v any) (json.RawMessage, error) { return encodeJSON(v) }

// encodeJSON renders v without HTML escaping and without a trailing newline.
func encodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return out, nil
}

// NodeMetadataRequest pages the metadata rows of one field of one node: the
// field a node's metadata names with a "<field>_rows" marker. Rows come in
// ordinal order, so the pages concatenated are the field's value in order.
type NodeMetadataRequest struct {
	GenerationID GenerationID `json:"generation_id,omitempty"`
	NodeID       NodeID       `json:"node_id"`
	// Field is the metadata field, the marker's key without its "_rows"
	// suffix (`decorators`, `fragments`, `unresolved_lines`, `scripts`, ...).
	Field string      `json:"field"`
	Page  PageRequest `json:"page"`
}

// Validate enforces the request shape.
func (r NodeMetadataRequest) Validate() error {
	if err := requireID("node_metadata.node_id", string(r.NodeID)); err != nil {
		return err
	}
	if err := requireField("node_metadata.field", r.Field, MaxIdentifierBytes); err != nil {
		return err
	}
	return r.Page.ValidatePinned("node_metadata", r.GenerationID)
}
