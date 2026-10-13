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
// fields its resolution read, one record per member, and every fact, each
// named by identity. A fact's entry is its file and its index in that file's
// table (a site, an import or a declaration, by kind), which is valid
// wherever the state is read: a member whose table digest held has the same
// entries at the same indices, and a member whose digest moved is resolved
// again. A target is its file and its node identity (a declaration's, a
// file's module's or a parameter's), which a body edit or an inserted
// declaration does not move (reconcile.CanonicalKey), so a fact into an
// edited file survives the edit and nothing has to be renumbered; an
// external target is its external key (externalType, externalMethod).
//
// The stored form is stateTag, then these sections, each its count and its
// entries in ascending order:
//   - the manifest fields (manifestFields): key and value, strings;
//   - the member records, by FileID (record), each a string of its encoded
//     form;
//   - the facts, by entry then target (idBinding), each a string of its
//     encoded form;
//   - the index groups, by key (group): the hierarchy's and, for the flow
//     family, the module index's;
//   - the recorded reads, by key (read), and the address-taken functions,
//     by the member taking them (taken), each record a string of its
//     encoded form: what a refresh of a static-family unit finds its
//     re-runs from (trace.go), empty for the flow family;
//   - every value each node of the flow family's fixed point put into a
//     variable, by entry, value and node (idContribution); every variable
//     each node read, by entry and node (idRead); and every key of the
//     hierarchy or the module index each node looked up, by key and node
//     (nodeTrace), each named by identity (flowstate.go) and each a string
//     of its encoded form: what a refresh of a flow-family unit finds its
//     region from (refresh.go), empty for the static family;
//   - a byte, 1 when the unit's hierarchy follows (its module path
//     and root, its two indexes and its identity table, as they are on
//     disk), else 0;
//   - a byte, 1 when the flow family's module index follows (as it is on
//     disk), else 0.
//
// A string is its length (an unsigned varint) and its bytes. A state that is
// short, long, out of order or malformed is corrupt storage.

// stateTag opens every stored resolution state, so a state of any other form
// is refused before anything of it is read.
const stateTag = "link.resolution.nodes\n"

// details is every technique a fact can carry, in its stored order, which is
// also the order of preference a fold of two equal facts keeps (better); the
// empty technique, a fact no technique is named for (an open or external
// site), is last.
var details = [...]string{DetailDirect, DetailHierarchy, DetailAddressTaken, DetailFlow, DetailField,
	DetailImport, DetailArgument, DetailSupertype, DetailMethodSet, DetailSupertypeInterface, DetailSupertypeClass,
	DetailSummary, DetailReturn, DetailSupertypeCandidate, ""}

// detailCode is the stored byte of a fact's technique.
func detailCode(d string) (byte, bool) {
	for i, v := range details {
		if v == d {
			return byte(i), true
		}
	}
	return 0, false
}

// dispatches and reasons are the stored bytes of a call fact's dispatch and
// an open site's reason; the empty value is 0.
var (
	dispatches = [...]model.Dispatch{"", model.DispatchVirtual, model.DispatchExact}
	reasons    = [...]model.OpenSiteReason{"", model.ReasonReceiverUntyped, model.ReasonValueUntracked,
		model.ReasonGlobalUnbound, model.ReasonComputed}
)

func codeOf[T comparable](values []T, v T) (byte, bool) {
	for i, x := range values {
		if x == v {
			return byte(i), true
		}
	}
	return 0, false
}

// entry is the table a fact of kind k indexes with its Site: 0 a call site
// (Table.Sites), 1 an import (Table.Imports), 2 a declaration (Table.Decls).
// Facts sort by file, then entry table, then entry, so one file's facts
// about one site, import or declaration are adjacent.
func (k Fact) entry() int {
	switch k {
	case FactImport, FactOpenImport:
		return 1
	case FactExtends, FactImplements, FactOverrides:
		return 2
	}
	return 0
}

