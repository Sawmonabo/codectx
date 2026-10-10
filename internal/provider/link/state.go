package link

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The resolution state is what a unit's next build starts from: the manifest
// fields its resolution read, one record per member, and every binding, each
// named by identity. A site is its file and its index in that file's table,
// which is valid wherever the state is read: a member whose table digest
// held has the same sites at the same indices, and a member whose digest
// moved is resolved again. A target is its file and its declaration's node
// identity, which a body edit or an inserted declaration does not move
// (reconcile.CanonicalKey), so a binding into an edited file survives the
// edit and nothing has to be renumbered.
//
// The stored form is stateTag, then three sections, each its count and its
// entries in ascending order:
//   - the manifest fields (manifestFields): key and value, strings;
//   - the member records, by FileID (record);
//   - the bindings, by site then target (idBinding): site file, site index,
//     target file, target node and the technique as one byte;
//   - the index groups, by key (group), the recorded reads, by key (read),
//     and the address-taken functions, by the member taking them (taken),
//     each record a string of its encoded form: what a refresh of a
//     static-family unit finds its re-runs from (trace.go), empty for a
//     family whose resolution records no reads.
//
// A string is its length (an unsigned varint) and its bytes. A state that is
// short, long, out of order or malformed is corrupt storage.

// stateTag opens every stored resolution state, so a state of any other form
// is refused before anything of it is read.
const stateTag = "link.resolution.identity\n"

// details is every technique a binding can carry, in its stored order.
var details = [...]string{DetailDirect, DetailHierarchy, DetailAddressTaken, DetailFlow, DetailField}

// detailCode is the stored byte of a binding's technique.
func detailCode(d string) (byte, bool) {
	for i, v := range details {
		if v == d {
			return byte(i), true
		}
	}
	return 0, false
}

// compareSiteTarget orders positional bindings by site, then target.
func compareSiteTarget(a, b Binding) int {
	return cmp.Or(compareRef(a.Site, b.Site), compareRef(a.Target, b.Target))
}

// compareTargetSite orders positional bindings by target, then site.
func compareTargetSite(a, b Binding) int {
	return cmp.Or(compareRef(a.Target, b.Target), compareRef(a.Site, b.Site))
}

// encodeBinding is a positional binding's record in a sorted run.
func encodeBinding(b Binding) ([]byte, error) {
	code, ok := detailCode(b.Detail)
	if !ok {
		return nil, internalErr("a resolver bound a site with the unknown technique " + strconv.Quote(b.Detail))
	}
	if b.Site.File < 0 || b.Site.Index < 0 || b.Target.File < 0 || b.Target.Index < 0 {
		return nil, internalErr("a resolver bound a site through a negative member or table index")
	}
	out := make([]byte, 0, 4*binary.MaxVarintLen32+1)
	for _, v := range [...]int32{b.Site.File, b.Site.Index, b.Target.File, b.Target.Index} {
		out = binary.AppendUvarint(out, uint64(v))
	}
	return append(out, code), nil
}

// decodeBinding reads one record encodeBinding wrote.
func decodeBinding(raw []byte) (Binding, error) {
	var v [4]int32
	for i := range v {
		n, w := binary.Uvarint(raw)
		if w <= 0 || n > 1<<31-1 {
			return Binding{}, errors.New("a binding index is malformed")
		}
		v[i], raw = int32(n), raw[w:]
	}
	if len(raw) != 1 || int(raw[0]) >= len(details) {
		return Binding{}, errors.New("a binding technique is malformed")
	}
	return Binding{Site: Ref{File: v[0], Index: v[1]}, Target: Ref{File: v[2], Index: v[3]}, Detail: details[raw[0]]}, nil
}

// newBindingSort opens a sort of positional bindings in dir by order.
func newBindingSort(dir string, order func(a, b Binding) int) (*pagination.ExternalSort[Binding], error) {
	return pagination.NewExternalSort(dir, 0, encodeBinding, decodeBinding, order)
}

// better reports whether technique a sorts before b in details, the one a
// fold of two bindings of one site and target keeps, so the published
// technique does not depend on the order a resolver emitted its repeats in.
func better(a, b string) bool {
	ca, _ := detailCode(a)
	cb, _ := detailCode(b)
	return ca < cb
}

