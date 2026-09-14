package neo4jcsv

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// boundedRecordReader hands encoding/csv its bytes while enforcing the record
// cap itself. It tracks quote state so a quoted field containing newlines is
// still one record, and fails as soon as more than limit bytes have been read
// since the last record boundary — before the csv reader has grown its buffer
// to hold them. csv.Reader returns the underlying error unwrapped, so the
// typed limit surfaces to the caller.
type boundedRecordReader struct {
	r        io.Reader
	limit    int64
	since    int64
	inQuote  bool
	consumed int64
}

func (b *boundedRecordReader) Read(p []byte) (int, error) {
	if int64(len(p)) > b.limit {
		p = p[:b.limit]
	}
	n, err := b.r.Read(p)
	for _, c := range p[:n] {
		switch c {
		case '"':
			b.inQuote = !b.inQuote
		case '\n':
			if !b.inQuote {
				b.since = -1
			}
		}
		b.since++
		if b.since > b.limit {
			return 0, resourceLimit("an export record exceeds %d bytes", b.limit).WithDetail("limit", "max_provider_record_bytes")
		}
	}
	b.consumed += int64(n)
	return n, err
}

// csvColumns is one decoded Neo4j header: the role columns and the property
// columns by name, with the type suffix stripped.
type csvColumns struct {
	id, label, start, end, typ int
	props                      map[string]int
}

func parseHeader(fields []string) (csvColumns, error) {
	if len(fields) == 0 || len(fields) > maxFields {
		return csvColumns{}, outputInvalid("an export CSV header has %d columns; the import admits 1..%d", len(fields), maxFields)
	}
	c := csvColumns{id: -1, label: -1, start: -1, end: -1, typ: -1, props: make(map[string]int, len(fields))}
	for i, f := range fields {
		name, typ, _ := strings.Cut(f, ":")
		if name == "" {
			switch typ {
			case "ID":
				c.id = i
			case "LABEL":
				c.label = i
			case "START_ID":
				c.start = i
			case "END_ID":
				c.end = i
			case "TYPE":
				c.typ = i
			default:
				return csvColumns{}, outputInvalid("an export CSV header has an unknown role column %q", typ)
			}
			continue
		}
		c.props[name] = i
	}
	return c, nil
}

func (c csvColumns) get(rec []string, name string) string {
	if i, ok := c.props[name]; ok && i < len(rec) {
		return rec[i]
	}
	return ""
}

// nullInt decodes an optional non-negative integer property.
func nullInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// exportFile is one recognized data file of an export directory.
type exportFile struct {
	name   string // data file name
	header string // header file name, empty when the export wrote none
	label  string // node label or edge type
	edge   bool
}

// classify recognizes the export's own file names. The engine writes exactly
// three files per label — `<kind>_<LABEL>_header.csv`, `_data.csv` and
// `_cypher.csv` — and the last is a Neo4j load script, not data: reading it as
// CSV would fail on its first line. Anything else in the directory is ignored
// rather than guessed at.
func classify(name string) (kind string, label string, part string, ok bool) {
	rest, isNode := strings.CutPrefix(name, "nodes_")
	if !isNode {
		var isEdge bool
		rest, isEdge = strings.CutPrefix(name, "edges_")
		if !isEdge {
			return "", "", "", false
		}
		kind = "edges"
	} else {
		kind = "nodes"
	}
	for _, suffix := range []string{"_header.csv", "_data.csv", "_cypher.csv"} {
		if l, cut := strings.CutSuffix(rest, suffix); cut {
			if l == "" || len(l) > maxLabelBytes {
				return "", "", "", false
			}
			return kind, l, strings.TrimSuffix(strings.TrimPrefix(suffix, "_"), ".csv"), true
		}
	}
	return "", "", "", false
}

// importExport streams every recognized CSV file of one export directory into
// the scratch, in sorted name order so a run reads them the same way every
// time. Returns the bytes consumed.
func importExport(ctx context.Context, sc *scratch, dir string, maxRecordBytes int64) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, outputInvalid("the export directory cannot be read: %v", err)
	}
	if len(entries) > maxExportFiles {
		return 0, resourceLimit("the export has %d entries, over the %d bound", len(entries), maxExportFiles).WithDetail("limit", "max_export_files")
	}
	files := map[string]*exportFile{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		kind, label, part, ok := classify(ent.Name())
		if !ok || part == "cypher" {
			sc.ignoredFiles++
			continue
		}
		key := kind + "/" + label
		f := files[key]
		if f == nil {
			f = &exportFile{label: label, edge: kind == "edges"}
			files[key] = f
		}
		if part == "header" {
			f.header = ent.Name()
		} else {
			f.name = ent.Name()
		}
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var total int64
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		f := files[k]
		if f.name == "" {
			continue
		}
		var header []string
		if f.header != "" {
			h, n, err := readHeaderFile(filepath.Join(dir, f.header), maxRecordBytes)
			total += n
			if err != nil {
				return total, err
			}
			header = h
		}
		n, err := importCSVFile(ctx, sc, filepath.Join(dir, f.name), f.label, f.edge, header, maxRecordBytes)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func readHeaderFile(path string, maxRecordBytes int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, outputInvalid("export header %s cannot be opened: %v", filepath.Base(path), err)
	}
	defer f.Close()
	br := &boundedRecordReader{r: f, limit: maxRecordBytes}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	rec, err := r.Read()
	if err != nil {
		return nil, br.consumed, csvError(path, err)
	}
	return rec, br.consumed, nil
}

