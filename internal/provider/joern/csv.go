package joern

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
)

// boundedRecordReader hands encoding/csv its bytes while enforcing the record
// cap itself. It tracks quote state so a quoted field containing newlines is
// still one record, and fails as soon as more than limit bytes have been read
// since the last record boundary, before the csv reader has grown its buffer
// to hold them. csv.Reader returns the underlying error unwrapped, so the
// typed limit surfaces to the caller.
type boundedRecordReader struct {
	r        io.Reader
	limit    int64
	since    int64
	inQuote  bool
	consumed int64
	limitKey string
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
			return 0, resourceLimit("a Joern export record exceeds %d bytes", b.limit).WithDetail("limit", b.limitKey)
		}
	}
	b.consumed += int64(n)
	return n, err
}

// csvColumns is one decoded Neo4j header: the special role columns and the
// property columns by name (type suffix stripped).
type csvColumns struct {
	id, label, start, end, typ int
	props                      map[string]int
}

func parseHeader(fields []string) (csvColumns, error) {
	if len(fields) == 0 || len(fields) > maxFields {
		return csvColumns{}, outputInvalid("a Joern CSV header has %d columns; the profile admits 1..%d", len(fields), maxFields)
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
				return csvColumns{}, outputInvalid("a Joern CSV header has an unknown role column %q", f)
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

func nullInt(s string) (v int64, ok bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// importNeo4jCSV streams every CSV file of one export directory into the
// scratch. Joern's Neo4j exporter writes a header file and a data file per
// label (nodes_<LABEL>_header.csv / nodes_<LABEL>_data.csv, likewise
// edges_<TYPE>_*); a data file with no header sibling is read with its first
// row as header. Files are visited in sorted name order so a run reads them
// the same way every time. Returns the bytes consumed.
func importNeo4jCSV(ctx context.Context, sc *scratch, dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, outputInvalid("the Joern CSV export directory cannot be read: %v", err)
	}
	if len(entries) > maxExportFiles {
		return 0, resourceLimit("the Joern CSV export has %d entries, over the %d bound", len(entries), maxExportFiles).WithDetail("limit", "max_export_files")
	}
	names := make([]string, 0, len(entries))
	headers := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".csv") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_header.csv") {
			headers[e.Name()] = true
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".csv"), "_data")
		var header []string
		if headers[base+"_header.csv"] {
			h, n, err := readHeaderFile(filepath.Join(dir, base+"_header.csv"))
			total += n
			if err != nil {
				return total, err
			}
			header = h
		}
		n, err := importCSVFile(ctx, sc, filepath.Join(dir, name), header)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func readHeaderFile(path string) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, outputInvalid("Joern CSV header %s cannot be opened: %v", filepath.Base(path), err)
	}
	defer f.Close()
	br := &boundedRecordReader{r: f, limit: maxRecordBytes, limitKey: "max_export_record_bytes"}
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
func importCSVFile(ctx context.Context, sc *scratch, path string, header []string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, outputInvalid("Joern CSV %s cannot be opened: %v", filepath.Base(path), err)
	}
	defer f.Close()
	br := &boundedRecordReader{r: f, limit: maxRecordBytes, limitKey: "max_export_record_bytes"}
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
	isEdge := cols.start >= 0 && cols.end >= 0
	if !isEdge && cols.id < 0 {
		return br.consumed, outputInvalid("Joern CSV %s has neither an :ID nor :START_ID/:END_ID column", filepath.Base(path))
	}
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return br.consumed, nil
		}
		if err != nil {
			return br.consumed, csvError(path, err)
		}
		if isEdge {
			label := ""
			if cols.typ >= 0 {
				label = rec[cols.typ]
			}
			if len(label) > maxLabelBytes || rec[cols.start] == "" || rec[cols.end] == "" {
				return br.consumed, outputInvalid("Joern CSV %s has an edge row with an empty endpoint or an oversized type", filepath.Base(path))
			}
			if err := sc.putEdge(ctx, rec[cols.start], rec[cols.end], label, cols.get(rec, "VARIABLE")); err != nil {
				return br.consumed, err
			}
			continue
		}
		n := graphNode{id: rec[cols.id]}
		if cols.label >= 0 {
			// A node may carry several labels separated by ';'; Joern writes one.
			n.label, _, _ = strings.Cut(rec[cols.label], ";")
		}
		if n.id == "" || len(n.label) > maxLabelBytes {
			return br.consumed, outputInvalid("Joern CSV %s has a node row with an empty id or an oversized label", filepath.Base(path))
		}
		fillNode(&n, cols.get(rec, "NAME"), cols.get(rec, "FULL_NAME"), cols.get(rec, "SIGNATURE"), cols.get(rec, "FILENAME"),
			cols.get(rec, "LINE_NUMBER"), cols.get(rec, "LINE_NUMBER_END"), cols.get(rec, "COLUMN_NUMBER"), cols.get(rec, "CODE"),
			cols.get(rec, "IS_EXTERNAL"), cols.get(rec, "METHOD_FULL_NAME"))
		if n.label == labelMetaData {
			if lang := cols.get(rec, "LANGUAGE"); lang != "" {
				if err := sc.putMeta(ctx, "language", lang); err != nil {
					return br.consumed, err
				}
			}
		}
		if err := sc.putNode(ctx, n); err != nil {
			return br.consumed, err
		}
	}
}

// fillNode decodes the attribute strings both importers produce.
func fillNode(n *graphNode, name, fullName, signature, filename, line, lineEnd, col, code, isExternal, methodFullName string) {
	n.name, n.fullName, n.signature, n.filename, n.code, n.methodFullName = name, fullName, signature, filename, code, methodFullName
	if v, ok := nullInt(line); ok {
		n.line.Int64, n.line.Valid = v, true
	}
	if v, ok := nullInt(lineEnd); ok {
		n.lineEnd.Int64, n.lineEnd.Valid = v, true
	}
	if v, ok := nullInt(col); ok {
		n.col.Int64, n.col.Valid = v, true
	}
	n.isExternal = strings.EqualFold(strings.TrimSpace(isExternal), "true")
}

// csvError keeps a typed limit typed and reports anything else as a malformed
// export, naming the file but never quoting its content.
func csvError(path string, err error) error {
	if typed := asTyped(err); typed != nil {
		return typed
	}
	return outputInvalid("Joern CSV %s is malformed: %v", filepath.Base(path), err)
}