// keepOne folds two positional bindings of one site and target into one.
func keepOne(a, b Binding) (Binding, error) {
	if better(b.Detail, a.Detail) {
		return b, nil
	}
	return a, nil
}

// idBinding is a binding named by identity: the site by its file and its
// index in that file's table, the target by its file and its declaration's
// node.
type idBinding struct {
	SiteFile   model.FileID
	Site       int32
	TargetFile model.FileID
	Target     model.NodeID
	Detail     string
}

// target is a declaration a binding targets.
type target struct {
	File model.FileID
	Node model.NodeID
}

func (b idBinding) target() target { return target{File: b.TargetFile, Node: b.Target} }

// compareIDSite orders bindings by site, then target: the stored order, and
// the order a file's sites are published in.
func compareIDSite(a, b idBinding) int {
	return cmp.Or(cmp.Compare(a.SiteFile, b.SiteFile), cmp.Compare(a.Site, b.Site),
		cmp.Compare(a.TargetFile, b.TargetFile), cmp.Compare(a.Target, b.Target))
}

func compareTarget(a, b target) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Node, b.Node))
}

// keepOneID folds two bindings of one site and target into one, as keepOne.
func keepOneID(a, b idBinding) (idBinding, error) {
	if better(b.Detail, a.Detail) {
		return b, nil
	}
	return a, nil
}

func appendString(out []byte, s string) []byte {
	return append(binary.AppendUvarint(out, uint64(len(s))), s...)
}

func encodeIDBinding(b idBinding) ([]byte, error) {
	code, ok := detailCode(b.Detail)
	if !ok {
		return nil, internalErr("a binding carries the unknown technique " + strconv.Quote(b.Detail))
	}
	if b.Site < 0 {
		return nil, internalErr("a binding names a negative site index")
	}
	out := appendString(nil, string(b.SiteFile))
	out = binary.AppendUvarint(out, uint64(b.Site))
	out = appendString(out, string(b.TargetFile))
	out = appendString(out, string(b.Target))
	return append(out, code), nil
}

func decodeIDBinding(raw []byte) (idBinding, error) {
	r := byteReader{b: raw}
	b := idBinding{SiteFile: model.FileID(r.str()), Site: r.index(), TargetFile: model.FileID(r.str()), Target: model.NodeID(r.str())}
	code := r.byte()
	if r.err != nil || len(r.b) != 0 || int(code) >= len(details) {
		return idBinding{}, errors.New("a binding is malformed")
	}
	b.Detail = details[code]
	return b, nil
}

func newIDBindingSort(dir string) (*pagination.ExternalSort[idBinding], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeIDBinding, decodeIDBinding, compareIDSite)
	if err != nil {
		return nil, err
	}
	return s.WithFold(keepOneID), nil
}

func encodeTarget(t target) ([]byte, error) {
	return appendString(appendString(nil, string(t.File)), string(t.Node)), nil
}

func decodeTarget(raw []byte) (target, error) {
	r := byteReader{b: raw}
	t := target{File: model.FileID(r.str()), Node: model.NodeID(r.str())}
	if r.err != nil || len(r.b) != 0 {
		return target{}, errors.New("a binding target is malformed")
	}
	return t, nil
}

// targetsOf is the declarations run's bindings target, ascending and each
// once.
func targetsOf(dir string, run *pagination.SortedRun[idBinding]) (*pagination.SortedRun[target], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeTarget, decodeTarget, compareTarget)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	s.WithFold(func(a, _ target) (target, error) { return a, nil })
	if err := run.Each(func(b idBinding) error { return s.Add(b.target()) }); err != nil {
		return nil, err
	}
	return s.Sorted()
}

// record is what the state keeps of one member: its content, its table's
// digests (empty when it has no table) and its share of the unit's
// coverage. A build that does not read the member's table takes all of it
// from here.
type record struct {
	File model.FileID
	// Content is the content hash the member had.
	Content string
	// Digest is the table's MemberDigest, and Interface and Sites its two
	// summaries' digests.
	Digest, Interface, Sites string
	Tabled                   bool
	// Dropped is the table's Dropped count, and Unkeyed the member's
	// ambiguous sites whose call-site key does not fit a native key.
	Dropped, Unkeyed int64
}

