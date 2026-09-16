package lsp

import (
	"bytes"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
)

// encodingOf maps the protocol's positionEncoding values to the shared
// source encodings. An unknown value is not guessed.
func encodingOf(wire string) (source.ColumnEncoding, bool) {
	switch wire {
	case "utf-8":
		return source.UTF8, true
	case "utf-16":
		return source.UTF16, true
	case "utf-32":
		return source.UTF32, true
	}
	return "", false
}

// offeredEncodings is what the client advertises, in preference order. UTF-8
// costs nothing to convert; UTF-16 is mandatory in the protocol and therefore
// the fallback every server supports.
var offeredEncodings = []string{"utf-8", "utf-32", "utf-16"}

// document is one pinned file's exact bytes with the cursor that converts
// coordinates against them. Every position the overlay sends or receives for
// this file passes through here. The cursor caches its last line and is not
// safe for concurrent use, so the document serializes conversions: a
// document is shared by every overlay on the server.
type document struct {
	version model.FileVersion
	data    []byte

	mu     sync.Mutex
	cursor *source.Cursor
}

// positionOf converts a byte offset to an LSP position in enc. The line and
// UTF-8 column come from the shared cursor; the column is then recounted in
// the negotiated units over the bytes of that line. An offset that is not a
// rune boundary is rejected by the cursor, never rounded.
func (d *document) positionOf(offset uint64, enc source.ColumnEncoding) (position, error) {
	d.mu.Lock()
	pos, err := d.cursor.PositionAt(offset)
	d.mu.Unlock()
	if err != nil {
		return position{}, err
	}
	lineStart := pos.Byte - uint64(pos.Column)
	units, err := unitsIn(d.data[lineStart:pos.Byte], enc)
	if err != nil {
		return position{}, err
	}
	return position{Line: pos.Line - 1, Character: units}, nil
}

// rangeOf converts an LSP range in enc to a byte-authoritative source range,
// validated against these exact bytes by the shared conversion: a column that
// falls inside a code point, past the line or past the file is an error, and
// so is a range that ends before it starts.
func (d *document) rangeOf(r lspRange, enc source.ColumnEncoding) (model.SourceRange, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cursor.SourceRange(r.Start.Line+1, r.Start.Character, r.End.Line+1, r.End.Character, enc)
}

// unitsIn counts the code units of enc in content, which must be valid UTF-8.
// It is the inverse of the shared column conversion for one line prefix.
func unitsIn(content []byte, enc source.ColumnEncoding) (uint32, error) {
	if enc == source.UTF8 {
		return uint32(len(content)), nil
	}
	var units uint32
	for i := 0; i < len(content); {
		r, size := utf8.DecodeRune(content[i:])
		if r == utf8.RuneError && size <= 1 {
			return 0, invalid("the line is not valid UTF-8, so a %s column cannot be counted", enc)
		}
		units++
		if enc == source.UTF16 && r > 0xFFFF {
			units++
		}
		i += size
	}
	return units, nil
}

// materializationURI is the file URI of one root-relative path inside the
// private materialization, and the inverse mapping back to that path.
type materializationURI struct {
	root string
}

// uri returns the file URI the server should use for rel. The empty rel is
// the materialization root itself, which is how the workspace-root project is
// spelled.
func (m materializationURI) uri(rel string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(m.root, filepath.FromSlash(rel)))}
	return u.String()
}

// relOf maps a URI the server returned back to a root-relative snapshot path.
//
// Only a file URI without an authority is a path at all; anything else is a
// protocol violation, because this client never told the server about any
// other kind of resource and following one would be reading outside the
// repository (Section 11.5: never follow external URIs). A file URI that
// resolves outside the materialization root is not a violation — a server
// legitimately points into a standard library or dependency cache — but it is
// not a snapshot file either: ok is false and the caller excludes it without
// opening anything.
func (m materializationURI) relOf(raw string) (rel string, ok bool, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || !strings.EqualFold(u.Scheme, "file") || u.Host != "" || u.Opaque != "" || u.Path == "" {
		return "", false, outputInvalid("location %q is not a file URI for a materialized path", truncate(raw, 128))
	}
	if strings.ContainsRune(u.Path, 0) {
		return "", false, outputInvalid("location URI contains a NUL byte")
	}
	abs := filepath.Clean(filepath.FromSlash(u.Path))
	if !filepath.IsAbs(abs) {
		return "", false, outputInvalid("location %q is not an absolute file path", truncate(raw, 128))
	}
	r, rerr := filepath.Rel(m.root, abs)
	if rerr != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false, nil
	}
	rel = filepath.ToSlash(r)
	if path.Clean(rel) != rel || strings.HasPrefix(rel, "/") {
		return "", false, nil
	}
	return rel, true, nil
}

// isText reports whether data is valid UTF-8, which is what a didOpen text
// field and a UTF-16 or UTF-32 column both require. A file that is not text
// has no positions a language server can name.
func isText(data []byte) bool {
	return utf8.Valid(data) && !bytes.ContainsRune(data, 0)
}
