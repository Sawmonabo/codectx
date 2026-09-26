package scip

import (
	"bufio"
	"context"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
)

// Delta-import state of one run (Section 11.4, "Delta import"). Everything
// here is disk-backed in the run's scratch database for the same reason the
// symbol map is: a unit may describe up to Limits.MaxDocuments documents and
// no whole-repository list may live in the Go heap.
//
//   - docpath is filled by the binding pre-pass with the *last* index each
//     relative_path appears at. scip.proto calls relative_path unique but
//     defines no behaviour for a duplicate, and the format's own two reference
//     consumers disagree (FlattenDocuments unions, expt-convert keeps the
//     first), so this importer picks one rule and counts it: the last document
//     wins and the earlier ones are dropped
//     (docs/research/12-incremental-scip-lsp.md Section 1.2).
//   - dochash spools one canonical digest per occurrence and per symbol of the
//     document being walked, so the document hash can be taken over a sorted
//     multiset without holding the document.
//   - prevdoc is the stored unit's manifest, looked up per document, with the
//     occurrences its import refused in each document.
//   - docdelta is the fresh manifest under construction plus the publish
//     decision every later pass filters on, and the occurrences refused in
//     each document (see carryRefusals).
//   - projects holds the directories of this kind's other projects, whose
//     documents belong to their own units (loadProjects, ownerOf).
//
// The four tables are created inside the import's transaction, but the
// scratch database runs with the journal off, where a rollback is not
// guaranteed to undo anything, so scratchEmpty drops them like every other
// table of the pooled surface.
var deltaSchema = []string{
	`CREATE TABLE docpath(path TEXT PRIMARY KEY, last INTEGER NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE dochash(doc INTEGER NOT NULL, digest TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY(doc, digest, seq)) WITHOUT ROWID`,
	`CREATE TABLE prevdoc(path TEXT PRIMARY KEY, hash TEXT NOT NULL, refused INTEGER NOT NULL) WITHOUT ROWID`,
	`CREATE TABLE docdelta(path TEXT PRIMARY KEY, doc INTEGER NOT NULL UNIQUE, hash TEXT NOT NULL, publish INTEGER NOT NULL,
		refused INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID`,
	`CREATE TABLE projects(dir TEXT PRIMARY KEY) WITHOUT ROWID`,
}

// Digest domains. Each frames one canonical record; the suffix means a change
// to what a digest covers cannot be mistaken for a changed document.
const (
	occurrenceDigestDomain = "scip-occurrence-v1"
	symbolDigestDomain     = "scip-symbol-v1"
)

// openDelta creates the delta tables and loads the stored manifest.
func (im *importer) openDelta(ctx context.Context) error {
	for _, stmt := range deltaSchema {
		if err := im.sc.exec(ctx, stmt); err != nil {
			return err
		}
	}
	if im.previous == nil {
		return nil
	}
	f, err := os.Open(im.previous.Name())
	if err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	defer f.Close()
	sc := manifestScanner(f)
	if !sc.Scan() || sc.Text() != manifestDocumentHeader {
		if err := sc.Err(); err != nil {
			return internal("scip document manifest: " + err.Error())
		}
		return invalid("scip document manifest " + im.previous.Name() + " does not start with " + manifestDocumentHeader)
	}
	for sc.Scan() {
		row, err := parseManifestRow(sc.Text())
		if err != nil {
			return err
		}
		if err := im.sc.charge(int64(len(sc.Text()))); err != nil {
			return err
		}
		if err := im.sc.exec(ctx, `INSERT OR REPLACE INTO prevdoc(path, hash, refused) VALUES(?, ?, ?)`, row.path, row.hash, row.refused); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return internal("scip document manifest: " + err.Error())
	}
	return nil
}

