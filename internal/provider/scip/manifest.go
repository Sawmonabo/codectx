package scip

import (
	"bufio"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The document manifest is the per-document membership of one sealed SCIP
// unit (Section 11.4, "Delta import"): the sorted list of the root-relative
// paths the import admitted, each with the canonical hash of the SCIP
// Document it was built from. It exists so a refreshed unit can be applied as
// a per-document delta instead of a rewrite: the managed indexer still
// re-indexes the whole unit, but only the documents whose canonical hash
// changed produce facts, and the stored paths the fresh index no longer
// contains are deleted.
//
// It is a file, never a Go slice. A unit may hold up to Limits.MaxDocuments
// documents, and Section 6 forbids retaining a whole-repository file list in
// the Go heap, so every operation here streams: Load validates and counts,
// Save copies, and Diff is a merge join over two sorted files with one line
// of each in memory at a time.
const (
	// manifestDocumentHeader is the first line of a document manifest. It is
	// not the input-hash manifest of Options.Manifest, which is a different
	// file with a different job (proving the index describes the pinned
	// bytes); a reader that confuses them fails on the header.
	manifestDocumentHeader = "codectx-scip-documents v1"

	// documentHashDomain frames the canonical document hash. The version is
	// part of the domain: changing what the hash covers yields a disjoint
	// hash space, so an old manifest can never be read as if it described the
	// new composition. Provider.Version must be bumped with it, because the
	// unit fingerprint is what makes an old manifest reachable at all.
	documentHashDomain = "scip-document-v1"

	// manifestSeparator is the two-space separator of a manifest row,
	// matching the input-hash manifest format documented in
	// docs/providers-scip.md.
	manifestSeparator = "  "
)

// DocumentManifest names a document-manifest file on disk. It carries no
// document list: everything is read from the file on demand.
type DocumentManifest struct {
	name  string
	count int64
	// owned is true for the manifest an import wrote under its own work
	// directory. Close removes only an owned file, so closing a manifest that
	// was loaded from the caller's storage never deletes the caller's file.
	owned bool
}

// Class is what a delta says about one path.
type Class string

const (
	// ClassChanged is a path the fresh index describes with a different
	// canonical document hash than the stored unit, or does not hold at all.
	// Its stored rows are deleted and the fresh facts inserted.
	ClassChanged Class = "changed"
	// ClassRemoved is a path the stored unit holds and the fresh index does
	// not describe. A deleted source file yields no Document at all rather
	// than an empty one, so removal is set-based, never a tombstone
	// (docs/research/12-incremental-scip-lsp.md Section 6.2).
	ClassRemoved Class = "removed"
	// ClassUnchanged is a path whose canonical document hash and pinned
	// content hash are both unchanged. Its stored rows are retained.
	ClassUnchanged Class = "unchanged"
)

// Change is one path of a delta.
type Change struct {
	Path  string
	Class Class
}

// Delta counts the three classes of one refresh.
type Delta struct {
	Changed   int64
	Removed   int64
	Unchanged int64
}

// Name is the absolute path of the manifest file.
func (m *DocumentManifest) Name() string { return m.name }

// Len is the number of documents the manifest describes.
func (m *DocumentManifest) Len() int64 { return m.count }

// LoadDocumentManifest opens a stored manifest and validates it completely:
// the header, one `<hash>  <path>` row per document, hex hashes, root-relative
// paths and strictly increasing path order. A manifest that fails any of these
// is refused rather than silently treated as an empty previous state, because
// an empty previous state imports everything and quietly destroys the sharing
// the caller asked for.
func LoadDocumentManifest(name string) (*DocumentManifest, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, internal("scip document manifest: " + err.Error())
	}
	defer f.Close()
	sc := manifestScanner(f)
	if !sc.Scan() || sc.Text() != manifestDocumentHeader {
		if err := sc.Err(); err != nil {
			return nil, internal("scip document manifest: " + err.Error())
		}
		return nil, invalid("scip document manifest " + name + " does not start with " + manifestDocumentHeader)
	}
	var count int64
	var previous string
	for sc.Scan() {
		_, p, err := parseManifestRow(sc.Text())
		if err != nil {
			return nil, err
		}
		if count > 0 && p <= previous {
			return nil, invalid("scip document manifest " + name + " is not sorted by path")
		}
		previous = p
		count++
	}
	if err := sc.Err(); err != nil {
		return nil, internal("scip document manifest: " + err.Error())
	}
	return &DocumentManifest{name: name, count: count}, nil
}

// Save copies the manifest to dst through a temporary file in dst's directory
// and one rename, so a crash never leaves a half-written manifest that a later
// refresh would read as authoritative document membership.
func (m *DocumentManifest) Save(dst string) error {
	if !filepath.IsAbs(dst) {
		return invalid("a scip document manifest is saved to an absolute path")
	}
	src, err := os.Open(m.name)
	if err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".scip-documents-")
	if err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return internal("scip document manifest: " + err.Error())
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return internal("scip document manifest: " + err.Error())
	}
	if err := tmp.Close(); err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	return nil
}

// Close removes the file of a manifest this package wrote under its own work
// directory. A manifest opened with LoadDocumentManifest owns nothing and
// Close is a no-op on it.
func (m *DocumentManifest) Close() error {
	if m == nil || !m.owned || m.name == "" {
		return nil
	}
	name := m.name
	m.owned = false
	if err := os.Remove(name); err != nil && !os.IsNotExist(err) {
		return internal("scip document manifest: " + err.Error())
	}
	return nil
}