// hasTarget reports whether a fact of kind k names a target of the unit when
// it names no external one.
func (k Fact) hasTarget() bool {
	switch k {
	case FactExternalSite, FactOpenSite, FactOpenImport:
		return false
	}
	return true
}

// external reports whether a fact of kind k may name an external target.
func (k Fact) external() bool {
	switch k {
	case FactCall, FactExtends, FactImplements, FactOverrides:
		return true
	}
	return false
}

// canonical is b checked against what its kind states and with every field
// its kind does not use zeroed, so two emissions of one fact compare equal
// and fold whatever a resolver left in the unused fields. A fact whose
// fields contradict its kind is a resolver defect.
func canonical(b Binding) (Binding, error) {
	bad := func(what string) (Binding, error) {
		return Binding{}, internalErr("a resolver emitted a fact of kind " + strconv.Itoa(int(b.Kind)) + " " + what)
	}
	if b.Kind > FactOpenImport {
		return bad("that is not a kind of fact")
	}
	if _, ok := detailCode(b.Detail); !ok {
		return bad("with the unknown technique " + strconv.Quote(b.Detail))
	}
	if b.Site.File < 0 || b.Site.Index < 0 {
		return bad("about a negative member or table index")
	}
	if b.Kind == FactCall {
		if b.Dispatch == "" || !b.Dispatch.Valid() {
			return bad("with no dispatch or an unknown one")
		}
	} else {
		b.Dispatch = ""
	}
	if b.Kind == FactOpenSite {
		if !b.Reason.Valid() {
			return bad("with no open-site reason or an unknown one")
		}
	} else {
		b.Reason = ""
	}
	if b.Kind != FactFlow && b.Kind != FactSummary {
		b.Arg, b.Param = 0, 0
	} else if b.Arg < 0 || b.Param < 0 {
		return bad("with a negative argument or parameter index")
	}
	if b.Kind != FactSummary {
		b.Position = 0
	} else if b.Position < 0 {
		return bad("with a negative parameter position")
	}
	switch {
	case b.External != "":
		if !b.Kind.external() {
			return bad("naming an external target")
		}
		if _, ok := externalCandidate(b.External); !ok {
			return bad("naming a malformed external key")
		}
		b.Target = Ref{}
	case !b.Kind.hasTarget():
		b.Target = Ref{}
	case b.Kind == FactImport:
		if b.Target.File < 0 || b.Target.Index != -1 {
			return bad("naming no member's module")
		}
	case b.Target.File < 0 || b.Target.Index < 0:
		return bad("naming a negative member or table index")
	}
	return b, nil
}

// better reports whether technique a sorts before b in details, the one a
// fold of two equal facts keeps, so the published technique does not depend
// on the order a resolver emitted its repeats in.
func better(a, b string) bool {
	ca, _ := detailCode(a)
	cb, _ := detailCode(b)
	return ca < cb
}

// foldDispatch is the dispatch of two emissions of one call fact: virtual
// when either can run an override.
func foldDispatch(a, b model.Dispatch) model.Dispatch {
	if a == model.DispatchVirtual || b == model.DispatchVirtual {
		return model.DispatchVirtual
	}
	return cmp.Or(a, b)
}

// foldReason is the reason of two emissions of one open-site fact: the one
// that sorts first in reasons, so the fold does not depend on their order.
func foldReason(a, b model.OpenSiteReason) model.OpenSiteReason {
	ca, _ := codeOf(reasons[:], a)
	cb, _ := codeOf(reasons[:], b)
	if a == "" || b != "" && cb < ca {
		return b
	}
	return a
}

// idBinding is a fact named by identity: its entry by its file and its index
// in that file's table (Site, by Kind's entry table), and its target by its
// file and node (a declaration's, a member's module's, a parameter's) or by
// its external key. A fact with no target (an open or external site) has all
// three empty.
type idBinding struct {
	Kind     Fact
	SiteFile model.FileID
	Site     int32
	Arg      int32
	// Position is a FactSummary's parameter position (Binding.Position).
	Position   int32
	TargetFile model.FileID
	Target     model.NodeID
	External   string
	Detail     string
	Dispatch   model.Dispatch
	Reason     model.OpenSiteReason
}

