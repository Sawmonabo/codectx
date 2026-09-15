package scip

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/source"
)

// opener yields the index bytes and their size: the snapshot view for a
// supplied index, a private run file for a profile's output.
type opener func(context.Context) (io.ReadCloser, int64, error)

// fileNativeKey is the workspace-scoped native key prefix of a file node. It
// is the filesystem provider's key for the same node (R7-2), repeated here
// because that provider lives in another wave worktree; after the wave merges
// it and the candidate in enclosingNode consolidate into one shared file-node
// helper beside filesystem.PathCandidate.
const fileNativeKey = "file:"

// importer runs one unit. The import is two passes over the index plus one
// binding pre-pass, all streaming:
//
//   - pre-pass: document paths and embedded text hashes decide the source
//     binding before any fact exists, because the binding decides whether an
//     occurrence that does not land on the pinned bytes fails the unit
//     (verified: the index claims to describe these bytes and does not) or
//     is skipped (unverified: discovery over bytes the index never saw). It
//     also resolves the duplicate-path rule and rejects documents whose path
//     escapes the project root, both of which must be known before the first
//     fact of the first document is published;
//   - pass 1: occurrences and symbols are spooled to the scratch database as
//     they stream past, each contributing its canonical digest to the
//     document hash; when a document ends its position encoding is known, so
//     its definitions are converted against the pinned bytes, resolved and
//     recorded in the on-disk symbol map, and published unless the document
//     is unchanged;
//   - pass 2: references of the changed documents are read back from the
//     spool with the symbol map complete, so a forward or external reference
//     resolves to the same identity a later definition minted; edges are
//     grouped by relation identity on disk and published with one evidence
//     row per distinct occurrence range.
//
// Definitions are converted and resolved for **every** admitted document, not
// only the changed ones, while facts are published only for changed
// documents. A definition's canonical key is its file and byte range
// (internal/reconcile.CanonicalKey), so an unchanged document's definitions
// must be re-derived here or a reference from a changed document to a symbol
// defined in an unchanged one would mint a second, unlocated identity instead
// of naming the stored node — the measured "46 dangling occurrence errors" of
// an incomplete splice, in the other direction
// (docs/research/12-incremental-scip-lsp.md Section 3.5). The delta therefore
// buys the storage, FTS and reconcile cost of the unchanged documents, which
// is where almost all of the import cost lives; it does not buy their
// position-conversion cost.
type importer struct {
	p        *Provider
	req      provider.UnitRequest
	sink     provider.Sink
	sc       *scratch
	profile  *Profile
	binding  model.SourceBinding
	unverify bool // binding is unverified: skip, never fail, on a bad position
	meta     metadata
	// ctx is the run context, carried for the walker's document callback,
	// which takes none.
	ctx context.Context

	// previous is the stored unit's document manifest, or nil for a full
	// import; manifest and delta are this run's result.
	previous  *DocumentManifest
	manifest  *DocumentManifest
	delta     Delta
	digestSeq int64

	// indexHash is the SHA-256 of the index bytes being imported, which a
	// supplied manifest must commit to; manifestUsable records whether the
	// supplied manifest proved to be a verification rather than an assertion.
	indexHash      string
	manifestUsable bool
	// manifestSHA is the digest of the input manifest a profile run wrote.
	manifestSHA string

	release    func()
	reserved   int64
	records    uint64
	indexBytes uint64

	skippedDocs, skippedOccurrences, truncatedEdges int64
	outsideRoot, duplicatePaths, skippedAliases     int64
	assumedEncoding                                 int64
	partialCode                                     string
	// drops is the one account of everything the wire decoder discarded for
	// exceeding a field bound, across every pass of this import.
	drops decodeDrops
	// seen is the one account of the largest figure this import observed at
	// each user-settable bound, across every pass. It is compared with the
	// configured bounds once, in result.
	seen limitSeen
}

// Bound names the seven providers.scip.* bounds are reported under. They are
// the configuration keys themselves, so an operator reading a capability row
// reads the name of the key to raise.
const (
	limitIndexBytes             = "max_index_bytes"
	limitManifestBytes          = "max_manifest_bytes"
	limitDocuments              = "max_documents"
	limitOccurrencesPerDocument = "max_occurrences_per_document"
	limitSpoolBytes             = "max_spool_bytes"
	limitSourceFileBytes        = "max_source_file_bytes"
)

// detailLimitsExceeded is the capability-row detail the crossed bounds are
// published under, as one sorted `key=seen/bound` list so the value is
// reproducible across runs of the same index and costs one detail slot
// whatever crossed.
const detailLimitsExceeded = "resource_limits_exceeded"

// limitSeen records the largest figure observed at each bound. It is a
// measurement, not a gate: nothing consults it until the run is over, which
// is what makes an unset bound cost nothing and a set one report rather than
// refuse.
type limitSeen struct{ m map[string]int64 }

func (l *limitSeen) note(bound string, v int64) {
	if l == nil {
		return
	}
	if l.m == nil {
		l.m = map[string]int64{}
	}
	if v > l.m[bound] {
		l.m[bound] = v
	}
}

// exceeded renders the bounds a user set and this run crossed, sorted, or "".
func (l *limitSeen) exceeded(lim Limits) string {
	bounds := []struct {
		name string
		v    config.Limit
	}{{limitIndexBytes, lim.MaxIndexBytes}, {limitManifestBytes, lim.MaxManifestBytes},
		{limitDocuments, lim.MaxDocuments}, {limitOccurrencesPerDocument, lim.MaxOccurrencesPerDocument},
		{limitSpoolBytes, lim.MaxSpoolBytes}, {limitSourceFileBytes, lim.MaxSourceFileBytes}}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].name < bounds[j].name })
	var b strings.Builder
	for _, bd := range bounds {
		seen := int64(0)
		if l != nil && l.m != nil {
			seen = l.m[bd.name]
		}
		if !bd.v.Exceeded(seen) {
			continue
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%d/%d", bd.name, seen, int64(bd.v))
	}
	return b.String()
}

// location is a pinned file plus a byte range on it: where evidence points.
type location struct {
	file model.FileVersion
	rng  *model.SourceRange
}

// docSource is one document's exact bytes with its position cursor.
type docSource struct {
	doc  docRow
	data []byte
	cur  *source.Cursor
	enc  source.ColumnEncoding
}

// docRow is the scratch record of one document bound to a snapshot file.
type docRow struct {
	idx      int64
	path     string
	language string
	encoding int32
	file     model.FileVersion
}

