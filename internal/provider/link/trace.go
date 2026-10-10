package link

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// Verifying traces (ADR-0014 decision 3). The resolution of a Go, Java, Rust,
// C or C++ member reads its own table and, beyond it, only three things: the
// hierarchy's two indexes (each read one key at a time through
// index.lookup), the address-taken functions of a signature (the
// cross-product) and the unit's manifest fields (a Go module path; a Rust
// crate's root is an index entry). Each read is recorded, by the member that
// made it, under a stable key: the key with every member ordinal and table
// index in it replaced by the file and declaration identity it denotes, so
// the key means the same thing in every build. A key is recorded whether or
// not the read found anything, since a lookup that found nothing changes
// when something is declared under it.
//
// The state keeps, for every key the indexes hold, the hash of what they hold
// under it (a group), and the reverse index of the reads: each read key with
// the members that read it, sorted by key. A refresh diffs the groups of the
// rebuilt indexes against the predecessor's, joins the changed keys with the
// reverse index, and re-runs exactly the members whose reads changed.

// The hash domains of a trace key and of a group.
const (
	traceKeyDomain = "link.trace_key"
	groupDomain    = "link.trace_group"
)

// The scopes of a trace key (TraceKey.Scope).
const (
	scopeDecls     = "decls"     // a key of the decls index
	scopeLinks     = "links"     // a key of the links index
	scopeSignature = "signature" // the address-taken functions of a signature
	scopeManifest  = "manifest"  // a manifest field (manifestFields)
)

// hash is the stored form of a trace key.
func (k TraceKey) hash() string {
	return model.H(traceKeyDomain, strconv.Itoa(int(k.Kind)), k.Scope, k.Name)
}

// signatureKey is the read of the address-taken functions of sig.
func signatureKey(sig string) TraceKey {
	return TraceKey{Kind: TraceScopeLookup, Scope: scopeSignature, Name: sig}
}

// manifestKey is the read of the manifest field field.
func manifestKey(field string) TraceKey {
	return TraceKey{Kind: TraceManifestField, Scope: scopeManifest, Name: field}
}

// read is one recorded read: the stable key's hash and the member that made
// it. A refresh reads them sorted by key; their fold keeps one per pair.
type read struct {
	Key  string
	File model.FileID
}

func compareRead(a, b read) int {
	return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.File, b.File))
}

func encodeRead(r read) ([]byte, error) {
	return appendString(appendString(nil, r.Key), string(r.File)), nil
}

func decodeRead(raw []byte) (read, error) {
	b := byteReader{b: raw}
	r := read{Key: b.str(), File: model.FileID(b.str())}
	if b.err != nil || len(b.b) != 0 {
		return read{}, errors.New("a recorded read is malformed")
	}
	return r, nil
}

func newReadSort(dir string, order func(a, b read) int) (*pagination.ExternalSort[read], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeRead, decodeRead, order)
	if err != nil {
		return nil, err
	}
	return s.WithFold(func(a, _ read) (read, error) { return a, nil }), nil
}

// group is what an index holds under one stable key, hashed: Key is the
// key's hash, Hash the hash of its entries in index order.
type group struct{ Key, Hash string }

func compareGroup(a, b group) int { return cmp.Compare(a.Key, b.Key) }

func encodeGroup(g group) ([]byte, error) {
	return appendString(appendString(nil, g.Key), g.Hash), nil
}

func decodeGroup(raw []byte) (group, error) {
	b := byteReader{b: raw}
	g := group{Key: b.str(), Hash: b.str()}
	if b.err != nil || len(b.b) != 0 {
		return group{}, errors.New("an index group is malformed")
	}
	return g, nil
}

func newGroupSort(dir string) (*pagination.ExternalSort[group], error) {
	return pagination.NewExternalSort(dir, 0, encodeGroup, decodeGroup, compareGroup)
}