func encodeRecord(r record) ([]byte, error) {
	out := appendString(nil, string(r.File))
	for _, s := range [...]string{r.Content, r.Digest, r.Interface, r.Sites} {
		out = appendString(out, s)
	}
	tabled := byte(0)
	if r.Tabled {
		tabled = 1
	}
	out = append(out, tabled)
	if r.Dropped < 0 || r.Unkeyed < 0 {
		return nil, internalErr("a member record holds a negative count")
	}
	out = binary.AppendUvarint(out, uint64(r.Dropped))
	return binary.AppendUvarint(out, uint64(r.Unkeyed)), nil
}

func decodeRecord(raw []byte) (record, error) {
	r := byteReader{b: raw}
	rec := record{File: model.FileID(r.str()), Content: r.str(), Digest: r.str(), Interface: r.str(), Sites: r.str()}
	tabled := r.byte()
	rec.Dropped, rec.Unkeyed = r.count(), r.count()
	if r.err != nil || len(r.b) != 0 || tabled > 1 {
		return record{}, errors.New("a member record is malformed")
	}
	rec.Tabled = tabled == 1
	return rec, nil
}

func newRecordSort(dir string) (*pagination.ExternalSort[record], error) {
	return pagination.NewExternalSort(dir, 0, encodeRecord, decodeRecord,
		func(a, b record) int { return cmp.Compare(a.File, b.File) })
}

// manifestField is one field of a manifest the unit's resolution reads
// (manifestFields).
type manifestField struct{ Key, Value string }

func compareField(a, b manifestField) int {
	return cmp.Or(cmp.Compare(a.Key, b.Key), cmp.Compare(a.Value, b.Value))
}

// byteReader reads the fields of one stored record; the first failure
// sticks in err.
type byteReader struct {
	b   []byte
	err error
}

func (r *byteReader) fail() {
	if r.err == nil {
		r.err = errors.New("short record")
	}
}

func (r *byteReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	n, w := binary.Uvarint(r.b)
	if w <= 0 {
		r.fail()
		return 0
	}
	r.b = r.b[w:]
	return n
}

