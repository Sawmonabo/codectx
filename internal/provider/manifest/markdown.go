package manifest

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

// markdown scans a Markdown document line by line with a small bounded
// scanner (R7-3): ATX headings become name-only search documents of the
// document node, and links whose target is a workspace path become
// `documents` edges to the file or directory they name. Fenced code is
// skipped so a `#` or a link inside a code sample is not structure. The
// scanner claims no complete CommonMark parse; what it does not recognize is
// still searchable through the file's own chunks.
func (u *unit) markdown(ctx context.Context) error {
	rel := u.e.File().Path
	cls := filesystem.Classify(rel)
	// The document node and its `defines` edge are the filesystem unit's;
	// this unit's fact carries the same identity so the heading documents,
	// whose node reference is checked within the unit, can name it.
	doc, err := u.e.Node(ctx, filesystem.PathCandidate(ID, model.NodeDocument, rel),
		filesystem.Attrs{Precision: model.PrecisionSyntax, Located: true, Metadata: filesystem.Metadata(map[string]any{"format": cls.Format})})
	if err != nil {
		return err
	}
	var fence []byte
	headings, links := 0, 0
	seen := map[string]bool{}
	data := u.data
	for off := 0; off < len(data); {
		next := bytes.IndexByte(data[off:], '\n')
		end := len(data)
		if next >= 0 {
			end = off + next
		}
		line := data[off:end]
		lineEnd := end
		if lineEnd > off && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
			lineEnd--
		}
		trimmed := bytes.TrimLeft(line, " ")
		switch {
		case fence != nil:
			if bytes.HasPrefix(trimmed, fence) && len(bytes.Trim(trimmed, string(fence[0]))) == 0 {
				fence = nil
			}
		case bytes.HasPrefix(trimmed, []byte("```")) || bytes.HasPrefix(trimmed, []byte("~~~")):
			fence = trimmed[:3]
		case bytes.HasPrefix(trimmed, []byte("#")):
			if title, ok := atxHeading(trimmed); ok {
				if headings++; headings > MaxEntries {
					u.overBound()
				} else {
					u.heading(doc, title, off, lineEnd)
				}
			}
		default:
			for _, l := range linksIn(line) {
				if links++; links > MaxEntries {
					u.overBound()
					break
				}
				target, dir, ok := linkTarget(l.target)
				if !ok || seen[target] {
					continue
				}
				seen[target] = true
				rng := u.rng(off+l.start, off+l.end)
				node, ok, err := u.pathTarget(ctx, target, dir, rng, model.PrecisionHeuristic)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if err := u.e.Relation(doc.ID, model.RelDocuments, node.ID, filesystem.Attrs{Precision: model.PrecisionHeuristic, Range: rng,
					Detail: filesystem.Detail("link", l.target)}); err != nil {
					return err
				}
			}
		}
		off = end + 1
	}
	return nil
}

// heading queues the name-only search document of one heading: the heading
// text as the name, the heading line as its range, no body. Its key derives
// from the file, its content and the heading's exact offsets.
func (u *unit) heading(doc model.Node, title string, start, end int) {
	fv := u.e.File()
	u.e.Search(model.SearchUnit{
		ID:     model.H("search-heading-v1", string(fv.ID), fv.ContentHash, strconv.Itoa(start), strconv.Itoa(end)),
		NodeID: doc.ID, FileID: fv.ID, Path: fv.Path, Kind: model.NodeDocument, Name: title,
		Bytes: model.ByteRange{Start: uint64(start), End: uint64(end)},
	})
}

// atxHeading parses `#{1,6} title [#*]`.
func atxHeading(line []byte) (string, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(line) && line[level] != ' ' && line[level] != '\t') {
		return "", false
	}
	title := strings.TrimSpace(string(line[level:]))
	title = strings.TrimRight(title, "#")
	title = strings.TrimSpace(title)
	if title == "" || len(title) > model.MaxNameBytes {
		return "", false
	}
	return title, true
}

// link is one link target with its byte span within the line.
type link struct {
	target     string
	start, end int
}

// linksIn finds inline links `[text](target "title")` and reference
// definitions `[label]: target` on one line.
func linksIn(line []byte) []link {
	var out []link
	for i := 0; i < len(line); {
		j := bytes.Index(line[i:], []byte("]("))
		if j < 0 {
			break
		}
		start := i + j + 2
		depth, k := 1, start
		for ; k < len(line) && depth > 0; k++ {
			switch line[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
		}
		if depth != 0 {
			break
		}
		out = append(out, link{target: destination(string(line[start : k-1])), start: start, end: k - 1})
		i = k
	}
	// A reference definition: `[label]: destination`.
	if s := bytes.TrimLeft(line, " "); bytes.HasPrefix(s, []byte("[")) {
		if close := bytes.Index(s, []byte("]:")); close > 1 {
			after := len(line) - len(s) + close + 2
			dest := destination(string(line[after:]))
			if dest != "" {
				if i := bytes.Index(line[after:], []byte(dest)); i >= 0 {
					out = append(out, link{target: dest, start: after + i, end: after + i + len(dest)})
				}
			}
		}
	}
	return out
}

// destination strips a title and angle brackets from a link destination.
func destination(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<") {
		if i := strings.IndexByte(s, '>'); i > 0 {
			return s[1:i]
		}
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}

// linkTarget reduces a destination to a workspace path: fragments and
// queries are dropped, anything with a URL scheme or nothing but a fragment
// is not a path. dir reports a trailing slash.
func linkTarget(dest string) (target string, dir, ok bool) {
	if i := strings.IndexAny(dest, "#?"); i >= 0 {
		dest = dest[:i]
	}
	if dest == "" || hasScheme(dest) {
		return "", false, false
	}
	dir = strings.HasSuffix(dest, "/")
	if unescaped, err := strconv.Unquote(`"` + strings.ReplaceAll(dest, `"`, `\"`) + `"`); err == nil {
		dest = unescaped
	}
	return dest, dir, true
}

// hasScheme reports a `scheme:` prefix per RFC 3986.
func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			return i > 0
		case c == '/' || c == '.':
			return false
		case i == 0 && !isAlpha(c):
			return false
		case !isAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-':
			return false
		}
	}
	return false
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