// target is a node of the unit a fact targets, with its file.
type target struct {
	File model.FileID
	Node model.NodeID
}

func (b idBinding) target() target { return target{File: b.TargetFile, Node: b.Target} }

// compareIDSite orders facts by file, entry table, entry, kind, argument and
// target: the stored order, the order a file's facts are published in, and
// the key one fact has, which a fold collapses (keepOneID).
func compareIDSite(a, b idBinding) int {
	return cmp.Or(cmp.Compare(a.SiteFile, b.SiteFile), cmp.Compare(a.Kind.entry(), b.Kind.entry()),
		cmp.Compare(a.Site, b.Site), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Arg, b.Arg),
		cmp.Compare(a.TargetFile, b.TargetFile), cmp.Compare(a.Target, b.Target), cmp.Compare(a.External, b.External))
}

func compareTarget(a, b target) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Node, b.Node))
}

// keepOneID folds two facts of one key into one: the better technique, the
// folded dispatch and reason.
func keepOneID(a, b idBinding) (idBinding, error) {
	out := a
	if better(b.Detail, a.Detail) {
		out = b
	}
	out.Dispatch, out.Reason = foldDispatch(a.Dispatch, b.Dispatch), foldReason(a.Reason, b.Reason)
	return out, nil
}

func appendString(out []byte, s string) []byte {
	return append(binary.AppendUvarint(out, uint64(len(s))), s...)
}

func encodeIDBinding(b idBinding) ([]byte, error) {
	code, ok := detailCode(b.Detail)
	dispatch, dok := codeOf(dispatches[:], b.Dispatch)
	reason, rok := codeOf(reasons[:], b.Reason)
	if !ok || !dok || !rok || b.Kind > FactOpenImport || b.Site < 0 || b.Arg < 0 || b.Position < 0 {
		return nil, internalErr("a fact named by identity is malformed")
	}
	out := appendString([]byte{byte(b.Kind)}, string(b.SiteFile))
	out = binary.AppendUvarint(binary.AppendUvarint(out, uint64(b.Site)), uint64(b.Arg))
	out = binary.AppendUvarint(out, uint64(b.Position))
	out = appendString(out, string(b.TargetFile))
	out = appendString(out, string(b.Target))
	out = appendString(out, b.External)
	return append(out, code, dispatch, reason), nil
}

func decodeIDBinding(raw []byte) (idBinding, error) {
	r := byteReader{b: raw}
	b := idBinding{Kind: Fact(r.byte()), SiteFile: model.FileID(r.str()), Site: r.index(), Arg: r.index(),
		Position: r.index(), TargetFile: model.FileID(r.str()), Target: model.NodeID(r.str()), External: r.str()}
	code, dispatch, reason := r.byte(), r.byte(), r.byte()
	if r.err != nil || len(r.b) != 0 || b.Kind > FactOpenImport || int(code) >= len(details) ||
		int(dispatch) >= len(dispatches) || int(reason) >= len(reasons) {
		return idBinding{}, errors.New("a fact is malformed")
	}
	b.Detail, b.Dispatch, b.Reason = details[code], dispatches[dispatch], reasons[reason]
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
		return target{}, errors.New("a fact's target is malformed")
	}
	return t, nil
}

// targetsOf is the nodes of the unit run's facts target, ascending and each
// once: declarations, members' modules and parameters. An external target is
// none of them (externalsOf).
func targetsOf(dir string, run *pagination.SortedRun[idBinding]) (*pagination.SortedRun[target], error) {
	s, err := pagination.NewExternalSort(dir, 0, encodeTarget, decodeTarget, compareTarget)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	s.WithFold(func(a, _ target) (target, error) { return a, nil })
	err = run.Each(func(b idBinding) error {
		if b.Target == "" {
			return nil
		}
		return s.Add(b.target())
	})
	if err != nil {
		return nil, err
	}
	return s.Sorted()
}