func (r *byteReader) str() string {
	n := r.uvarint()
	if r.err != nil || uint64(len(r.b)) < n {
		r.fail()
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func (r *byteReader) byte() byte {
	if r.err != nil || len(r.b) == 0 {
		r.fail()
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *byteReader) index() int32 {
	n := r.uvarint()
	if n > 1<<31-1 {
		r.fail()
	}
	return int32(n)
}

func (r *byteReader) count() int64 {
	n := r.uvarint()
	if n > 1<<63-1 {
		r.fail()
	}
	return int64(n)
}

// writeState stores fields, records (by FileID) and res to w.
func writeState(w io.Writer, fields []manifestField, records *pagination.SortedRun[record], res *resolution) error {
	bw := bufio.NewWriter(w)
	out := binary.AppendUvarint([]byte(stateTag), uint64(len(fields)))
	for _, f := range fields {
		out = appendString(appendString(out, f.Key), f.Value)
	}
	_, err := bw.Write(binary.AppendUvarint(out, uint64(records.Len())))
	if err == nil {
		err = records.Each(func(r record) error {
			rec, err := encodeRecord(r)
			if err == nil {
				_, err = bw.Write(rec)
			}
			return err
		})
	}
	if err == nil {
		_, err = bw.Write(binary.AppendUvarint(nil, uint64(res.bindings.Len())))
	}
	if err == nil {
		err = res.bindings.Each(func(b idBinding) error {
			rec, err := encodeIDBinding(b)
			if err == nil {
				_, err = bw.Write(rec)
			}
			return err
		})
	}
	if err == nil {
		err = writeSection(bw, res.groups, encodeGroup)
	}
	if err == nil {
		err = writeSection(bw, res.reads, encodeRead)
	}
	if err == nil {
		err = writeSection(bw, res.taken, encodeTaken)
	}
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		return internalErr("link resolution state: " + err.Error())
	}
	return nil
}

// writeSection writes run's count, then each record's encoded form as a
// string.
func writeSection[T any](bw *bufio.Writer, run *pagination.SortedRun[T], encode func(T) ([]byte, error)) error {
	if _, err := bw.Write(binary.AppendUvarint(nil, uint64(run.Len()))); err != nil {
		return err
	}
	return run.Each(func(v T) error {
		rec, err := encode(v)
		if err == nil {
			_, err = bw.Write(appendString(nil, string(rec)))
		}
		return err
	})
}

// readSection reads one section writeSection wrote, refusing records out of
// order (or, with strict, repeated).
func readSection[T any](s stateReader, what string, decode func([]byte) (T, error), compare func(a, b T) int,
	strict bool, add func(T) error) error {
	n, err := s.uvarint("its " + what + " count")
	if err != nil {
		return err
	}
	var last T
	for i := uint64(0); i < n; i++ {
		raw, err := s.str("a record of its " + what)
		if err != nil {
			return err
		}
		v, err := decode([]byte(raw))
		if err != nil {
			return stateCorrupt(err.Error())
		}
		if c := compare(last, v); i > 0 && (c > 0 || strict && c == 0) {
			return stateCorrupt("its " + what + " are out of order")
		}
		last = v
		if err := add(v); err != nil {
			return err
		}
	}
	return nil
}

// stateReader reads a stored state's fields from its stream.
type stateReader struct {
	br *bufio.Reader
}

func (s stateReader) uvarint(what string) (uint64, error) {
	n, err := binary.ReadUvarint(s.br)
	if err != nil {
		return 0, stateCorrupt(what + " is short")
	}
	return n, nil
}

func (s stateReader) str(what string) (string, error) {
	n, err := s.uvarint(what)
	if err != nil {
		return "", err
	}
	// A string longer than what is left is short, which reading it says;
	// the length is never trusted to size a buffer up front.
	var b []byte
	for n > 0 {
		chunk := min(n, 1<<16)
		start := len(b)
		b = append(b, make([]byte, chunk)...)
		if _, err := io.ReadFull(s.br, b[start:]); err != nil {
			return "", stateCorrupt(what + " is short")
		}
		n -= chunk
	}
	return string(b), nil
}

func (s stateReader) byte(what string) (byte, error) {
	c, err := s.br.ReadByte()
	if err != nil {
		return 0, stateCorrupt(what + " is short")
	}
	return c, nil
}

// readState streams a stored state's sections to field, rec and bind, each
// in its stored order, and refuses a state that is short, long, out of
// order or malformed.
func readState(r io.Reader, sink stateSink) error {
	field, rec, bind := sink.field, sink.record, sink.binding
	s := stateReader{br: bufio.NewReader(r)}
	tag := make([]byte, len(stateTag))
	if _, err := io.ReadFull(s.br, tag); err != nil || string(tag) != stateTag {
		return stateCorrupt("it is not a resolution state of this form")
	}
	n, err := s.uvarint("its field count")
	if err != nil {
		return err
	}
	var lastField manifestField
	for i := uint64(0); i < n; i++ {
		var f manifestField
		if f.Key, err = s.str("a manifest field"); err != nil {
			return err
		}
		if f.Value, err = s.str("a manifest field"); err != nil {
			return err
		}
		if i > 0 && compareField(lastField, f) > 0 {
			return stateCorrupt("its manifest fields are out of order")
		}
		lastField = f
		if err := field(f); err != nil {
			return err
		}
	}
	if n, err = s.uvarint("its record count"); err != nil {
		return err
	}
	var lastFile model.FileID
	for i := uint64(0); i < n; i++ {
		var r record
		strs := [...]*string{(*string)(&r.File), &r.Content, &r.Digest, &r.Interface, &r.Sites}
		for _, p := range strs {
			if *p, err = s.str("a member record"); err != nil {
				return err
			}
		}
		tabled, err := s.byte("a member record")
		if err != nil {
			return err
		}
		if tabled > 1 {
			return stateCorrupt("a member record is malformed")
		}
		r.Tabled = tabled == 1
		for _, p := range [...]*int64{&r.Dropped, &r.Unkeyed} {
			c, err := s.uvarint("a member record")
			if err != nil {
				return err
			}
			if c > 1<<63-1 {
				return stateCorrupt("a member record's count is malformed")
			}
			*p = int64(c)
		}
		if i > 0 && r.File <= lastFile {
			return stateCorrupt("its member records are out of order")
		}
		lastFile = r.File
		if err := rec(r); err != nil {
			return err
		}
	}
	if n, err = s.uvarint("its binding count"); err != nil {
		return err
	}
	var last idBinding
	for i := uint64(0); i < n; i++ {
		var b idBinding
		if f, err := s.str("a binding"); err != nil {
			return err
		} else {
			b.SiteFile = model.FileID(f)
		}
		site, err := s.uvarint("a binding")
		if err != nil {
			return err
		}
		if site > 1<<31-1 {
			return stateCorrupt("a binding's site index is malformed")
		}
		b.Site = int32(site)
		tf, err := s.str("a binding")
		if err != nil {
			return err
		}
		node, err := s.str("a binding")
		if err != nil {
			return err
		}
		b.TargetFile, b.Target = model.FileID(tf), model.NodeID(node)
		code, err := s.byte("a binding")
		if err != nil {
			return err
		}
		if int(code) >= len(details) {
			return stateCorrupt("a binding technique is malformed")
		}
		b.Detail = details[code]
		if i > 0 && compareIDSite(last, b) >= 0 {
			return stateCorrupt("its bindings are out of order")
		}
		last = b
		if err := bind(b); err != nil {
			return err
		}
	}
	if err := readSection(s, "index groups", decodeGroup, compareGroup, true, sink.group); err != nil {
		return err
	}
	if err := readSection(s, "recorded reads", decodeRead, compareRead, true, sink.read); err != nil {
		return err
	}
	if err := readSection(s, "address-taken functions", decodeTaken, compareTakenFrom, true, sink.taken); err != nil {
		return err
	}
	if _, err := s.br.ReadByte(); err != io.EOF {
		return stateCorrupt("it holds more than its sections")
	}
	return nil
}

// stateSink receives a stored state's sections, each in its stored order.
type stateSink struct {
	field   func(manifestField) error
	record  func(record) error
	binding func(idBinding) error
	group   func(group) error
	read    func(read) error
	taken   func(taken) error
}

// ValidateState reads a stored resolution state through and reports whether
// it is well formed, so a build can resolve in full instead of handing Link a
// state it would refuse once it had begun publishing.
func ValidateState(r io.Reader) error {
	return readState(r, stateSink{
		field:   func(manifestField) error { return nil },
		record:  func(record) error { return nil },
		binding: func(idBinding) error { return nil },
		group:   func(group) error { return nil },
		read:    func(read) error { return nil },
		taken:   func(taken) error { return nil },
	})
}

// previous is a predecessor's state as one build reads it: its manifest
// fields, and its records and resolution spilled to sorted runs in the
// unit's scratch directory.
type previous struct {
	fields  []manifestField
	records *pagination.SortedRun[record]
	resolution
}

// loadState reads a stored state into dir.
func loadState(r io.Reader, dir string) (*previous, error) {
	records, err := newRecordSort(dir)
	if err != nil {
		return nil, err
	}
	defer records.Close()
	bindings, err := newIDBindingSort(dir)
	if err != nil {
		return nil, err
	}
	defer bindings.Close()
	groups, err := newGroupSort(dir)
	if err != nil {
		return nil, err
	}
	defer groups.Close()
	reads, err := newReadSort(dir, compareRead)
	if err != nil {
		return nil, err
	}
	defer reads.Close()
	takenSort, err := newTakenSort(dir, compareTakenFrom)
	if err != nil {
		return nil, err
	}
	defer takenSort.Close()
	p := &previous{}
	err = readState(r, stateSink{
		field: func(f manifestField) error {
			p.fields = append(p.fields, f)
			return nil
		},
		record: records.Add, binding: bindings.Add, group: groups.Add, read: reads.Add, taken: takenSort.Add,
	})
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	if p.records, err = records.Sorted(); err != nil {
		return nil, err
	}
	if p.bindings, err = bindings.Sorted(); err != nil {
		return nil, err
	}
	if p.groups, err = groups.Sorted(); err != nil {
		return nil, err
	}
	if p.reads, err = reads.Sorted(); err != nil {
		return nil, err
	}
	if p.taken, err = takenSort.Sorted(); err != nil {
		return nil, err
	}
	ok = true
	return p, nil
}

func (p *previous) close() {
	if p != nil {
		p.records.Close()
		p.resolution.close()
	}
}

func stateCorrupt(msg string) *model.Error {
	return &model.Error{Code: model.CodeStorageCorrupt, Message: "the previous link resolution state is corrupt: " + msg}
}

func internalErr(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