// run imports one index. open is called once per pass.
func (im *importer) run(ctx context.Context, open opener) error {
	if err := im.openDelta(ctx); err != nil {
		return err
	}
	if im.profile == nil && im.p.manifestPath != "" {
		if err := im.loadManifest(ctx); err != nil {
			return err
		}
	}
	binding, meta, err := im.scanBinding(ctx, open)
	if err != nil {
		return err
	}
	im.binding, im.meta = binding, meta
	im.unverify = binding == model.SourceBindingUnverified
	if im.profile != nil {
		if err := checkTool(*im.profile, meta); err != nil {
			return err
		}
	}
	if im.unverify {
		im.partialCode = model.CodeSourceBindingUnverified
	}
	if err := im.passDefinitions(ctx, open); err != nil {
		return err
	}
	if err := im.passReferences(ctx); err != nil {
		return err
	}
	if err := im.passRelationships(ctx); err != nil {
		return err
	}
	if err := im.emitEdges(ctx); err != nil {
		return err
	}
	// The fresh manifest is written from the documents this import admitted,
	// and the delta the run reports is read back out of it by Diff, so the
	// counts a caller sees and the classification a later refresh recomputes
	// from the stored manifests come from one implementation.
	m, err := im.writeManifest(ctx)
	if err != nil {
		return err
	}
	im.manifest = m
	if im.delta, err = m.Diff(im.previous, nil); err != nil {
		return err
	}
	return nil
}

// scanBinding streams the index once, reading only paths and text, and
// decides the Section 11.4 source binding. A codectx-invoked profile analyzed
// a private materialization of exactly these bytes and is verified by
// construction. A supplied index is verified only when every document that
// names a snapshot file proves its bytes, by embedded text whose hash is the
// pinned content hash or by a matching entry in the supplied input-hash
// manifest; anything less is unverified, never upgraded.
func (im *importer) scanBinding(ctx context.Context, open opener) (model.SourceBinding, metadata, error) {
	var meta metadata
	var verified, unverified int64
	w := &walker{limits: im.p.limits, onGrow: im.reserve, drops: &im.drops, seen: &im.seen,
		onMetadata: func(m metadata) error { meta = m; return nil },
		onDocument: func(d document) error {
			admitted, err := im.seeDocument(ctx, d)
			if err != nil || !admitted {
				return err
			}
			fv, ok, err := im.lookup(ctx, d.path)
			if err != nil || !ok {
				return err
			}
			if d.hasText && d.textHash == fv.ContentHash {
				verified++
				return nil
			}
			if im.manifestUsable {
				var hash string
				if found, err := im.sc.row(ctx, `SELECT hash FROM manifest WHERE path = ?`, []any{d.path}, &hash); err != nil {
					return err
				} else if found && hash == fv.ContentHash {
					verified++
					return nil
				}
			}
			unverified++
			return nil
		}}
	if err := im.walk(ctx, open, w); err != nil {
		return "", meta, err
	}
	if im.profile != nil {
		return model.SourceBindingVerified, meta, nil
	}
	if verified > 0 && unverified == 0 {
		return model.SourceBindingVerified, meta, nil
	}
	return model.SourceBindingUnverified, meta, nil
}

// walk opens the index and streams it through w.
func (im *importer) walk(ctx context.Context, open opener, w *walker) error {
	rc, size, err := open(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	im.seen.note(limitIndexBytes, size)
	consumed, err := w.walk(ctx, bufio.NewReaderSize(rc, 64<<10), size)
	im.indexBytes += uint64(consumed)
	return err
}

// reserve charges the walker's record buffer against the sink's pool when
// the sink supports reservations (the production BatchSink does), releasing
// the previous charge first so exactly one reservation is outstanding.
func (im *importer) reserve(ctx context.Context, n int64) error {
	r, ok := im.sink.(interface {
		Reserve(context.Context, int64) (func(), error)
	})
	if !ok || n <= im.reserved {
		return nil
	}
	release, err := r.Reserve(ctx, n)
	if err != nil {
		return err
	}
	if im.release != nil {
		im.release()
	}
	im.release, im.reserved = release, n
	return nil
}

// decorate names the profile run behind a failure. A profile unit's
// diagnostics have to carry the declared network posture and the digest of
// the input manifest that binds the run, because the manifest file itself
// dies with the run directory.
func (im *importer) decorate(err error) error {
	if im.profile == nil {
		return err
	}
	return im.p.profileError(*im.profile, im.manifestSHA, err)
}

func (im *importer) close() {
	if im.release != nil {
		im.release()
		im.release = nil
	}
}

// lookup finds the snapshot file at a document's relative path.
func (im *importer) lookup(ctx context.Context, p string) (model.FileVersion, bool, error) {
	if p == "" || strings.HasPrefix(p, "/") || path.Clean(p) != p || len(p) > model.MaxPathBytes {
		return model.FileVersion{}, false, nil
	}
	var out model.FileVersion
	var found bool
	err := im.req.Content.EachFile(ctx, model.FileSelection{Paths: []string{p}}, func(fv model.FileVersion) error {
		if fv.Status != model.FileDeleted {
			out, found = fv, true
		}
		return nil
	})
	return out, found, err
}

// loadManifest streams the supplied input-hash manifest into the scratch
// table and decides whether it verifies anything at all. Its lines are
// bounded and the file is a snapshot input like the index itself.
//
// A bare list of `<sha256>  <path>` lines is a user assertion, not a
// verification: nothing ties it to the index being imported, so the same list
// would "prove" any index. A supplied manifest therefore qualifies only when
//
//   - its first line is the v1 header and its second names the SHA-256 of the
//     exact index bytes this unit is importing, and
//   - every row names a file the snapshot holds at exactly that content hash.
//
// Anything else leaves manifestUsable false, so the manifest proves nothing
// and the binding falls back to embedded text: unverified rather than
// exact, never a hard failure, because a manifest that does not describe this
// snapshot is an ordinary discovery import (Section 11.4).
func (im *importer) loadManifest(ctx context.Context) error {
	fv, ok, err := im.lookup(ctx, im.p.manifestPath)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	im.seen.note(limitManifestBytes, fv.Size)
	rc, _, err := im.req.Content.Open(ctx, fv.ID)
	if err != nil {
		return err
	}
	defer rc.Close()
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 4096), model.IDHexLen+2+model.MaxPathBytes)
	var lineNo, rows int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		lineNo++
		switch lineNo {
		case 1:
			if line != manifestHeader {
				return nil
			}
			continue
		case 2:
			declared, ok := strings.CutPrefix(line, manifestIndexLine)
			if declared = strings.TrimSpace(declared); !ok || declared != im.indexHash {
				return nil
			}
			continue
		}
		hash, rest, ok := strings.Cut(line, " ")
		if !ok || !model.ValidHexID(hash) {
			return nil
		}
		listed := strings.TrimLeft(rest, " ")
		pinned, found, err := im.lookup(ctx, listed)
		if err != nil {
			return err
		}
		if !found || pinned.ContentHash != hash {
			return nil
		}
		if err := im.sc.charge(int64(len(line))); err != nil {
			return err
		}
		if err := im.sc.exec(ctx, `INSERT OR REPLACE INTO manifest(path, hash) VALUES(?, ?)`, listed, hash); err != nil {
			return err
		}
		rows++
	}
	if err := sc.Err(); err != nil {
		return malformed("input-hash manifest cannot be read: " + err.Error())
	}
	if lineNo < 2 || rows == 0 {
		return nil
	}
	im.manifestUsable = true
	return nil
}