// Diff classifies every path of the fresh manifest m against the stored
// manifest prev and reports the counts. A nil prev is a full import: every
// path is changed.
//
// The Changed, Removed and Unchanged sets are delivered through fn rather than
// returned as slices: a unit may describe up to Limits.MaxDocuments documents
// and Section 6 forbids holding that list in the Go heap. fn may be nil when
// only the counts are wanted. Both files are sorted by path, so this is one
// merge join with one line of each side live at a time.
func (m *DocumentManifest) Diff(prev *DocumentManifest, fn func(Change) error) (Delta, error) {
	var delta Delta
	emit := func(p string, c Class) error {
		switch c {
		case ClassChanged:
			delta.Changed++
		case ClassRemoved:
			delta.Removed++
		case ClassUnchanged:
			delta.Unchanged++
		}
		if fn == nil {
			return nil
		}
		return fn(Change{Path: p, Class: c})
	}
	fresh, err := openManifestRows(m)
	if err != nil {
		return Delta{}, err
	}
	defer fresh.close()
	stored, err := openManifestRows(prev)
	if err != nil {
		return Delta{}, err
	}
	defer stored.close()
	for {
		a, aok, err := fresh.peek()
		if err != nil {
			return Delta{}, err
		}
		b, bok, err := stored.peek()
		if err != nil {
			return Delta{}, err
		}
		switch {
		case !aok && !bok:
			return delta, nil
		case !bok || (aok && a.path < b.path):
			fresh.next()
			if err := emit(a.path, ClassChanged); err != nil {
				return Delta{}, err
			}
		case !aok || b.path < a.path:
			stored.next()
			if err := emit(b.path, ClassRemoved); err != nil {
				return Delta{}, err
			}
		default:
			fresh.next()
			stored.next()
			class := ClassUnchanged
			if a.hash != b.hash {
				class = ClassChanged
			}
			if err := emit(a.path, class); err != nil {
				return Delta{}, err
			}
		}
	}
}

// manifestRow is one parsed manifest line.
type manifestRow struct {
	hash string
	path string
}

// manifestRows is a one-row lookahead over a manifest file. A nil manifest is
// an empty stream, which is what a full import diffs against.
type manifestRows struct {
	f      *os.File
	sc     *bufio.Scanner
	row    manifestRow
	loaded bool
	done   bool
}

func openManifestRows(m *DocumentManifest) (*manifestRows, error) {
	if m == nil {
		return &manifestRows{done: true}, nil
	}
	f, err := os.Open(m.name)
	if err != nil {
		return nil, internal("scip document manifest: " + err.Error())
	}
	sc := manifestScanner(f)
	if !sc.Scan() || sc.Text() != manifestDocumentHeader {
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, internal("scip document manifest: " + err.Error())
		}
		return nil, invalid("scip document manifest " + m.name + " does not start with " + manifestDocumentHeader)
	}
	return &manifestRows{f: f, sc: sc}, nil
}

func (r *manifestRows) peek() (manifestRow, bool, error) {
	if r.loaded {
		return r.row, true, nil
	}
	if r.done {
		return manifestRow{}, false, nil
	}
	if !r.sc.Scan() {
		r.done = true
		if err := r.sc.Err(); err != nil {
			return manifestRow{}, false, internal("scip document manifest: " + err.Error())
		}
		return manifestRow{}, false, nil
	}
	hash, p, err := parseManifestRow(r.sc.Text())
	if err != nil {
		return manifestRow{}, false, err
	}
	r.row, r.loaded = manifestRow{hash: hash, path: p}, true
	return r.row, true, nil
}

func (r *manifestRows) next() { r.loaded = false }

func (r *manifestRows) close() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}

// manifestScanner bounds a manifest line at one hash, the separator and one
// path, so a corrupt file cannot size an allocation.
func manifestScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), model.IDHexLen+len(manifestSeparator)+model.MaxPathBytes)
	return sc
}

// parseManifestRow splits and validates one `<hash>  <path>` row.
func parseManifestRow(line string) (hash, p string, err error) {
	hash, p, ok := strings.Cut(line, manifestSeparator)
	if !ok || !model.ValidHexID(hash) {
		return "", "", invalid("scip document manifest row does not start with a document hash")
	}
	if !rootRelative(p) {
		return "", "", invalid("scip document manifest names a path that is not inside the project root")
	}
	return hash, p, nil
}

// rootRelative reports whether a SCIP `relative_path` names a file inside the
// project root and can be written to a manifest row.
//
// Section 11.4 requires rejecting a document whose path escapes the project
// root, and it is not a hypothetical: 18 of the 141 documents scip-go emits
// for this repository are the `go test` mains it generates under `$GOCACHE`,
// whose paths are `../../../../../..`-style escapes into a content-addressed
// build cache (docs/research/12-incremental-scip-lsp.md Section 6.2). Admitting
// them would bake absolute machine paths into the index, churn about 13% of the
// document set for unrelated reasons, and produce paths that can never join a
// repository FileID.
//
// A control character is refused for the same reason a path may not escape:
// the manifest is a line-oriented file, and a path holding a newline would
// forge manifest rows.
func rootRelative(p string) bool {
	if p == "" || len(p) > model.MaxPathBytes {
		return false
	}
	if strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return false
		}
	}
	return true
}
