package manifest

import (
	"bytes"
	"context"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

// markdown scans a Markdown document line by line with a small bounded
// scanner (R7-3): ATX headings become section nodes the document contains,
// each with its own name-only search document, and links whose target is a
// workspace path become
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
	// trail is the open heading path, innermost last; sections counts how
	// often a qualified name has already been minted in this document so two
	// identical headings under the same parent stay two sections.
	var trail []section
	sections := map[string]int{}
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
			if title, level, ok := atxHeading(trimmed); ok {
				for len(trail) > 0 && trail[len(trail)-1].level >= level {
					trail = trail[:len(trail)-1]
				}
				trail = append(trail, section{level: level, title: title})
				if headings++; !u.cut(BoundEntries, u.entries, int64(headings)) {
					if err := u.heading(ctx, doc, title, qualifySection(rel, trail, sections), off, lineEnd); err != nil {
						return err
					}
				}
			}
		default:
			for _, l := range linksIn(line) {
				if links++; u.cut(BoundEntries, u.entries, int64(links)) {
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

// section is one open ATX heading of the trail being walked.
type section struct {
	level int
	title string
}

// sectionTrail joins the open heading path for a qualified name, and
// sectionKeyBytes is the budget that name has: the node candidate's native key
// is the name behind the kind prefix, and a candidate over MaxNativeKeyBytes
// fails validation, which would fail the whole unit. A deeply nested document
// whose headings are long reaches it, so the name is truncated and flagged
// with a digest instead -- a length ceiling truncates, it never refuses a file.
const (
	sectionTrail     = " > "
	sectionDomain    = "manifest-section-v1"
	sectionKeyBytes  = model.MaxNativeKeyBytes - len(string(model.NodeSection)+":")
	sectionDigestHex = 16
)

// qualifySection names one heading: the document path, then the trail of
// headings that encloses it. Two headings with the same trail (the same text
// repeated under one parent) are still two sections, so a repeat takes an
// ordinal suffix rather than converging on the identity of the first -- a
// shared identity would fold them back into one search hit, which is the
// defect per-heading nodes exist to remove. The name is content-derived and
// carries no byte offset, so inserting a line above a heading does not re-key
// every section below it.
func qualifySection(rel string, trail []section, sections map[string]int) string {
	var sb strings.Builder
	sb.WriteString(rel)
	sb.WriteByte('#')
	for i, s := range trail {
		if i > 0 {
			sb.WriteString(sectionTrail)
		}
		sb.WriteString(s.title)
	}
	qualified := sb.String()
	n := sections[qualified]
	sections[qualified] = n + 1
	if n > 0 {
		qualified += "~" + strconv.Itoa(n+1)
	}
	if len(qualified) > sectionKeyBytes {
		// The cut prefix is not an identity: two trails sharing it would
		// otherwise become one section. The digest of the full trail keeps
		// them apart and stays content-derived.
		digest := model.H(sectionDomain, qualified)[:sectionDigestHex]
		head, _ := model.TruncateField(qualified, sectionKeyBytes-len(digest)-1)
		qualified = head + "~" + digest
	}
	return qualified
}

// heading emits one heading as a section node the document contains, and
// queues that section's name-only search document: the heading text as the
// name, the heading line as its range, no body. The search key derives from
// the file, its content and the heading's exact offsets; the node's identity
// is the heading trail, so the section survives an edit elsewhere in the file.
func (u *unit) heading(ctx context.Context, doc model.Node, title, qualified string, start, end int) error {
	fv := u.e.File()
	rng := u.rng(start, end)
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: provider.ScopeWorkspace,
		NativeKey: string(model.NodeSection) + ":" + qualified, Kind: model.NodeSection,
		Language: filesystem.Language(fv.Path), Name: title, QualifiedName: qualified,
		FileID: fv.ID, ContentHash: fv.ContentHash}
	node, err := u.e.Node(ctx, cand, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng, Located: true})
	if err != nil {
		return err
	}
	if err := u.e.Relation(doc.ID, model.RelContains, node.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng}); err != nil {
		return err
	}
	u.e.Search(model.SearchUnit{
		ID:     model.H("search-heading-v1", string(fv.ID), fv.ContentHash, strconv.Itoa(start), strconv.Itoa(end)),
		NodeID: node.ID, FileID: fv.ID, Path: fv.Path, Kind: model.NodeSection, Name: title, QualifiedName: qualified,
		Bytes: model.ByteRange{Start: uint64(start), End: uint64(end)},
	})
	return nil
}

// atxHeading parses `#{1,6} title [#*]`, returning the title and its level.
func atxHeading(line []byte) (string, int, bool) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(line) && line[level] != ' ' && line[level] != '\t') {
		return "", 0, false
	}
	title := strings.TrimSpace(string(line[level:]))
	title = strings.TrimRight(title, "#")
	title = strings.TrimSpace(title)
	if title == "" || len(title) > model.MaxNameBytes {
		return "", 0, false
	}
	return title, level, true
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