// seeDocument is the binding pre-pass's record of one document: it rejects a
// path that escapes the workspace root (rootRelative), leaves a document of
// another project to that project's unit and refuses one no project owns
// (ownerOf), and resolves the duplicate-path rule. It reports whether the
// document is one this import may admit at all; a document it refuses has no
// docpath row, so admits refuses it in every later pass.
func (im *importer) seeDocument(ctx context.Context, d document) (bool, error) {
	if !rootRelative(d.path) {
		// Counted, not degraded: see the details in importer.go.
		im.outsideRoot.note(d.path)
		return false, nil
	}
	owner, err := im.ownerOf(ctx, d.path)
	if err != nil {
		return false, err
	}
	switch owner {
	case otherUnit:
		im.otherProjectDocs++
		return false, nil
	case noUnit:
		// A file the snapshot does not hold is the not-held case, which
		// describes no served bytes; a held one is served with facts no
		// unit publishes, which is a loss.
		if _, held, err := im.lookup(ctx, d.path); err != nil || !held {
			if err == nil {
				im.notHeld.note(d.path)
			}
			return false, err
		}
		im.unowned.note(d.path)
		im.degrade(model.CodeProviderOutputInvalid)
		return false, nil
	}
	var last int64
	found, err := im.sc.row(ctx, `SELECT last FROM docpath WHERE path = ?`, []any{d.path}, &last)
	if err != nil {
		return false, err
	}
	if found {
		im.duplicatePath.note(d.path)
		im.degrade(model.CodeProviderOutputInvalid)
	} else if err := im.sc.charge(int64(len(d.path)) + 16); err != nil {
		return false, err
	}
	return true, im.sc.exec(ctx, `INSERT OR REPLACE INTO docpath(path, last) VALUES(?, ?)`, d.path, d.index)
}

// loadProjects records, for a profile unit, every plannable project of the
// same kind in the workspace other than the unit's own (projectAt). Each is
// planned as a unit of its own by Scopes, and no profile's argument array can
// exclude a subtree from a run, so a unit drops the documents another project
// owns at seeDocument instead: one path is published by exactly one unit. The
// set is read from the pinned snapshot the run materialized, so it is the set
// of projects that snapshot holds, and it is spooled to the scratch database
// rather than held, as every per-repository set here is.
func (im *importer) loadProjects(ctx context.Context) error {
	if im.profile == nil {
		return nil
	}
	k, root := im.profile.Kind, im.profile.Root
	return im.req.Content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		dir, ok := projectAt(k, fv.Path)
		if !ok || dir == root {
			return nil
		}
		if err := im.sc.charge(int64(len(dir)) + 16); err != nil {
			return err
		}
		im.hasOtherProjects = true
		return im.sc.exec(ctx, `INSERT OR IGNORE INTO projects(dir) VALUES(?)`, dir)
	})
}

// docOwner is which unit publishes a workspace-relative path.
type docOwner int

const (
	// ownUnit: the path is inside this unit's project and no nested project
	// of the same kind holds it.
	ownUnit docOwner = iota
	// otherUnit: another project loadProjects recorded holds it, so that
	// project's unit publishes it.
	otherUnit
	// noUnit: the path lies outside this unit's project, reached through
	// `../`, and no project holds it, so no unit may publish it.
	noUnit
)

// ownerOf answers which unit publishes the workspace-relative path: the unit
// of the innermost project directory that holds it. The walk looks up each of
// the path's directories from the innermost out and stops at this unit's own
// root, so a nested project claims its subtree. A path the unit reaches
// through `../` is admitted only through the project that owns it, the
// workspace root's included: two sibling units that both reach a directory no
// project declares would otherwise each admit its files, and one path would be
// published twice. A supplied index names workspace paths and belongs to no
// project, so it owns every path it holds.
func (im *importer) ownerOf(ctx context.Context, p string) (docOwner, error) {
	if im.profile == nil {
		return ownUnit, nil
	}
	root := im.profile.Root
	inside := root == "" || strings.HasPrefix(p, root+"/")
	if !im.hasOtherProjects {
		if inside {
			return ownUnit, nil
		}
		return noUnit, nil
	}
	for dir := p; ; {
		if i := strings.LastIndexByte(dir, '/'); i >= 0 {
			dir = dir[:i]
		} else {
			dir = ""
		}
		if dir == root {
			return ownUnit, nil
		}
		var one int
		found, err := im.sc.row(ctx, `SELECT 1 FROM projects WHERE dir = ?`, []any{dir}, &one)
		switch {
		case err != nil:
			return ownUnit, err
		case found:
			return otherUnit, nil
		case dir == "":
			// Only a path outside this unit's root walks past it to the
			// workspace root without meeting it.
			return noUnit, nil
		}
	}
}