// externalsOf is the external keys run's facts target, ascending and each
// once.
func externalsOf(dir string, run *pagination.SortedRun[idBinding]) (*pagination.SortedRun[string], error) {
	s, err := newKeySort(dir)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	err = run.Each(func(b idBinding) error {
		if b.External == "" {
			return nil
		}
		return s.Add(b.External)
	})
	if err != nil {
		return nil, err
	}
	return s.Sorted()
}

// counts are one member's share of the counts its unit's capability rows
// carry, so a row sums them over the records without reading a carried
// member's table: its call sites, those bound to a declaration of the unit,
// those open (Computed ones excluded), the computed ones, those external;
// its imports resolved to a member and its open import sites.
type counts struct {
	Sites, Bound, Open, Computed, External int64
	Imports, OpenImports                   int64
}

func (c *counts) add(o counts) {
	c.Sites += o.Sites
	c.Bound += o.Bound
	c.Open += o.Open
	c.Computed += o.Computed
	c.External += o.External
	c.Imports += o.Imports
	c.OpenImports += o.OpenImports
}

// record is what the state keeps of one member: its content, its table's
// digests (empty when it has no table), its table's Dropped count and its
// counts. A build that does not read the member's table takes all of it from
// here.
type record struct {
	File model.FileID
	// Content is the content hash the member had.
	Content string
	// Digest is the table's MemberDigest, and Interface and Sites its two
	// summaries' digests.
	Digest, Interface, Sites string
	Tabled                   bool
	Dropped                  int64
	Counts                   counts
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
	c := r.Counts
	for _, n := range [...]int64{r.Dropped, c.Sites, c.Bound, c.Open, c.Computed, c.External, c.Imports, c.OpenImports} {
		if n < 0 {
			return nil, internalErr("a member record holds a negative count")
		}
		out = binary.AppendUvarint(out, uint64(n))
	}
	return out, nil
}

func decodeRecord(raw []byte) (record, error) {
	r := byteReader{b: raw}
	rec := record{File: model.FileID(r.str()), Content: r.str(), Digest: r.str(), Interface: r.str(), Sites: r.str()}
	tabled := r.byte()
	c := &rec.Counts
	for _, p := range [...]*int64{&rec.Dropped, &c.Sites, &c.Bound, &c.Open, &c.Computed, &c.External, &c.Imports, &c.OpenImports} {
		*p = r.count()
	}
	if r.err != nil || len(r.b) != 0 || tabled > 1 {
		return record{}, errors.New("a member record is malformed")
	}
	rec.Tabled = tabled == 1
	return rec, nil
}

func compareRecord(a, b record) int { return cmp.Compare(a.File, b.File) }

