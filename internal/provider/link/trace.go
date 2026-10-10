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

// The hash domains of a stable key and of a group.
const (
	traceKeyDomain = "link.trace_key"
	groupDomain    = "link.trace_group"
)

// The stable spellings of what a trace key names, ahead of the key itself.
const (
	traceDecls    = "d\x00" // a key of the decls index
	traceLinks    = "l\x00" // a key of the links index
	traceSig      = "s\x00" // the address-taken functions of a signature
	traceManifest = "m\x00" // a manifest field (manifestFields)
)

// keyHash is the stored form of a stable key.
func keyHash(stable string) string { return model.H(traceKeyDomain, stable) }

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
// is two scratch files of fixed-width records, so one translation is one
// positioned read and nothing of it is resident: members holds, per member,
// its FileID and the position of its first declaration's node in nodes, and
// its declaration count.
type idTable struct {
	members, nodes       *os.File
	memberLease, nodeLse *scratch.Lease
	mw, nw               *bufio.Writer
	count, decls         int64
}

// idRecord is the width of one member's record: its FileID, its first
// declaration's position and its declaration count.
const idRecord = model.IDHexLen + 8 + 4

func newIDTable(dir string) (*idTable, error) {
	ml, mf, err := scratch.For(dir).TakeFile(scratch.SortRun)
	if err != nil {
		return nil, err
	}
	nl, nf, err := scratch.For(dir).TakeFile(scratch.SortRun)
	if err != nil {
		ml.Release()
		return nil, err
	}
	return &idTable{members: mf, nodes: nf, memberLease: ml, nodeLse: nl, mw: bufio.NewWriter(mf), nw: bufio.NewWriter(nf)}, nil
}

// add records the next member, fv's file, with t's declarations; t is nil
// for a member without a table.
func (ids *idTable) add(file model.FileID, t *Table) error {
	if !model.ValidHexID(string(file)) {
		return internalErr("a member's file identity is not a digest: " + strconv.Quote(string(file)))
	}
	rec := append([]byte(file), make([]byte, 12)...)
	binary.BigEndian.PutUint64(rec[model.IDHexLen:], uint64(ids.decls))
	n := 0
	if t != nil {
		n = len(t.Decls)
		for i := range t.Decls {
			if !model.ValidHexID(string(t.Decls[i].Node)) {
				return internalErr("a declaration of " + t.Path + " has a node identity that is not a digest")
			}
			if _, err := ids.nw.WriteString(string(t.Decls[i].Node)); err != nil {
				return err
			}
		}
	}
	binary.BigEndian.PutUint32(rec[model.IDHexLen+8:], uint32(n))
	ids.decls += int64(n)
	ids.count++
	_, err := ids.mw.Write(rec)
	return err
}

// finish makes what add wrote readable.
func (ids *idTable) finish() error {
	if err := ids.mw.Flush(); err != nil {
		return err
	}
	return ids.nw.Flush()
}

func (ids *idTable) member(file int32) (model.FileID, int64, int32, error) {
	if file < 0 || int64(file) >= ids.count {
		return "", 0, 0, internalErr("member " + strconv.Itoa(int(file)) + " is past the unit's members")
	}
	var rec [idRecord]byte
	if _, err := ids.members.ReadAt(rec[:], int64(file)*idRecord); err != nil {
		return "", 0, 0, internalErr("link identity table: " + err.Error())
	}
	return model.FileID(rec[:model.IDHexLen]), int64(binary.BigEndian.Uint64(rec[model.IDHexLen:])),
		int32(binary.BigEndian.Uint32(rec[model.IDHexLen+8:])), nil
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
	buf := make([]byte, model.IDHexLen)
	if _, err := ids.nodes.ReadAt(buf, (first+int64(r.Index))*model.IDHexLen); err != nil {
		return target{}, internalErr("link identity table: " + err.Error())
	}
	return target{File: id, Node: model.NodeID(buf)}, nil
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
	if ids == nil || ids.memberLease == nil {
		return
	}
	ids.memberLease.Release()
	ids.nodeLse.Release()
	ids.memberLease, ids.nodeLse = nil, nil
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

// stableKey is the stable spelling of key, a key of the index the spelling
// prefix names (traceDecls, traceLinks).
func (ids *idTable) stableKey(prefix, key string) (string, error) {
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
		return prefix + p.prefix + s + "\x00" + rest[8:], err
	}
	return prefix + key, nil
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

// groups adds to out every key ix holds under prefix, with the hash of its
// entries in index order, each entry spelled stably. A key's entries are in
// the same order in every build that holds them: ordinals ascend with
// FileIDs and a table's indices with its declarations.
func (ids *idTable) groups(ix *index, prefix string, out *pagination.ExternalSort[group]) error {
	var key string
	var parts []string
	flush := func() error {
		if parts == nil {
			return nil
		}
		stable, err := ids.stableKey(prefix, key)
		if err != nil {
			return err
		}
		err = out.Add(group{Key: keyHash(stable), Hash: model.H(groupDomain, parts...)})
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

// writeTo writes the table's member and declaration counts, then its two
// files.
func (ids *idTable) writeTo(w *bufio.Writer) error {
	out := binary.AppendUvarint(binary.AppendUvarint(nil, uint64(ids.count)), uint64(ids.decls))
	if _, err := w.Write(out); err != nil {
		return err
	}
	if _, err := io.Copy(w, io.NewSectionReader(ids.members, 0, ids.count*idRecord)); err != nil {
		return err
	}
	_, err := io.Copy(w, io.NewSectionReader(ids.nodes, 0, ids.decls*model.IDHexLen))
	return err
}

// readIDTable reads a table writeTo wrote into files of dir's arena.
func readIDTable(dir string, s stateReader) (*idTable, error) {
	count, err := s.uvarint("its identity table")
	if err != nil {
		return nil, err
	}
	decls, err := s.uvarint("its identity table")
	if err != nil {
		return nil, err
	}
	if count > 1<<31 || decls > 1<<62/model.IDHexLen {
		return nil, stateCorrupt("its identity table's counts are malformed")
	}
	ids, err := newIDTable(dir)
	if err != nil {
		return nil, err
	}
	ids.count, ids.decls = int64(count), int64(decls)
	for _, part := range []struct {
		w *bufio.Writer
		n int64
	}{{ids.mw, ids.count * idRecord}, {ids.nw, ids.decls * model.IDHexLen}} {
		if n, err := io.CopyN(part.w, s.br, part.n); err != nil || n != part.n {
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
	count, err := s.uvarint("its identity table")
	if err != nil {
		return err
	}
	decls, err := s.uvarint("its identity table")
	if err != nil {
		return err
	}
	if count > 1<<31 || decls > 1<<62/model.IDHexLen {
		return stateCorrupt("its identity table's counts are malformed")
	}
	n := int64(count)*idRecord + int64(decls)*model.IDHexLen
	if m, err := io.CopyN(io.Discard, s.br, n); err != nil || m != n {
		return stateCorrupt("its identity table is short")
	}
	return nil
}