// idTable is the unit's ordinals translated to identities: each member's
// FileID, and each declaration's node, by member ordinal and table index. It
// is three scratch files, so one translation is a few positioned reads and
// nothing of it is resident: members holds, per member, where its FileID is
// in strs, its first declaration's position in decls and its declaration
// count; decls holds, per declaration, where its node is in strs.
type idTable struct {
	files        [3]*os.File
	leases       [3]*scratch.Lease
	mw, dw, sw   *bufio.Writer
	count, decls int64
	strs         int64
}

// The widths of a member's and a declaration's record: a string's position
// and length, and for a member its first declaration and count.
const (
	idMember = 8 + 4 + 8 + 4
	idDecl   = 8 + 4
)

func newIDTable(dir string) (*idTable, error) {
	ids := &idTable{}
	for i := range ids.files {
		l, f, err := scratch.For(dir).TakeFile(scratch.SortRun)
		if err != nil {
			ids.close()
			return nil, err
		}
		ids.leases[i], ids.files[i] = l, f
	}
	ids.mw, ids.dw, ids.sw = bufio.NewWriter(ids.files[0]), bufio.NewWriter(ids.files[1]), bufio.NewWriter(ids.files[2])
	return ids, nil
}

// str appends s to strs and answers its position and length.
func (ids *idTable) str(s string) ([]byte, error) {
	rec := binary.BigEndian.AppendUint64(nil, uint64(ids.strs))
	rec = binary.BigEndian.AppendUint32(rec, uint32(len(s)))
	_, err := ids.sw.WriteString(s)
	ids.strs += int64(len(s))
	return rec, err
}

// add records the next member, file, with t's declarations; t is nil for a
// member without a table.
func (ids *idTable) add(file model.FileID, t *Table) error {
	rec, err := ids.str(string(file))
	if err != nil {
		return err
	}
	rec = binary.BigEndian.AppendUint64(rec, uint64(ids.decls))
	n := 0
	if t != nil {
		n = len(t.Decls)
		for i := range t.Decls {
			d, err := ids.str(string(t.Decls[i].Node))
			if err != nil {
				return err
			}
			if _, err := ids.dw.Write(d); err != nil {
				return err
			}
		}
	}
	rec = binary.BigEndian.AppendUint32(rec, uint32(n))
	ids.decls += int64(n)
	ids.count++
	_, err = ids.mw.Write(rec)
	return err
}