// admits reports whether the document at this index is the one its path
// resolves to. An earlier document with a path a later one repeats is dropped:
// concatenating a fresh document onto a stale one is not replacement, and the
// wire format carries no recency marker to break the tie, so the position in
// the index is the only ordering there is.
func (im *importer) admits(ctx context.Context, d document) (bool, error) {
	if !rootRelative(d.path) {
		return false, nil
	}
	var last int64
	found, err := im.sc.row(ctx, `SELECT last FROM docpath WHERE path = ?`, []any{d.path}, &last)
	if err != nil {
		return false, err
	}
	return found && last == d.index, nil
}

// addDigest spools one canonical record digest of the document being walked.
func (im *importer) addDigest(ctx context.Context, doc int64, digest string) error {
	if err := im.sc.charge(int64(len(digest)) + 24); err != nil {
		return err
	}
	im.digestSeq++
	return im.sc.exec(ctx, `INSERT INTO dochash(doc, digest, seq) VALUES(?, ?, ?)`, doc, digest, im.digestSeq)
}

// occurrenceDigest is the canonical digest of one Occurrence.
//
// It covers exactly the fields this provider turns into facts, in the
// normalized four-value range form, so a producer that switches between the
// deprecated packed range and the typed single/multi-line range, or that emits
// a three-value single-line range instead of a four-value one, does not report
// a changed document; and a field this provider ignores (syntax_kind,
// diagnostics, override_documentation) cannot mark a document changed when no
// fact of ours would differ.
func occurrenceDigest(o occurrence) string {
	h := model.NewHasher(occurrenceDigestDomain)
	h.AddString(o.symbol)
	h.AddString(strconv.FormatInt(int64(o.roles), 10))
	r := pad(o.rng)
	for _, v := range r {
		h.AddString(strconv.FormatInt(int64(v), 10))
	}
	if o.hasEnclosing {
		e := pad(o.enclosing)
		for _, v := range e {
			h.AddString(strconv.FormatInt(int64(v), 10))
		}
	}
	return h.Sum()
}

// symbolDigest is the canonical digest of one SymbolInformation. Its
// relationships are sorted, because they are a set: case L of
// docs/research/12-incremental-scip-lsp.md Section 3.3 is a document that
// changed by exactly one relationship on one symbol, which is the shape this
// digest exists to catch.
func symbolDigest(s symbolInfo) string {
	h := model.NewHasher(symbolDigestDomain)
	h.AddString(s.symbol)
	h.AddString(strconv.FormatInt(int64(s.kind), 10))
	h.AddString(s.displayName)
	h.AddString(s.signature)
	rels := make([]string, 0, len(s.relationships))
	for _, r := range s.relationships {
		rels = append(rels, strconv.FormatInt(int64(r.flags), 10)+" "+r.symbol)
	}
	slices.Sort(rels)
	for _, r := range rels {
		h.AddString(r)
	}
	return h.Sum()
}

// documentHash finalizes the canonical hash of one document and drops its
// spooled digests. The digests are folded in sorted order, so a producer that
// emits the same occurrences in a different order does not report a changed
// document; the document's `text` is excluded, because indexers do not emit it
// (measured: docs_with_text=0 on all six) and it is not a fact this provider
// publishes.
//
// The pinned file's content hash is part of the document hash. Without it a
// document that is byte-identical in the index while its source file changed —
// a trailing newline, a comment edited after the last declaration — would be
// classified unchanged, and the retained evidence rows would then name content
// hashes that are not the pinned bytes. That is the one way a delta import can
// serve wrong source as compiler evidence, so the hash closes it.
func (im *importer) documentHash(ctx context.Context, d docRow) (string, error) {
	h := model.NewHasher(documentHashDomain)
	h.AddString(d.path)
	h.AddString(d.language)
	h.AddString(strconv.FormatInt(int64(d.encoding), 10))
	h.AddString(d.file.ContentHash)
	err := im.sc.each(ctx, `SELECT digest FROM dochash WHERE doc = ? ORDER BY digest, seq`, []any{d.idx},
		func(scan func(...any) error) error {
			var digest string
			if err := scan(&digest); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			h.AddString(digest)
			return nil
		})
	if err != nil {
		return "", err
	}
	if err := im.sc.exec(ctx, `DELETE FROM dochash WHERE doc = ?`, d.idx); err != nil {
		return "", err
	}
	return h.Sum(), nil
}