func newRecordSort(dir string) (*pagination.ExternalSort[record], error) {
	return pagination.NewExternalSort(dir, 0, encodeRecord, decodeRecord, compareRecord)
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
	_, err := bw.Write(out)
	if err == nil {
		err = writeSection(bw, records, encodeRecord)
	}
	if err == nil {
		err = writeSection(bw, res.bindings, encodeIDBinding)
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
		err = writeSection(bw, res.flowContributions, encodeWith(idContributionCodec))
	}
	if err == nil {
		err = writeSection(bw, res.flowReads, encodeWith(idReadCodec))
	}
	if err == nil {
		err = writeSection(bw, res.flowTraces, encodeWith(nodeTraceCodec))
	}
	if err == nil {
		if res.h == nil {
			err = bw.WriteByte(0)
		} else if err = bw.WriteByte(1); err == nil {
			err = res.h.writeTo(bw)
		}
	}
	if err == nil {
		if res.mods == nil {
			err = bw.WriteByte(0)
		} else if err = bw.WriteByte(1); err == nil {
			err = res.mods.writeTo(bw)
		}
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

// readState streams a stored state's sections to sink, each in its stored
// order, and refuses a state that is short, long, out of order, repeated or
// malformed.
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
	if err := readSection(s, "member records", decodeRecord, compareRecord, true, rec); err != nil {
		return err
	}
	if err := readSection(s, "facts", decodeIDBinding, compareIDSite, true, bind); err != nil {
		return err
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
	if err := readSection(s, "flow contributions", decodeWith(idContributionCodec, "a flow contribution"),
		idContributionCodec.cmp, true, sink.contribution); err != nil {
		return err
	}
	if err := readSection(s, "flow reads", decodeWith(idReadCodec, "a flow read"), idReadCodec.cmp, true,
		sink.flowRead); err != nil {
		return err
	}
	if err := readSection(s, "flow lookups", decodeWith(nodeTraceCodec, "a flow lookup"), nodeTraceCodec.cmp, true,
		sink.flowTrace); err != nil {
		return err
	}
	for _, part := range []struct {
		what string
		read func(stateReader) error
	}{{"its hierarchy mark", sink.hierarchy}, {"its module index mark", sink.modules}} {
		switch has, err := s.byte(part.what); {
		case err != nil:
			return err
		case has == 1:
			if err := part.read(s); err != nil {
				return err
			}
		case has != 0:
			return stateCorrupt(part.what + " is malformed")
		}
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
	// contribution, flowRead and flowTrace receive the flow family's
	// contributions, reads and lookups (refresh.go).
	contribution func(idContribution) error
	flowRead     func(idRead) error
	flowTrace    func(nodeTrace) error
	// hierarchy and modules read the stored hierarchy and module index
	// from the stream, when there is one.
	hierarchy func(stateReader) error
	modules   func(stateReader) error
}

// ValidateState reads a stored resolution state through and reports whether
// it is well formed, so a build can resolve in full instead of handing Link a
// state it would refuse once it had begun publishing.
func ValidateState(r io.Reader) error {
	return readState(r, stateSink{
		field:        func(manifestField) error { return nil },
		record:       func(record) error { return nil },
		binding:      func(idBinding) error { return nil },
		group:        func(group) error { return nil },
		read:         func(read) error { return nil },
		taken:        func(taken) error { return nil },
		contribution: func(idContribution) error { return nil },
		flowRead:     func(idRead) error { return nil },
		flowTrace:    func(nodeTrace) error { return nil },
		hierarchy:    skipHierarchy,
		modules:      skipIndex,
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
	contributions, err := newSort0(dir, idContributionCodec, nil)
	if err != nil {
		return nil, err
	}
	defer contributions.Close()
	flowReads, err := newSort0(dir, idReadCodec, nil)
	if err != nil {
		return nil, err
	}
	defer flowReads.Close()
	flowTraces, err := newSort0(dir, nodeTraceCodec, nil)
	if err != nil {
		return nil, err
	}
	defer flowTraces.Close()
	p := &previous{}
	defer func() {
		if p.records == nil {
			if p.h != nil {
				_ = p.h.close()
			}
			p.mods.close()
		}
	}()
	err = readState(r, stateSink{
		field: func(f manifestField) error {
			p.fields = append(p.fields, f)
			return nil
		},
		record: records.Add, binding: bindings.Add, group: groups.Add, read: reads.Add, taken: takenSort.Add,
		contribution: contributions.Add, flowRead: flowReads.Add, flowTrace: flowTraces.Add,
		hierarchy: func(s stateReader) error {
			var err error
			p.h, err = readHierarchy(dir, s)
			return err
		},
		modules: func(s stateReader) error {
			var err error
			p.mods, err = readIndex(dir, s)
			return err
		},
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
	if p.flowContributions, err = contributions.Sorted(); err != nil {
		return nil, err
	}
	if p.flowReads, err = flowReads.Sorted(); err != nil {
		return nil, err
	}
	if p.flowTraces, err = flowTraces.Sorted(); err != nil {
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
