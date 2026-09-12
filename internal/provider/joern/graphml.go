package joern

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// boundedTokenReader is the io.ByteReader encoding/xml consumes directly (no
// internal bufio), so the bytes it has read since the last token boundary are
// the bytes of the token being decoded. When they exceed the cap the read
// fails, before the decoder has assembled an unbounded CharData or attribute
// value. The importer calls reset after every token.
type boundedTokenReader struct {
	r        io.Reader
	buf      [4096]byte
	head, n  int
	since    int64
	limit    int64
	consumed int64
}

func (b *boundedTokenReader) ReadByte() (byte, error) {
	if b.head == b.n {
		n, err := b.r.Read(b.buf[:])
		if n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			return 0, err
		}
		b.head, b.n = 0, n
		b.consumed += int64(n)
	}
	c := b.buf[b.head]
	b.head++
	b.since++
	if b.since > b.limit {
		return 0, resourceLimit("a Joern GraphML token exceeds %d bytes", b.limit).WithDetail("limit", "max_export_record_bytes")
	}
	return c, nil
}

func (b *boundedTokenReader) Read(p []byte) (int, error) {
	// encoding/xml uses ReadByte when the reader offers it; Read exists only to
	// satisfy io.Reader and delegates to the same bounded path.
	for i := range p {
		c, err := b.ReadByte()
		if err != nil {
			return i, err
		}
		p[i] = c
	}
	return len(p), nil
}

func (b *boundedTokenReader) reset() { b.since = 0 }

// graphml attribute keys the profile reads. GraphML declares <key id=...
// attr.name=...> and data elements reference the id; Joern uses the attribute
// name as the id ("labelV", "labelE", "NAME"), but the mapping is honoured
// either way.
const (
	gmlLabelV = "labelV"
	gmlLabelE = "labelE"
)

// importGraphML streams every .graphml file of one export directory. Nodes
// are staged with INSERT OR IGNORE, so the CSV export's attributes win and
// the PDG contributes what the CSV lacked; edges deduplicate on their key.
// Depth, token size and file count are bounded; entities and DTDs are not
// resolved. Returns the bytes consumed.
func importGraphML(ctx context.Context, sc *scratch, dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, outputInvalid("the Joern GraphML export directory cannot be read: %v", err)
	}
	if len(entries) > maxExportFiles {
		return 0, resourceLimit("the Joern GraphML export has %d entries, over the %d bound", len(entries), maxExportFiles).WithDetail("limit", "max_export_files")
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".graphml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var total int64
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := importGraphMLFile(ctx, sc, filepath.Join(dir, name))
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

type gmlElement struct {
	kind         string // "node" or "edge"
	id, src, dst string
	data         map[string]string
	dataKey      string
	text         strings.Builder
}

func importGraphMLFile(ctx context.Context, sc *scratch, path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, outputInvalid("Joern GraphML %s cannot be opened: %v", filepath.Base(path), err)
	}
	defer f.Close()
	br := &boundedTokenReader{r: f, limit: maxRecordBytes}
	dec := xml.NewDecoder(br)
	dec.Strict = true
	dec.Entity = map[string]string{}
	keys := map[string]string{}
	var depth int
	var cur *gmlElement
	for {
		if err := ctx.Err(); err != nil {
			return br.consumed, err
		}
		tok, err := dec.Token()
		br.reset()
		if errors.Is(err, io.EOF) {
			return br.consumed, nil
		}
		if err != nil {
			if typed := asTyped(err); typed != nil {
				return br.consumed, typed
			}
			return br.consumed, outputInvalid("Joern GraphML %s is malformed: %v", filepath.Base(path), err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxXMLDepth {
				return br.consumed, resourceLimit("Joern GraphML %s nests deeper than %d elements", filepath.Base(path), maxXMLDepth).WithDetail("limit", "max_xml_depth")
			}
			switch t.Name.Local {
			case "key":
				id, name := attr(t, "id"), attr(t, "attr.name")
				if id != "" {
					if name == "" {
						name = id
					}
					keys[id] = name
				}
			case "node", "edge":
				if cur != nil {
					return br.consumed, outputInvalid("Joern GraphML %s nests a %s inside another element", filepath.Base(path), t.Name.Local)
				}
				cur = &gmlElement{kind: t.Name.Local, id: attr(t, "id"), src: attr(t, "source"), dst: attr(t, "target"), data: map[string]string{}}
				if l := attr(t, "label"); l != "" {
					// Some exporters put the label on the element itself.
					cur.data[gmlLabelE] = l
				}
			case "data":
				if cur != nil {
					cur.dataKey = attr(t, "key")
					cur.text.Reset()
				}
			}
		case xml.CharData:
			if cur != nil && cur.dataKey != "" {
				// One data value may arrive as several CharData tokens (comments
				// split it); the value as a whole is still bounded.
				if int64(cur.text.Len()+len(t)) > maxRecordBytes {
					return br.consumed, resourceLimit("a Joern GraphML data value exceeds %d bytes", maxRecordBytes).WithDetail("limit", "max_export_record_bytes")
				}
				cur.text.Write(t)
			}
		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "data":
				if cur != nil && cur.dataKey != "" {
					name := keys[cur.dataKey]
					if name == "" {
						name = cur.dataKey
					}
					if len(cur.data) < maxFields {
						cur.data[name] = cur.text.String()
					}
					cur.dataKey = ""
				}
			case "node", "edge":
				if cur == nil {
					continue
				}
				if err := stageGraphMLElement(ctx, sc, path, cur); err != nil {
					return br.consumed, err
				}
				cur = nil
			}
		}
	}
}

func stageGraphMLElement(ctx context.Context, sc *scratch, path string, e *gmlElement) error {
	if e.kind == "edge" {
		label := e.data[gmlLabelE]
		if e.src == "" || e.dst == "" || len(label) > maxLabelBytes {
			return outputInvalid("Joern GraphML %s has an edge with an empty endpoint or an oversized label", filepath.Base(path))
		}
		// A PDG edge label may carry the variable after the type ("DDG: x").
		variable := e.data["VARIABLE"]
		if typ, rest, ok := strings.Cut(label, ":"); ok {
			label = strings.TrimSpace(typ)
			if variable == "" {
				variable = strings.TrimSpace(rest)
			}
		}
		return sc.putEdge(ctx, e.src, e.dst, label, variable)
	}
	label := e.data[gmlLabelV]
	if e.id == "" || len(label) > maxLabelBytes {
		return outputInvalid("Joern GraphML %s has a node with an empty id or an oversized label", filepath.Base(path))
	}
	n := graphNode{id: e.id, label: label}
	d := e.data
	fillNode(&n, d["NAME"], d["FULL_NAME"], d["SIGNATURE"], d["FILENAME"], d["LINE_NUMBER"], d["LINE_NUMBER_END"],
		d["COLUMN_NUMBER"], d["CODE"], d["IS_EXTERNAL"], d["METHOD_FULL_NAME"])
	return sc.putNode(ctx, n)
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
