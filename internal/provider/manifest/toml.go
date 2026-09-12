package manifest

import (
	"bytes"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// tomlLayout locates keys in a TOML document by line. The pinned decoder
// reports no key positions, so evidence ranges come from a bounded textual
// scan of the exact bytes: a table header line opens a section, and a key is
// the first line of that section that begins with the key followed by `=` or
// `.`. A key the scan cannot place yields a fact without a range rather than
// a guessed one.
type tomlLayout struct {
	lines    []span // byte span of each line, without its newline
	sections []tomlSection
}

type tomlSection struct {
	name       string // e.g. "dependencies", "target.x86.dependencies"
	first, end int    // line index range of the section body, half open
}

// maxTOMLLines bounds the layout: one line span is 16 bytes, so a manifest
// of many very short lines would otherwise amplify max_parse_file_bytes
// several times over in the unit's heap. Over the cap the layout is empty,
// and every fact of that manifest carries evidence without a range — the
// documented degradation, never a guessed range.
const maxTOMLLines = 200000

func layoutTOML(data []byte) tomlLayout {
	var l tomlLayout
	if bytes.Count(data, []byte("\n")) >= maxTOMLLines {
		return l
	}
	for off := 0; off <= len(data); {
		next := bytes.IndexByte(data[off:], '\n')
		if next < 0 {
			l.lines = append(l.lines, span{off, len(data)})
			break
		}
		l.lines = append(l.lines, span{off, off + next})
		off += next + 1
	}
	l.sections = append(l.sections, tomlSection{name: "", first: 0})
	for i, ln := range l.lines {
		text := strings.TrimSpace(string(data[ln.start:ln.end]))
		if !strings.HasPrefix(text, "[") {
			continue
		}
		// A `[table]` or `[[array-of-tables]]` header opens a section.
		l.sections[len(l.sections)-1].end = i
		l.sections = append(l.sections, tomlSection{name: tomlHeader(text), first: i + 1})
	}
	l.sections[len(l.sections)-1].end = len(l.lines)
	return l
}

// tomlHeader extracts the dotted table name from a header line, stripping
// quotes around components.
func tomlHeader(text string) string {
	text = strings.TrimLeft(text, "[")
	if i := strings.IndexByte(text, ']'); i >= 0 {
		text = text[:i]
	}
	parts := strings.Split(text, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, ".")
}

// key returns the byte span of the line assigning key inside table, or of
// the header `[table.key]` when the key is written as its own sub-table.
func (l tomlLayout) key(data []byte, table, key string) (span, bool) {
	for _, s := range l.sections {
		if s.name == table {
			for i := s.first; i < s.end; i++ {
				ln := l.lines[i]
				if tomlLineAssigns(string(data[ln.start:ln.end]), key) {
					return ln, true
				}
			}
		}
		if s.name == table+"."+key && s.first > 0 {
			return l.lines[s.first-1], true
		}
	}
	return span{}, false
}

// tomlLineAssigns reports whether a line begins with key (bare or quoted)
// followed by `=` or `.`.
func tomlLineAssigns(line, key string) bool {
	line = strings.TrimSpace(line)
	for _, form := range []string{key, `"` + key + `"`, `'` + key + `'`} {
		rest, ok := strings.CutPrefix(line, form)
		if !ok {
			continue
		}
		rest = strings.TrimLeft(rest, " \t")
		if strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, ".") {
			return true
		}
	}
	return false
}

// section returns the byte span of a whole table body.
func (l tomlLayout) section(table string) (span, bool) {
	for _, s := range l.sections {
		if s.name == table && s.first < s.end {
			return span{l.lines[s.first].start, l.lines[s.end-1].end}, true
		}
	}
	return span{}, false
}

// keyValue returns the span from a key's line to the end of its value, which
// for an array may run over several lines until the closing bracket.
func (l tomlLayout) keyValue(data []byte, table, key string) (span, bool) {
	start, ok := l.key(data, table, key)
	if !ok {
		return span{}, false
	}
	text := data[start.start:start.end]
	open := bytes.Count(text, []byte("[")) - bytes.Count(text, []byte("]"))
	end := start.end
	for i := lineIndex(l, start.start) + 1; open > 0 && i < len(l.lines); i++ {
		ln := l.lines[i]
		open += bytes.Count(data[ln.start:ln.end], []byte("[")) - bytes.Count(data[ln.start:ln.end], []byte("]"))
		end = ln.end
	}
	return span{start.start, end}, true
}

func lineIndex(l tomlLayout, offset int) int {
	for i, ln := range l.lines {
		if ln.start <= offset && offset <= ln.end {
			return i
		}
	}
	return len(l.lines)
}

// quoted finds the first quoted occurrence of value inside s and returns its
// span including the quotes.
func quoted(data []byte, s span, value string) (span, bool) {
	body := data[s.start:s.end]
	for _, q := range []string{`"`, `'`} {
		if i := bytes.Index(body, []byte(q+value+q)); i >= 0 {
			return span{s.start + i, s.start + i + len(value) + 2}, true
		}
	}
	return span{}, false
}

// rangeOf converts a located span to a source range.
func (u *unit) rangeOf(s span, ok bool) *model.SourceRange {
	if !ok {
		return nil
	}
	return u.rng(s.start, s.end)
}