// finish makes what add wrote readable.
func (ids *idTable) finish() error {
	for _, w := range []*bufio.Writer{ids.mw, ids.dw, ids.sw} {
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (ids *idTable) readAt(part int, buf []byte, off int64) error {
	if _, err := ids.files[part].ReadAt(buf, off); err != nil {
		return internalErr("link identity table: " + err.Error())
	}
	return nil
}

// at reads the string a position-and-length record names.
func (ids *idTable) at(rec []byte) (string, error) {
	off, n := int64(binary.BigEndian.Uint64(rec)), int64(binary.BigEndian.Uint32(rec[8:]))
	if off+n > ids.strs {
		return "", internalErr("link identity table: a string is past its end")
	}
	buf := make([]byte, n)
	if n == 0 {
		return "", nil
	}
	err := ids.readAt(2, buf, off)
	return string(buf), err
}

func (ids *idTable) member(file int32) (model.FileID, int64, int32, error) {
	if file < 0 || int64(file) >= ids.count {
		return "", 0, 0, internalErr("member " + strconv.Itoa(int(file)) + " is past the unit's members")
	}
	var rec [idMember]byte
	if err := ids.readAt(0, rec[:], int64(file)*idMember); err != nil {
		return "", 0, 0, err
	}
	id, err := ids.at(rec[:12])
	return model.FileID(id), int64(binary.BigEndian.Uint64(rec[12:])), int32(binary.BigEndian.Uint32(rec[20:])), err
}

// file is the FileID of the member at ordinal file.
func (ids *idTable) file(file int32) (model.FileID, error) {
	id, _, _, err := ids.member(file)
	return id, err
}

// node is the file and node of the declaration at r.
func (ids *idTable) node(r Ref) (target, error) {
	id, first, n, err := ids.member(r.File)
	if err != nil {
		return target{}, err
	}
	if r.Index < 0 || r.Index >= n {
		return target{}, internalErr("declaration " + strconv.Itoa(int(r.Index)) + " is past its member's table")
	}
	var rec [idDecl]byte
	if err := ids.readAt(1, rec[:], (first+int64(r.Index))*idDecl); err != nil {
		return target{}, err
	}
	node, err := ids.at(rec[:])
	return target{File: id, Node: model.NodeID(node)}, err
}

// scope is the stable spelling of a Ref that names a declaration, the module
// (-1) or a Java anonymous class body (anonymousType) of a member.
func (ids *idTable) scope(r Ref) (string, error) {
	if r.Index >= 0 {
		t, err := ids.node(r)
		return string(t.File) + "\x00" + string(t.Node), err
	}
	id, err := ids.file(r.File)
	if r.Index == -1 {
		return string(id) + "\x00", err
	}
	return string(id) + "\x00#" + strconv.Itoa(int(-1-r.Index)), err
}

func (ids *idTable) close() {
	if ids == nil {
		return
	}
	for i, l := range ids.leases {
		if l != nil {
			l.Release()
			ids.leases[i] = nil
		}
	}
}

// refPrefixes are the index keys that carry a refKey right after their
// prefix, and whether that ref names only a file (its index is zero and
// meaningless) or a declaration.
var refPrefixes = []struct {
	prefix string
	file   bool
}{
	{keyFile, true}, {keyImport, true}, {keyMethod, false}, {keySub, false}, {keySuper, false}, {keyGoField, false},
}

// traceKey is the read of key in the index scope names (scopeDecls,
// scopeLinks), spelled stably.
func (ids *idTable) traceKey(scope, key string) (TraceKey, error) {
	name, err := ids.stableKey(key)
	return TraceKey{Kind: TraceScopeLookup, Scope: scope, Name: name}, err
}

// stableKey is key with the ref it carries, if any, spelled as the file or
// declaration it denotes.
func (ids *idTable) stableKey(key string) (string, error) {
	for _, p := range refPrefixes {
		rest, ok := strings.CutPrefix(key, p.prefix)
		if !ok || len(rest) < 8 {
			continue
		}
		r := refOf(rest[:8])
		var s string
		var err error
		if p.file {
			var id model.FileID
			id, err = ids.file(r.File)
			s = string(id)
		} else {
			s, err = ids.scope(r)
		}
		return p.prefix + s + "\x00" + rest[8:], err
	}
	return key, nil
}

// entryRef is the stable spelling of e's Ref under key: a declaration, a
// scope (an import's), a file (a path's), or nothing for the keys whose
// entries name no declaration (a file's context, a Go directory, a Rust
// module or crate, a re-export, a module exported as a namespace).
func (ids *idTable) entryRef(key string, e entry) (string, error) {
	switch {
	case strings.HasPrefix(key, keyFile), strings.HasPrefix(key, keyGoDir), strings.HasPrefix(key, keyRustModule),
		strings.HasPrefix(key, keyRustCrate), strings.HasPrefix(key, keyReexport),
		strings.HasPrefix(key, keyExport) && e.Aux == exportModule:
		return "", nil
	case strings.HasPrefix(key, keyPath):
		id, err := ids.file(e.Ref.File)
		return string(id), err
	}
	return ids.scope(e.Ref)
}

// groups adds to out every key ix, the index scope names, holds, with the hash of its
// entries in index order, each entry spelled stably. A key's entries are in
// the same order in every build that holds them: ordinals ascend with
// FileIDs and a table's indices with its declarations.
func (ids *idTable) groups(ix *index, scope string, out *pagination.ExternalSort[group]) error {
	var key string
	var parts []string
	flush := func() error {
		if parts == nil {
			return nil
		}
		k, err := ids.traceKey(scope, key)
		if err != nil {
			return err
		}
		err = out.Add(group{Key: k.hash(), Hash: model.H(groupDomain, parts...)})
		parts = nil
		return err
	}
	err := ix.each(func(e entry) error {
		if e.Key != key {
			if err := flush(); err != nil {
				return err
			}
			key = e.Key
		}
		ref, err := ids.entryRef(e.Key, e)
		if err != nil {
			return err
		}
		e.Key, e.Ref = "", Ref{}
		raw, _ := encodeEntry(e)
		parts = append(parts, ref, string(raw))
		return nil
	})
	if err == nil {
		err = flush()
	}
	return err
}

// The hierarchy as the state keeps it, for a later build whose layout is the
// same (refresh.layout): its module path and root, its two indexes and its
// identity table, each written as it is on disk.

// writeTo writes h to w.
func (h *hierarchy) writeTo(w *bufio.Writer) error {
	if _, err := w.Write(appendString(appendString(nil, h.goModule), h.goRoot)); err != nil {
		return err
	}
	if err := h.decls.writeTo(w); err != nil {
		return err
	}
	if err := h.links.writeTo(w); err != nil {
		return err
	}
	return h.ids.writeTo(w)
}

// readHierarchy reads a hierarchy writeTo wrote into files of dir's arena.
func readHierarchy(dir string, s stateReader) (*hierarchy, error) {
	h := &hierarchy{}
	ok := false
	defer func() {
		if !ok {
			_ = h.close()
		}
	}()
	var err error
	if h.goModule, err = s.str("its hierarchy's module path"); err != nil {
		return nil, err
	}
	if h.goRoot, err = s.str("its hierarchy's module root"); err != nil {
		return nil, err
	}
	if h.decls, err = readIndex(dir, s); err != nil {
		return nil, err
	}
	if h.links, err = readIndex(dir, s); err != nil {
		return nil, err
	}
	if h.ids, err = readIDTable(dir, s); err != nil {
		return nil, err
	}
	ok = true
	return h, nil
}

// writeTo writes the index's length, then its records as they are on disk.
func (ix *index) writeTo(w *bufio.Writer) error {
	if _, err := w.Write(binary.AppendUvarint(nil, uint64(ix.size))); err != nil {
		return err
	}
	_, err := io.Copy(w, io.NewSectionReader(ix.f, 0, ix.size))
	return err
}

// readIndex reads an index writeTo wrote into a file of dir's arena, locating
// its blocks as writeIndex does.
func readIndex(dir string, s stateReader) (*index, error) {
	size, err := s.uvarint("a hierarchy index")
	if err != nil {
		return nil, err
	}
	lease, f, err := scratch.For(dir).TakeFile(scratch.SortRun)
	if err != nil {
		return nil, err
	}
	ix := &index{lease: lease, f: f}
	w := bufio.NewWriter(f)
	block := int64(-indexBlockBytes)
	var buf []byte
	for uint64(ix.size) < size {
		n, err := binary.ReadUvarint(s.br)
		if err != nil || n > size {
			ix.close()
			return nil, stateCorrupt("a hierarchy index is short")
		}
		buf = append(buf[:0], make([]byte, n)...)
		if _, err := io.ReadFull(s.br, buf); err != nil {
			ix.close()
			return nil, stateCorrupt("a hierarchy index is short")
		}
		e, err := decodeEntry(buf)
		if err != nil {
			ix.close()
			return nil, stateCorrupt("a hierarchy index entry is malformed")
		}
		if ix.size-block >= indexBlockBytes {
			block = ix.size
			ix.first = append(ix.first, e.Key)
			ix.off = append(ix.off, ix.size)
		}
		head := binary.AppendUvarint(nil, n)
		if _, err := w.Write(head); err == nil {
			_, err = w.Write(buf)
		}
		if err != nil {
			ix.close()
			return nil, internalErr("hierarchy index: " + err.Error())
		}
		ix.size += int64(len(head)) + int64(n)
	}
	if uint64(ix.size) != size {
		ix.close()
		return nil, stateCorrupt("a hierarchy index overruns its length")
	}
	if err := w.Flush(); err != nil {
		ix.close()
		return nil, internalErr("hierarchy index: " + err.Error())
	}
	return ix, nil
}

// writeTo writes the table's member, declaration and string byte counts,
// then its three files.
func (ids *idTable) writeTo(w *bufio.Writer) error {
	out := binary.AppendUvarint(binary.AppendUvarint(nil, uint64(ids.count)), uint64(ids.decls))
	if _, err := w.Write(binary.AppendUvarint(out, uint64(ids.strs))); err != nil {
		return err
	}
	for i, n := range ids.sizes() {
		if _, err := io.Copy(w, io.NewSectionReader(ids.files[i], 0, n)); err != nil {
			return err
		}
	}
	return nil
}

// sizes is the byte length of each of the three files.
func (ids *idTable) sizes() [3]int64 {
	return [3]int64{ids.count * idMember, ids.decls * idDecl, ids.strs}
}

// readIDCounts reads the counts writeTo wrote.
func readIDCounts(s stateReader) (count, decls, strs int64, err error) {
	var v [3]uint64
	for i := range v {
		if v[i], err = s.uvarint("its identity table"); err != nil {
			return 0, 0, 0, err
		}
	}
	if v[0] > 1<<31 || v[1] > 1<<58 || v[2] > 1<<62 {
		return 0, 0, 0, stateCorrupt("its identity table's counts are malformed")
	}
	return int64(v[0]), int64(v[1]), int64(v[2]), nil
}

// readIDTable reads a table writeTo wrote into files of dir's arena.
func readIDTable(dir string, s stateReader) (*idTable, error) {
	count, decls, strs, err := readIDCounts(s)
	if err != nil {
		return nil, err
	}
	ids, err := newIDTable(dir)
	if err != nil {
		return nil, err
	}
	ids.count, ids.decls, ids.strs = count, decls, strs
	for i, w := range []*bufio.Writer{ids.mw, ids.dw, ids.sw} {
		n := ids.sizes()[i]
		if m, err := io.CopyN(w, s.br, n); err != nil || m != n {
			ids.close()
			return nil, stateCorrupt("its identity table is short")
		}
	}
	if err := ids.finish(); err != nil {
		ids.close()
		return nil, internalErr("link identity table: " + err.Error())
	}
	return ids, nil
}

// skipHierarchy reads a hierarchy writeTo wrote through, checking its form,
// and keeps nothing of it.
func skipHierarchy(s stateReader) error {
	for _, what := range []string{"its hierarchy's module path", "its hierarchy's module root"} {
		if _, err := s.str(what); err != nil {
			return err
		}
	}
	for range 2 {
		size, err := s.uvarint("a hierarchy index")
		if err != nil {
			return err
		}
		var read uint64
		for read < size {
			raw, err := s.str("a hierarchy index entry")
			if err != nil {
				return err
			}
			if _, err := decodeEntry([]byte(raw)); err != nil {
				return stateCorrupt("a hierarchy index entry is malformed")
			}
			read += uint64(len(binary.AppendUvarint(nil, uint64(len(raw))))) + uint64(len(raw))
		}
		if read != size {
			return stateCorrupt("a hierarchy index overruns its length")
		}
	}
	count, decls, strs, err := readIDCounts(s)
	if err != nil {
		return err
	}
	n := count*idMember + decls*idDecl + strs
	if m, err := io.CopyN(io.Discard, s.br, n); err != nil || m != n {
		return stateCorrupt("its identity table is short")
	}
	return nil
}