// importCSVFile streams one data file. The reader reuses its record slice, so
// the only allocation that scales with the file is the bounded record buffer.
func importCSVFile(ctx context.Context, sc *scratch, path, label string, edge bool, header []string, maxRecordBytes int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, outputInvalid("export CSV %s cannot be opened: %v", filepath.Base(path), err)
	}
	defer f.Close()
	br := &boundedRecordReader{r: f, limit: maxRecordBytes}
	r := csv.NewReader(br)
	r.ReuseRecord = true
	r.FieldsPerRecord = -1
	if header == nil {
		rec, err := r.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return br.consumed, nil
			}
			return br.consumed, csvError(path, err)
		}
		header = append([]string(nil), rec...)
	}
	cols, err := parseHeader(header)
	if err != nil {
		return br.consumed, err
	}
	r.FieldsPerRecord = len(header)
	if edge != (cols.start >= 0 && cols.end >= 0) {
		return br.consumed, outputInvalid("export CSV %s does not carry the columns its name declares", filepath.Base(path))
	}
	if !edge && cols.id < 0 {
		return br.consumed, outputInvalid("export CSV %s has no :ID column", filepath.Base(path))
	}
	for {
		if err := ctx.Err(); err != nil {
			return br.consumed, err
		}
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return br.consumed, nil
		}
		if err != nil {
			return br.consumed, csvError(path, err)
		}
		if edge {
			kind := label
			if cols.typ >= 0 && rec[cols.typ] != "" {
				kind = rec[cols.typ]
			}
			if len(kind) > maxLabelBytes || rec[cols.start] == "" || rec[cols.end] == "" {
				return br.consumed, outputInvalid("export CSV %s has an edge row with an empty endpoint or an oversized type", filepath.Base(path))
			}
			if err := sc.putEdge(ctx, rec[cols.start], rec[cols.end], kind, cols.get(rec, "VARIABLE")); err != nil {
				return br.consumed, err
			}
			continue
		}
		n := graphNode{id: rec[cols.id], label: label}
		if cols.label >= 0 && rec[cols.label] != "" {
			// A node may carry several labels separated by ';'; the engine
			// writes one.
			n.label, _, _ = strings.Cut(rec[cols.label], ";")
		}
		if n.id == "" || len(n.label) > maxLabelBytes {
			return br.consumed, outputInvalid("export CSV %s has a node row with an empty id or an oversized label", filepath.Base(path))
		}
		if n.label == labelMetaData {
			if lang := cols.get(rec, "LANGUAGE"); lang != "" {
				if err := sc.putMeta(ctx, "language", lang); err != nil {
					return br.consumed, err
				}
			}
			continue
		}
		n.name, n.fullName, n.signature = cols.get(rec, "NAME"), cols.get(rec, "FULL_NAME"), cols.get(rec, "SIGNATURE")
		n.canonicalName, n.filename, n.code = cols.get(rec, "CANONICAL_NAME"), cols.get(rec, "FILENAME"), cols.get(rec, "CODE")
		n.methodFullName, n.typeFullName = cols.get(rec, "METHOD_FULL_NAME"), cols.get(rec, "TYPE_FULL_NAME")
		n.closureBinding = cols.get(rec, "CLOSURE_BINDING_ID")
		n.line = optInt(cols.get(rec, "LINE_NUMBER"))
		n.lineEnd = optInt(cols.get(rec, "LINE_NUMBER_END"))
		n.col = optInt(cols.get(rec, "COLUMN_NUMBER"))
		n.argIndex = optInt(cols.get(rec, "ARGUMENT_INDEX"))
		n.isExternal = strings.EqualFold(strings.TrimSpace(cols.get(rec, "IS_EXTERNAL")), "true")
		if err := sc.putNode(ctx, n); err != nil {
			return br.consumed, err
		}
	}
}

func optInt(s string) nullable {
	v, ok := nullInt(s)
	return nullable{Int64: v, Valid: ok}
}

// csvError keeps a typed limit typed and reports anything else as a malformed
// export, naming the file but never quoting its content.
func csvError(path string, err error) error {
	if typed := asTyped(err); typed != nil {
		return typed
	}
	return outputInvalid("export CSV %s is malformed: %v", filepath.Base(path), err)
}

func asTyped(err error) *model.Error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}