// classify records one admitted document in the fresh manifest and decides
// whether its facts are published. A document whose canonical hash equals the
// stored unit's is unchanged: the storage writer keeps that path's rows and
// this run emits nothing for it.
func (im *importer) classify(ctx context.Context, d docRow, hash string) (publish bool, err error) {
	publish = true
	if im.previous != nil {
		var stored string
		found, err := im.sc.row(ctx, `SELECT hash FROM prevdoc WHERE path = ?`, []any{d.path}, &stored)
		if err != nil {
			return false, err
		}
		publish = !found || stored != hash
	}
	if err := im.sc.charge(int64(len(d.path)+len(hash)) + 32); err != nil {
		return false, err
	}
	flag := 0
	if publish {
		flag = 1
	}
	return publish, im.sc.exec(ctx, `INSERT OR REPLACE INTO docdelta(path, doc, hash, publish) VALUES(?, ?, ?, ?)`, d.path, d.idx, hash, flag)
}

// writeManifest streams the fresh manifest to a private file under the
// provider's work directory, sorted by path. The caller owns the result: Save
// copies it to durable storage, Close removes it.
func (im *importer) writeManifest(ctx context.Context) (*DocumentManifest, error) {
	if err := os.MkdirAll(im.p.workDir, 0o700); err != nil {
		return nil, internal("scip work directory: " + err.Error())
	}
	f, err := os.CreateTemp(im.p.workDir, "scip-documents-")
	if err != nil {
		return nil, internal("scip document manifest: " + err.Error())
	}
	m := &DocumentManifest{name: f.Name(), owned: true}
	w := bufio.NewWriter(f)
	fail := func(err error) (*DocumentManifest, error) {
		f.Close()
		paced.Remove(m.name)
		return nil, err
	}
	if _, err := w.WriteString(manifestDocumentHeader + "\n"); err != nil {
		return fail(internal("scip document manifest: " + err.Error()))
	}
	err = im.sc.each(ctx, `SELECT hash, refused, path FROM docdelta ORDER BY path`, nil, func(scan func(...any) error) error {
		var hash, p string
		var refused int64
		if err := scan(&hash, &refused, &p); err != nil {
			return internal("scip scratch read: " + err.Error())
		}
		m.count++
		_, err := w.WriteString(hash + manifestSeparator + strconv.FormatInt(refused, 10) + manifestSeparator + p + "\n")
		return err
	})
	if err != nil {
		return fail(err)
	}
	if err := w.Flush(); err != nil {
		return fail(internal("scip document manifest: " + err.Error()))
	}
	if err := f.Sync(); err != nil {
		return fail(internal("scip document manifest: " + err.Error()))
	}
	if err := f.Close(); err != nil {
		paced.Remove(m.name)
		return nil, internal("scip document manifest: " + err.Error())
	}
	return m, nil
}

// addRefused records n occurrences refused in the document at doc by this
// run, in the unit's total and in the document's fresh manifest row.
func (im *importer) addRefused(ctx context.Context, doc, n int64) error {
	if n == 0 {
		return nil
	}
	im.refusedOccurrences += n
	return im.sc.exec(ctx, `UPDATE docdelta SET refused = refused + ? WHERE doc = ?`, n, doc)
}

// carryRefusals is addRefused for an unchanged document, whose rows the
// storage writer carries from the sealed unit. Only its definitions are
// re-read by this run; its references are not, so the occurrences its
// original import refused are taken from the stored manifest instead. That is
// exact rather than an estimate: a refusal is a function of the document's
// occurrences, symbols, resolved encoding and pinned bytes, which the
// canonical document hash covers, so an equal hash refuses the same
// occurrences. Without it a refresh would publish the carried document's
// missing occurrences as fresh.
func (im *importer) carryRefusals(ctx context.Context, d docRow) error {
	var stored int64
	if _, err := im.sc.row(ctx, `SELECT refused FROM prevdoc WHERE path = ?`, []any{d.path}, &stored); err != nil {
		return err
	}
	if stored == 0 {
		return nil
	}
	if im.carriedExemplar == "" {
		im.carriedExemplar = d.path
	}
	im.degrade(model.CodeProviderOutputInvalid)
	return im.addRefused(ctx, d.idx, stored)
}