// passDefinitions is pass 1: spool occurrences and symbols, bind each
// document to its snapshot file when it ends, and publish its definitions.
func (im *importer) passDefinitions(ctx context.Context, open opener) error {
	w := &walker{limits: im.p.limits, onGrow: im.reserve, drops: &im.drops, seen: &im.seen,
		onOccurrence: func(doc, seq int64, o occurrence, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			if err := im.addDigest(ctx, doc, occurrenceDigest(o)); err != nil {
				return err
			}
			r := pad(o.rng)
			var e [4]any
			if o.hasEnclosing {
				enc := pad(o.enclosing)
				e = [4]any{enc[0], enc[1], enc[2], enc[3]}
			}
			return im.sc.exec(ctx, `INSERT INTO occ(doc, seq, symbol, roles, r0, r1, r2, r3, e0, e1, e2, e3) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				doc, seq, o.symbol, o.roles, r[0], r[1], r[2], r[3], e[0], e[1], e[2], e[3])
		},
		onSymbol: func(doc int64, s symbolInfo, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			if err := im.addDigest(ctx, doc, symbolDigest(s)); err != nil {
				return err
			}
			return im.recordSymbol(ctx, doc, s)
		},
		// Index.external_symbols is an Index field, not a Document field
		// (scip.proto). It is index-level state: it belongs to no path, it
		// contributes to no document hash, and a refresh always re-imports it
		// rather than sharing it per document.
		onExternal: func(s symbolInfo, n int64) error {
			if err := im.sc.charge(n); err != nil {
				return err
			}
			return im.recordSymbol(ctx, -1, s)
		},
		onDocument: im.endDocument,
	}
	return im.walk(ctx, open, w)
}

// pad widens a three-value single-line range to four values.
func pad(r []int32) [4]int32 {
	if len(r) == 3 {
		return [4]int32{r[0], r[1], r[0], r[2]}
	}
	return [4]int32{r[0], r[1], r[2], r[3]}
}

// symKey scopes a symbol in the map: a local symbol to its document, a
// global one to itself (Section 9.4: SCIP locals are document scoped).
func symKey(doc int64, sym symbol) string {
	if sym.local {
		return "l:" + strconv.FormatInt(doc, 10) + ":" + sym.raw
	}
	return "g:" + sym.raw
}

// recordSymbol upserts a SymbolInformation record. Fields already known are
// kept, so a later duplicate can add a kind or name but never erase the node
// a definition assigned.
func (im *importer) recordSymbol(ctx context.Context, doc int64, s symbolInfo) error {
	sym, err := parseSymbol(s.symbol)
	if err != nil {
		return err
	}
	key := symKey(doc, sym)
	name := s.displayName
	if name == "" {
		name = sym.lastName
	}
	// sigcut follows sig through the same arm: the stored signature and the
	// original length it was cut from are one fact, and a twice-seen symbol
	// keeping one from each row would publish a flag about a value it does
	// not hold.
	if err := im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig, sigcut) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET kind = CASE WHEN sym.kind = 0 THEN excluded.kind ELSE sym.kind END,
		name = CASE WHEN sym.name = '' THEN excluded.name ELSE sym.name END,
		sigcut = CASE WHEN sym.sig = '' THEN excluded.sigcut ELSE sym.sigcut END,
		sig = CASE WHEN sym.sig = '' THEN excluded.sig ELSE sym.sig END`, key, s.symbol, s.kind, name, s.signature, s.signatureCut); err != nil {
		return err
	}
	for _, rel := range s.relationships {
		if err := im.sc.exec(ctx, `INSERT OR IGNORE INTO rel(key, target, flags) VALUES(?, ?, ?)`, key, rel.symbol, rel.flags); err != nil {
			return err
		}
	}
	return nil
}

// endDocument binds a finished document to its snapshot file, classifies it
// against the stored manifest and resolves its definitions. A document the
// snapshot does not hold, or whose encoding the indexer left unspecified and
// the per-tool table cannot supply, or whose supplied encoding does not hold
// against the pinned bytes (encodingHolds), or whose file exceeds the source
// bound, is skipped and counted: nothing is guessed about it, and it stays out
// of the fresh manifest, so a later refresh treats it as new rather than
// inheriting rows nothing stands behind.
//
// Definitions are always resolved; they are published only when the document
// is changed. See the type comment for why the unchanged ones cannot simply be
// walked past.
func (im *importer) endDocument(d document) error {
	ctx := im.ctx
	admitted, err := im.admits(ctx, d)
	if err != nil {
		return err
	}
	if !admitted {
		// Rejected by the project-root rule or superseded by a later
		// document with the same path; both were counted in the pre-pass.
		return im.dropSpool(ctx, d.index)
	}
	fv, ok, err := im.lookup(ctx, d.path)
	if err != nil {
		return err
	}
	encoding, assumed := im.resolveEncoding(d.encoding)
	switch {
	case !ok:
		im.skippedDocs++
		return im.dropSpool(ctx, d.index)
	case columnEncoding(encoding) == "":
		im.skippedDocs++
		im.degrade(model.CodeProviderOutputInvalid)
		return im.dropSpool(ctx, d.index)
	case im.p.limits.MaxSourceFileBytes.Exceeded(fv.Size):
		// The one bound that still leaves something out, because it is the
		// one that bounds heap: the source is held whole while positions are
		// converted. Unlimited by default, so this arm is unreachable until
		// an operator asks for it, and the skip is reported under the key.
		im.seen.note(limitSourceFileBytes, fv.Size)
		im.skippedDocs++
		im.degrade(model.CodeResourceLimit)
		return im.dropSpool(ctx, d.index)
	}
	row := docRow{idx: d.index, path: d.path, language: d.language, encoding: encoding, file: fv}
	// The pinned bytes are read before the document is classified, because an
	// encoding taken from the per-tool table is a claim about these bytes that
	// has to hold before the document may contribute a hash, a manifest row or
	// a fact. A document whose guess does not hold is skipped and counted like
	// one whose encoding was never stated: it stays out of the fresh manifest,
	// so a later refresh treats it as new rather than inheriting rows nothing
	// stands behind.
	ds, err := im.loadSource(ctx, row)
	if err != nil {
		return err
	}
	if assumed {
		holds, err := im.encodingHolds(ctx, ds)
		if err != nil {
			return err
		}
		if !holds {
			im.skippedDocs++
			im.degrade(model.CodeProviderOutputInvalid)
			return im.dropSpool(ctx, d.index)
		}
		im.assumedEncoding++
	}
	hash, err := im.documentHash(ctx, row)
	if err != nil {
		return err
	}
	publish, err := im.classify(ctx, row, hash)
	if err != nil {
		return err
	}
	if err := im.sc.exec(ctx, `INSERT INTO docs(idx, path, lang, enc, file_id, content_hash, size) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		row.idx, row.path, row.language, row.encoding, string(fv.ID), fv.ContentHash, fv.Size); err != nil {
		return err
	}
	if err := im.sc.each(ctx, `SELECT seq, symbol, roles, r0, r1, r2, r3, e0, e1, e2, e3 FROM occ WHERE doc = ? AND (roles & ?) <> 0 ORDER BY seq`,
		[]any{row.idx, roleDefinition}, func(scan func(...any) error) error {
			var seq int64
			var symbolText string
			var roles int32
			var r [4]int32
			var e [4]*int32
			if err := scan(&seq, &symbolText, &roles, &r[0], &r[1], &r[2], &r[3], &e[0], &e[1], &e[2], &e[3]); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			if symbolText == "" {
				return nil
			}
			var enclosing *[4]int32
			if e[0] != nil && e[1] != nil && e[2] != nil && e[3] != nil {
				enclosing = &[4]int32{*e[0], *e[1], *e[2], *e[3]}
			}
			return im.defineSymbol(ctx, ds, symbolText, r, enclosing, publish)
		}); err != nil {
		return err
	}
	if publish {
		return nil
	}
	// An unchanged document emits no reference, so its occurrences are not
	// read again.
	return im.dropSpool(ctx, row.idx)
}

// dropSpool discards the spooled records of a document no later pass reads.
// The spool bound counts bytes ever spooled, not bytes live, so this bounds
// the scratch file rather than refunding the charge.
func (im *importer) dropSpool(ctx context.Context, doc int64) error {
	if err := im.sc.exec(ctx, `DELETE FROM occ WHERE doc = ?`, doc); err != nil {
		return err
	}
	return im.sc.exec(ctx, `DELETE FROM dochash WHERE doc = ?`, doc)
}

// degrade records the first reason the unit's capabilities are partial. An
// unverified binding outranks every later reason.
func (im *importer) degrade(code string) {
	if im.partialCode == "" {
		im.partialCode = code
	}
}

// columnEncoding maps a SCIP position encoding to the shared source
// vocabulary; unspecified maps to "" and is never guessed (Section 9.3).
func columnEncoding(enc int32) source.ColumnEncoding {
	switch enc {
	case encodingUTF8:
		return source.UTF8
	case encodingUTF16:
		return source.UTF16
	case encodingUTF32:
		return source.UTF32
	}
	return ""
}

// toolPositionEncoding is the measured column encoding of each indexer build
// the product ships, used only when a Document leaves `position_encoding`
// unspecified.
//
// The key is the **pair** `Metadata.tool_info.{name, version}`, never the name
// alone. A name-keyed table is an assertion about every build a tool will ever
// have, and a wrong encoding does not announce itself: converting a UTF-16
// column as a byte offset yields a valid, in-range, rune-aligned and wrong
// extent for most occurrences, so the facts are published at compiler
// precision against bytes that are not the symbol. Each pair below was
// measured on this machine against a fixture whose line carries a 4-byte and a
// 2-byte rune before an identifier, decoding every occurrence range under all
// three encodings:
//
//	scip-go 0.2.7          position_encoding absent   columns are UTF-8
//	scip-clang 0.4.0       position_encoding absent   columns are UTF-8
//	scip-typescript 0.4.0  position_encoding absent   columns are UTF-16
//	scip-python 0.6.6      position_encoding absent   columns are UTF-16
//	scip-java 0.0.0        position_encoding absent   columns are UTF-16
//	                       (the only build measured reports "0.0.0-SNAPSHOT")
//
// scip-python is UTF-16 rather than the UTF-32 scip.proto suggests for Python
// indexers, because it is a TypeScript program (a pyright fork), which is why
// the table is measured and not read off the proto's advice.
//
// rust-analyzer is deliberately absent: measured at 1.98.0 it declares
// `position_encoding = UTF8` on every document, so it never reaches this
// table; a build that stopped declaring it would be an unmeasured pair and
// stay skipped, which is the right outcome and not a row to write in advance.
//
// `Metadata.text_document_encoding` is deliberately not consulted. All six
// indexers set it to UTF8 — including the three whose columns are UTF-16 —
// because it describes the encoding of the source files on disk and scip.proto
// says so; reading it as a position encoding would convert every UTF-16 column
// as a byte offset and attribute compiler facts to the wrong bytes.
//
// Any other pair returns encodingUnspecified and its documents stay skipped:
// Section 9.3 forbids guessing one. Versions are compared with the profile
// checker's own comparator, so a leading "v" and a build or pre-release suffix
// (`0.0.0-SNAPSHOT`) do not defeat the match; a version that is not a dotted
// number is not a measured pair either and is skipped rather than raised.
func toolPositionEncoding(toolName, toolVersion string) int32 {
	var enc int32
	var measured string
	switch toolName {
	case "scip-go":
		enc, measured = encodingUTF8, "=0.2.7"
	case "scip-clang":
		enc, measured = encodingUTF8, "=0.4.0"
	case "scip-typescript":
		enc, measured = encodingUTF16, "=0.4.0"
	case "scip-python":
		enc, measured = encodingUTF16, "=0.6.6"
	case "scip-java":
		enc, measured = encodingUTF16, "=0.0.0"
	default:
		return encodingUnspecified
	}
	if ok, err := satisfies(toolVersion, measured); err != nil || !ok {
		return encodingUnspecified
	}
	return enc
}

// resolveEncoding is the encoding a document's ranges are converted in: the
// document's own declaration, else the measured encoding of the tool build
// that wrote the index. assumed says the second case applies, so the caller
// must prove the guess against the pinned bytes before it admits a fact.
func (im *importer) resolveEncoding(declared int32) (enc int32, assumed bool) {
	if columnEncoding(declared) != "" {
		return declared, false
	}
	enc = toolPositionEncoding(im.meta.toolName, im.meta.toolVersion)
	return enc, enc != encodingUnspecified
}

// maxEncodingProbes bounds the definition occurrences encodingHolds reads
// looking for one it can check. A document whose first maxEncodingProbes
// definitions all name something the source does not spell literally is
// skipped, exactly as one with no definition at all is: an unchecked guess is
// never admitted.
const maxEncodingProbes = 256

// encodingHolds proves an assumed position encoding against the document's
// pinned bytes, once, before any of its occurrences is admitted.
//
// A wrong encoding does not fail on its own. Reading a UTF-16 column as a byte
// offset lands inside a UTF-8 sequence only by luck; far more often it selects
// a valid, in-range, rune-aligned extent a few bytes off the identifier, which
// rangeOf accepts and which is then published at PrecisionCompiler against
// source that is not the symbol — the wrong-bytes class nothing downstream can
// detect. So the guess is checked the only way the index itself allows: the
// first definition occurrence whose symbol names an identifier the source
// spells literally (symbol.probeName) must select exactly that identifier.
//
// On a line with a non-ASCII rune before the token the readings disagree and a
// wrong guess is caught; on an ASCII-only line every reading converts to the
// same bytes, so there is nothing to catch and the check passes on the truth.
func (im *importer) encodingHolds(ctx context.Context, ds *docSource) (bool, error) {
	var checked, holds bool
	err := im.sc.each(ctx, `SELECT symbol, r0, r1, r2, r3 FROM occ WHERE doc = ? AND (roles & ?) <> 0 ORDER BY seq LIMIT ?`,
		[]any{ds.doc.idx, roleDefinition, maxEncodingProbes}, func(scan func(...any) error) error {
			var symbolText string
			var r [4]int32
			if err := scan(&symbolText, &r[0], &r[1], &r[2], &r[3]); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			if checked || symbolText == "" {
				return nil
			}
			sym, err := parseSymbol(symbolText)
			if err != nil {
				return err
			}
			name, ok := sym.probeName()
			if !ok {
				return nil
			}
			// This occurrence decides the document: a symbol that names an
			// identifier and does not land on it is the failure being looked
			// for, so a later occurrence is not tried instead.
			checked = true
			if r[0] < 0 || r[1] < 0 || r[2] < 0 || r[3] < 0 {
				return nil
			}
			rng, cerr := ds.cur.SourceRange(uint32(r[0])+1, uint32(r[1]), uint32(r[2])+1, uint32(r[3]), ds.enc)
			if cerr != nil || rng.End.Byte > uint64(len(ds.data)) || rng.Start.Byte > rng.End.Byte {
				return nil
			}
			holds = string(ds.data[rng.Start.Byte:rng.End.Byte]) == name
			return nil
		})
	return holds, err
}

// loadSource reads one document's exact pinned bytes through the snapshot
// view. The file is bounded by MaxSourceFileBytes, checked before the read.
func (im *importer) loadSource(ctx context.Context, d docRow) (*docSource, error) {
	rc, _, err := im.req.Content.Open(ctx, d.file.ID)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data := make([]byte, d.file.Size)
	if _, err := io.ReadFull(rc, data); err != nil {
		return nil, readError(err)
	}
	if _, err := rc.Read(make([]byte, 1)); err != io.EOF {
		return nil, &model.Error{Code: model.CodeSourceIntegrity, Message: "pinned file is longer than its manifest size", Details: map[string]string{"path": d.path}}
	}
	return &docSource{doc: d, data: data, cur: source.NewCursor(data), enc: columnEncoding(d.encoding)}, nil
}

// rangeOf converts a SCIP range against the document's bytes. A coordinate
// that does not land on these bytes is a wrong-source error under a verified
// binding and a skipped occurrence under an unverified one; it is never
// adjusted.
func (im *importer) rangeOf(ds *docSource, r [4]int32) (*model.SourceRange, error) {
	if r[0] < 0 || r[1] < 0 || r[2] < 0 || r[3] < 0 {
		return nil, im.badRange(ds, "occurrence range has a negative coordinate")
	}
	rng, err := ds.cur.SourceRange(uint32(r[0])+1, uint32(r[1]), uint32(r[2])+1, uint32(r[3]), ds.enc)
	if err != nil {
		return nil, im.badRange(ds, err.Error())
	}
	return &rng, nil
}

// badRange is the typed outcome of a coordinate that misses the pinned
// bytes: nil under an unverified binding (the caller skips and counts),
// otherwise CTX_PROVIDER_OUTPUT_INVALID that fails the unit.
func (im *importer) badRange(ds *docSource, msg string) error {
	if im.unverify {
		im.skippedOccurrences++
		return nil
	}
	return (&model.Error{Code: model.CodeProviderOutputInvalid, Message: "scip occurrence does not describe the pinned source bytes: " + msg}).
		WithDetail("path", ds.doc.path)
}

// defineSymbol resolves one definition occurrence and records its identity,
// its containment extent for pass 2 and its entry in the symbol map. When the
// document is changed it also publishes the node, its scoped alias and any
// may_refer_to alternatives; when the document is unchanged the storage writer
// keeps the rows the previous run wrote, so publishing them again would
// duplicate facts that were never invalidated.
func (im *importer) defineSymbol(ctx context.Context, ds *docSource, symbolText string, r [4]int32, enclosing *[4]int32, publish bool) error {
	sym, err := parseSymbol(symbolText)
	if err != nil {
		return err
	}
	rng, err := im.rangeOf(ds, r)
	if err != nil || rng == nil {
		return err
	}
	extent := rng
	if enclosing != nil {
		if extent, err = im.rangeOf(ds, *enclosing); err != nil {
			return err
		}
		if extent == nil || extent.Start.Byte > rng.Start.Byte || extent.End.Byte < rng.End.Byte {
			extent = rng
		}
	}
	key := symKey(ds.doc.idx, sym)
	var kind int32
	var name, sig, existing string
	var sigCut int64
	if _, err := im.sc.row(ctx, `SELECT kind, name, sig, node, sigcut FROM sym WHERE key = ?`, []any{key}, &kind, &name, &sig, &existing, &sigCut); err != nil {
		return err
	}
	if name == "" {
		name = sym.lastName
	}
	qualified := sym.descriptors
	if sym.local {
		qualified = ""
	}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: sym.scopeKey(ds.doc.path), NativeKey: sym.raw, Kind: sym.nodeKind(kind),
		Language: ds.doc.language, Name: name, QualifiedName: qualified, Signature: sig,
		FileID: ds.doc.file.ID, ContentHash: ds.doc.file.ContentHash, Range: rng}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return err
	}
	loc := location{file: ds.doc.file, rng: rng}
	if publish {
		if err := im.putNode(ctx, res, sym.raw, "definition", loc, false, sigCut); err != nil {
			return err
		}
		if err := im.sink.PutAliases(ctx, []model.NativeAlias{{ScopeKey: cand.ScopeKey, NativeKey: sym.raw, NodeID: res.Node.ID}}); err != nil {
			return err
		}
		im.records++
		for _, other := range res.Ambiguous {
			if err := im.addEdge(ctx, res.Node.ID, model.RelMayReferTo, other, loc, sym.raw, "may_refer_to"); err != nil {
				return err
			}
		}
	}
	if err := im.sc.exec(ctx, `INSERT OR IGNORE INTO defs(doc, start, end, node) VALUES(?, ?, ?, ?)`,
		ds.doc.idx, int64(extent.Start.Byte), int64(extent.End.Byte), string(res.Node.ID)); err != nil {
		return err
	}
	// The first definition of a symbol is the one references bind to; a
	// later one keeps its own located identity but does not take over.
	return im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig, node, def_doc, def_start, def_end) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET node = CASE WHEN sym.node = '' THEN excluded.node ELSE sym.node END,
		def_doc = CASE WHEN sym.node = '' THEN excluded.def_doc ELSE sym.def_doc END,
		def_start = CASE WHEN sym.node = '' THEN excluded.def_start ELSE sym.def_start END,
		def_end = CASE WHEN sym.node = '' THEN excluded.def_end ELSE sym.def_end END`,
		key, sym.raw, kind, name, sig, string(res.Node.ID), ds.doc.idx, int64(rng.Start.Byte), int64(rng.End.Byte))
}

// putNode publishes a resolved node with one evidence row at loc. Node
// metadata carries the binding when it is unverified and marks an entity
// that has no definition in this index as external.
//
// An external entity has no definition in this index, so it has no source
// location and its evidence carries none: Section 9.3 requires an absent range
// rather than a stand-in, and a referencing occurrence is a property of the
// reference edge, which keeps it. Pinning the node's evidence to whichever
// document happened to reference it first would also make an index-level fact
// belong to one path's rows, and a refresh that changed a different document
// would then publish a second evidence row for the same node on every pass
// until the bound of Section 11.1 was reached.
func (im *importer) putNode(ctx context.Context, res model.Resolution, nativeKey, detail string, loc location, external bool, sigCut int64) error {
	node := res.Node
	meta := map[string]any{}
	if im.unverify {
		meta["source_binding"] = string(model.SourceBindingUnverified)
	}
	if external {
		meta["scip_external"] = true
		loc = location{}
	}
	if sigCut > 0 {
		// Index-time field truncation, on the fact itself and under the same
		// attribute the structural provider publishes, so an answer built
		// from either says which stored value was cut and how long it was.
		// Never merged with a result page's transient truncation flag.
		meta["truncated_fields"] = map[string]int64{"signature": sigCut}
	}
	if len(meta) > 0 {
		raw, err := json.Marshal(meta)
		if err != nil {
			return internal("scip metadata: " + err.Error())
		}
		node.Metadata = raw
	}
	ev := im.evidence(node.ID, "", loc, nativeKey, detail)
	if err := im.sink.PutNodes(ctx, []model.NodeFact{{Node: node, CanonicalKey: res.CanonicalKey, Evidence: []model.Evidence{ev}}}); err != nil {
		return err
	}
	im.records++
	return nil
}

// evidence builds one evidence row for this unit and run with compiler
// precision, the SCIP precision class of Section 9.3.
func (im *importer) evidence(node model.NodeID, rel model.RelationID, loc location, nativeKey, detail string) model.Evidence {
	e := model.Evidence{UnitID: im.req.Unit.ID, ProviderID: im.req.Unit.ProviderID, ProviderVersion: im.req.Unit.ProviderVersion,
		OriginRunID: im.req.Run, NodeID: node, RelationID: rel, Precision: model.PrecisionCompiler,
		FileID: loc.file.ID, ContentHash: loc.file.ContentHash, Range: loc.rng, NativeKey: nativeKey, Detail: detail}
	e.ID = model.NewEvidenceID(e)
	return e
}

// addEdge records one occurrence of a canonical edge for grouped emission.
// The same edge at the same range is recorded once; a distinct range is a
// distinct occurrence (Section 11.4).
func (im *importer) addEdge(ctx context.Context, from model.NodeID, kind model.RelationKind, to model.NodeID, loc location, symbolText, detail string) error {
	if err := im.sc.charge(int64(len(from) + len(to) + len(symbolText) + 192)); err != nil {
		return err
	}
	rng := loc.rng
	return im.sc.exec(ctx, `INSERT OR IGNORE INTO edges(frm, kind, dst, file_id, content_hash, start, end, sl, sc, el, ec, symbol, detail) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(from), string(kind), string(to), string(loc.file.ID), loc.file.ContentHash, int64(rng.Start.Byte), int64(rng.End.Byte),
		rng.Start.Line, rng.Start.Column, rng.End.Line, rng.End.Column, symbolText, detail)
}

// passReferences is pass 2 over the spooled occurrences: every non-definition
// occurrence of a changed document becomes one occurrence of an edge from its
// innermost enclosing definition (or the document's file node) to the
// referenced symbol's node. An unchanged document is skipped whole: its edges,
// evidence and aliases are the rows the storage writer retains.
func (im *importer) passReferences(ctx context.Context) error {
	var after int64 = -1
	for {
		var page []docRow
		err := im.sc.each(ctx, `SELECT d.idx, d.path, d.lang, d.enc, d.file_id, d.content_hash, d.size FROM docs d
			JOIN docdelta x ON x.doc = d.idx AND x.publish = 1 WHERE d.idx > ? ORDER BY d.idx LIMIT 256`, []any{after},
			func(scan func(...any) error) error {
				var d docRow
				var id, hash string
				if err := scan(&d.idx, &d.path, &d.language, &d.encoding, &id, &hash, &d.file.Size); err != nil {
					return internal("scip scratch read: " + err.Error())
				}
				d.file.ID, d.file.ContentHash, d.file.Path = model.FileID(id), hash, d.path
				page = append(page, d)
				return nil
			})
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, d := range page {
			if err := im.referencesOf(ctx, d); err != nil {
				return err
			}
			after = d.idx
		}
	}
}

func (im *importer) referencesOf(ctx context.Context, d docRow) error {
	ds, err := im.loadSource(ctx, d)
	if err != nil {
		return err
	}
	return im.sc.each(ctx, `SELECT symbol, roles, r0, r1, r2, r3 FROM occ WHERE doc = ? AND (roles & ?) = 0 ORDER BY seq`,
		[]any{d.idx, roleDefinition}, func(scan func(...any) error) error {
			var symbolText string
			var roles int32
			var r [4]int32
			if err := scan(&symbolText, &roles, &r[0], &r[1], &r[2], &r[3]); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			if symbolText == "" {
				return nil
			}
			sym, err := parseSymbol(symbolText)
			if err != nil {
				return err
			}
			rng, err := im.rangeOf(ds, r)
			if err != nil || rng == nil {
				return err
			}
			loc := location{file: d.file, rng: rng}
			to, err := im.targetNode(ctx, symKey(d.idx, sym), sym, loc)
			if err != nil {
				return err
			}
			from, err := im.enclosingNode(ctx, ds, rng)
			if err != nil {
				return err
			}
			if err := im.putCallsiteAlias(ctx, d.path, rng, to); err != nil {
				return err
			}
			// The read and write occurrence roles are deliberately not
			// mapped: `reads` and `writes` are the dependence provider's
			// facts (Section 11.6), derived from the graph's assignment
			// operators, and two providers publishing one relation kind from
			// different precisions is the parallel implementation policy.md
			// forbids. Every non-definition, non-import occurrence is a
			// `references` edge here.
			kind, detail := model.RelReferences, "reference"
			if roles&roleImport != 0 {
				kind, detail = model.RelImports, "import"
			}
			return im.addEdge(ctx, from, kind, to, loc, sym.raw, detail)
		})
}

// putCallsiteAlias publishes the Section 11.3 call-site alias for one
// reference occurrence: the compiler-resolved symbol, keyed by the byte range
// the reference occupies in this file version, in the file's alias scope. The
// tree-sitter provider publishes the same key on the syntactic callee of every
// call site it finds, so the reconciler merges the two identities wherever the
// ranges are equal and the `calls` relation gains a precise target. See
// callsite.go for the key.
func (im *importer) putCallsiteAlias(ctx context.Context, p string, rng *model.SourceRange, to model.NodeID) error {
	scopeKey, nativeKey, ok := callsiteAlias(p, rng)
	if !ok {
		im.skippedAliases++
		return nil
	}
	if err := im.sink.PutAliases(ctx, []model.NativeAlias{{ScopeKey: scopeKey, NativeKey: nativeKey, NodeID: to}}); err != nil {
		return err
	}
	im.records++
	return nil
}

// targetNode returns the node a referenced symbol resolves to: the node its
// definition minted, or, for a symbol this index never defines, an entity
// minted once from the symbol alone (structural key over its package scope
// and qualified name) with loc as its evidence. It is recorded in the map so
// every later reference reuses it.
func (im *importer) targetNode(ctx context.Context, key string, sym symbol, loc location) (model.NodeID, error) {
	var kind int32
	var name, sig, node string
	var sigCut int64
	if _, err := im.sc.row(ctx, `SELECT kind, name, sig, node, sigcut FROM sym WHERE key = ?`, []any{key}, &kind, &name, &sig, &node, &sigCut); err != nil {
		return "", err
	}
	if node != "" {
		return model.NodeID(node), nil
	}
	if name == "" {
		name = sym.lastName
	}
	qualified := sym.descriptors
	if sym.local {
		qualified = ""
	}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: sym.scopeKey(loc.file.Path), NativeKey: sym.raw, Kind: sym.nodeKind(kind),
		Name: name, QualifiedName: qualified, Signature: sig}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return "", err
	}
	if err := im.putNode(ctx, res, sym.raw, "reference", loc, true, sigCut); err != nil {
		return "", err
	}
	for _, other := range res.Ambiguous {
		if err := im.addEdge(ctx, res.Node.ID, model.RelMayReferTo, other, loc, sym.raw, "may_refer_to"); err != nil {
			return "", err
		}
	}
	return res.Node.ID, im.sc.exec(ctx, `INSERT INTO sym(key, symbol, kind, name, sig, node) VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET node = excluded.node WHERE sym.node = ''`, key, sym.raw, kind, name, sig, string(res.Node.ID))
}

// enclosingNode is the innermost definition whose extent contains rng, or
// the document's own file node when the occurrence is at file level.
func (im *importer) enclosingNode(ctx context.Context, ds *docSource, rng *model.SourceRange) (model.NodeID, error) {
	var node string
	found, err := im.sc.row(ctx, `SELECT node FROM defs WHERE doc = ? AND start <= ? AND end >= ? ORDER BY (end - start), start, node LIMIT 1`,
		[]any{ds.doc.idx, int64(rng.Start.Byte), int64(rng.End.Byte)}, &node)
	if err != nil {
		return "", err
	}
	if found {
		return model.NodeID(node), nil
	}
	if _, err := im.sc.row(ctx, `SELECT file_node FROM docs WHERE idx = ?`, []any{ds.doc.idx}, &node); err != nil {
		return "", err
	}
	if node != "" {
		return model.NodeID(node), nil
	}
	// The file node a file-level occurrence points out of. The candidate is
	// the one the filesystem provider publishes for the same path — a
	// workspace-scoped `file:<path>` alias, no defining file and no range — so
	// resolving it against that declared dependency adopts the identity that
	// already exists instead of minting a second file node for one path. It
	// carries no Language because the language of a path is internal/lang,
	// which Task 7 owns and this worktree does not hold; deriving it here
	// would be a parallel implementation that drifts. Language is a
	// publishing unit's own attribute, so omitting it changes no identity.
	end, err := ds.cur.PositionAt(uint64(len(ds.data)))
	if err != nil {
		return "", err
	}
	whole := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1, Column: 0}, End: end}
	cand := model.NodeCandidate{ProviderID: ID, ScopeKey: provider.ScopeWorkspace, NativeKey: fileNativeKey + ds.doc.path,
		Kind: model.NodeFile, Name: path.Base(ds.doc.path), QualifiedName: ds.doc.path}
	res, err := im.req.Resolver.Resolve(ctx, cand)
	if err != nil {
		return "", err
	}
	if err := im.putNode(ctx, res, cand.NativeKey, "document", location{file: ds.doc.file, rng: whole}, false, 0); err != nil {
		return "", err
	}
	return res.Node.ID, im.sc.exec(ctx, `UPDATE docs SET file_node = ? WHERE idx = ?`, string(res.Node.ID), ds.doc.idx)
}

// passRelationships turns SymbolInformation relationships of defined symbols
// into edges: is_implementation → implements, is_reference and
// is_type_definition → references; is_definition names the symbol's own
// definition and is not an edge. Evidence is the source symbol's definition.
func (im *importer) passRelationships(ctx context.Context) error {
	type relRow struct {
		key, target, node string
		flags             int32
		docIdx            int64
		start, end        int64
	}
	var rows []relRow
	var afterDoc int64 = -1
	var after string
	// Rows are ordered by the document that holds the source symbol's
	// definition, so consecutive rows share a document and its pinned bytes
	// are read once per pass rather than once per relationship row. The
	// keyset cursor is the compound (def_doc, key, target) the order is over;
	// a cursor over the key alone would skip or repeat rows under it.
	var ds *docSource
	for {
		rows = rows[:0]
		// A relationship's evidence is the source symbol's own definition, so
		// the row belongs to the document that defines it; a relationship of a
		// symbol defined in an unchanged document is one of the rows the
		// storage writer retains and is not republished here.
		err := im.sc.each(ctx, `SELECT r.key, r.target, r.flags, s.node, s.def_doc, s.def_start, s.def_end FROM rel r JOIN sym s ON s.key = r.key
			JOIN docdelta x ON x.doc = s.def_doc AND x.publish = 1
			WHERE s.node <> '' AND s.def_doc >= 0 AND (s.def_doc > ? OR (s.def_doc = ? AND r.key || char(0) || r.target > ?))
			ORDER BY s.def_doc, r.key, r.target LIMIT 256`, []any{afterDoc, afterDoc, after},
			func(scan func(...any) error) error {
				var r relRow
				if err := scan(&r.key, &r.target, &r.flags, &r.node, &r.docIdx, &r.start, &r.end); err != nil {
					return internal("scip scratch read: " + err.Error())
				}
				rows = append(rows, r)
				return nil
			})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, r := range rows {
			afterDoc, after = r.docIdx, r.key+"\x00"+r.target
			if ds == nil || ds.doc.idx != r.docIdx {
				d, ok, err := im.docByIndex(ctx, r.docIdx)
				if err != nil {
					return err
				}
				if !ok {
					return internal("scip scratch names a document that was never recorded")
				}
				if ds, err = im.loadSource(ctx, d); err != nil {
					return err
				}
			}
			d := ds.doc
			start, err := ds.cur.PositionAt(uint64(r.start))
			if err != nil {
				return err
			}
			end, err := ds.cur.PositionAt(uint64(r.end))
			if err != nil {
				return err
			}
			loc := location{file: d.file, rng: &model.SourceRange{Start: start, End: end}}
			target, err := parseSymbol(r.target)
			if err != nil {
				return err
			}
			to, err := im.targetNode(ctx, symKey(r.docIdx, target), target, loc)
			if err != nil {
				return err
			}
			if r.flags&relImplementation != 0 {
				if err := im.addEdge(ctx, model.NodeID(r.node), model.RelImplements, to, loc, target.raw, "implementation"); err != nil {
					return err
				}
			}
			if r.flags&(relReference|relTypeDefinition) != 0 {
				if err := im.addEdge(ctx, model.NodeID(r.node), model.RelReferences, to, loc, target.raw, "relationship"); err != nil {
					return err
				}
			}
		}
	}
}

func (im *importer) docByIndex(ctx context.Context, idx int64) (docRow, bool, error) {
	var d docRow
	var id, hash string
	found, err := im.sc.row(ctx, `SELECT idx, path, lang, enc, file_id, content_hash, size FROM docs WHERE idx = ?`, []any{idx},
		&d.idx, &d.path, &d.language, &d.encoding, &id, &hash, &d.file.Size)
	d.file.ID, d.file.ContentHash, d.file.Path = model.FileID(id), hash, d.path
	return d, found, err
}

// emitEdges publishes the grouped edges: one RelationFact per canonical
// relation with one evidence row per distinct occurrence range, in a fixed
// order. Occurrences beyond MaxEvidencePerFact on one edge are counted and
// reported through the partial capability state, never silently dropped.
func (im *importer) emitEdges(ctx context.Context) error {
	var current *model.RelationFact
	var overflow int64
	flush := func() error {
		if current == nil {
			return nil
		}
		fact := *current
		current = nil
		if err := im.sink.PutRelations(ctx, []model.RelationFact{fact}); err != nil {
			return err
		}
		im.records++
		return nil
	}
	err := im.sc.each(ctx, `SELECT frm, kind, dst, symbol, detail, start, end, sl, sc, el, ec, file_id, content_hash
		FROM edges ORDER BY frm, kind, dst, file_id, start, end, detail`, nil,
		func(scan func(...any) error) error {
			var frm, kind, dst, symbolText, detail, fileID, hash string
			var rng model.SourceRange
			if err := scan(&frm, &kind, &dst, &symbolText, &detail, &rng.Start.Byte, &rng.End.Byte, &rng.Start.Line, &rng.Start.Column, &rng.End.Line, &rng.End.Column, &fileID, &hash); err != nil {
				return internal("scip scratch read: " + err.Error())
			}
			rel := model.Relation{From: model.NodeID(frm), Kind: model.RelationKind(kind), To: model.NodeID(dst)}
			rel.ID = model.NewRelationID(im.req.Binding.RepositoryID, rel.From, rel.Kind, rel.To)
			if current != nil && current.Relation.ID != rel.ID {
				if err := flush(); err != nil {
					return err
				}
			}
			if current == nil {
				current = &model.RelationFact{Relation: rel}
			}
			loc := location{file: model.FileVersion{ID: model.FileID(fileID), ContentHash: hash}, rng: &rng}
			if len(current.Evidence) >= im.p.evidenceClip {
				overflow++
				return nil
			}
			current.Evidence = append(current.Evidence, im.evidence("", rel.ID, loc, symbolText, detail))
			return nil
		})
	if err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if overflow > 0 {
		im.truncatedEdges = overflow
		im.degrade(model.CodeResourceLimit)
	}
	return nil
}

// result reports the run: succeeded with every capability fresh, or with
// each capability partial under the first degradation reason (an unverified
// binding, an unprocessable document, or truncated occurrence evidence).
func (im *importer) result() model.ProviderResult {
	if im.drops.any() {
		im.degrade(model.CodeResourceLimit)
	}
	if im.sc != nil {
		im.seen.note(limitSpoolBytes, im.sc.bytes)
	}
	crossed := im.seen.exceeded(im.p.limits)
	if crossed != "" {
		im.degrade(model.CodeResourceLimit)
	}
	state := model.CapabilityFresh
	if im.partialCode != "" {
		state = model.CapabilityPartial
	}
	// The run state stays succeeded even when a capability is partial:
	// provider.RunUnit admits only a succeeded result ("only a succeeded unit
	// is admitted") and fails the unit otherwise, so reporting RunPartial
	// here would make every unverified import unsealable — the opposite of
	// Section 11.4's "importable for discovery". Degradation is reported
	// through the capability states below, which is the channel that exists.
	r := model.ProviderResult{RunID: im.req.Run, State: model.RunSucceeded, RecordsEmitted: im.records, BytesProcessed: im.indexBytes}
	for _, c := range capabilities {
		cs := model.CapabilityState{ProviderID: ID, Capability: c, Scope: im.req.Unit.ScopeKey, State: state, DiagnosticCode: im.partialCode}
		// What the wire decoder discarded for exceeding a field bound, named
		// per bound and split by what the bound cost: a whole record whose
		// identity was unreadable, or one decorative field of a record that is
		// still published. Silence here was the class-G defect (plan row 26).
		if v := summarizeDrops(im.drops.records); v != "" {
			cs = cs.WithDetail(detailDroppedRecords, v)
		}
		if v := summarizeDrops(im.drops.fields); v != "" {
			cs = cs.WithDetail(detailDroppedFields, v)
		}
		if im.drops.any() {
			cs = cs.WithDetail(detailDropReason, dropReason)
		}
		// Every providers.scip.* bound the operator set that this run crossed,
		// as `key=seen/bound`. Nothing was refused and, apart from
		// max_source_file_bytes and max_materialize_bytes, nothing was left
		// out: the row says the figure was passed, which is what a threshold
		// the product does not enforce on the user's behalf can honestly say.
		if crossed != "" {
			cs = cs.WithDetail(detailLimitsExceeded, crossed)
		}
		r.Capabilities = append(r.Capabilities, cs)
	}
	return r
}

// report is the full account of the run: the provider result plus the
// per-document delta and every document and occurrence the import did not
// admit. RecordsEmitted counts only what reached the sink, so an unchanged
// document contributes nothing to it.
func (im *importer) report() Report {
	return Report{
		Result: im.result(), Delta: im.delta, Manifest: im.manifest,
		OutsideRoot: im.outsideRoot, DuplicatePaths: im.duplicatePaths, Skipped: im.skippedDocs,
		SkippedOccurrences: im.skippedOccurrences, SkippedCallsiteAliases: im.skippedAliases,
		TruncatedEdgeOccurrences: im.truncatedEdges, AssumedPositionEncoding: im.assumedEncoding,
	}
}
