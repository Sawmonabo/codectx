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

// The resolution state is a unit's bindings, positional: each names its site
// and its target by member ordinal and table index, never by range or node
// identity, and the unit's coverage. A later build whose every member keeps
// its MemberDigest holds the same declarations and sites at the same indices
// and drops the same entries, so it inherits the bindings and the coverage
// without resolving anything.
//
// The stored form is the binding count, then each binding in site-then-target
// order: site file, site index, target file, target index (unsigned varints)
// and the technique as one byte; then the coverage: the members without a
// table and the table entries dropped (unsigned varints). The count comes
// first so a stored state is never empty, which storage refuses, and so a
// short state is detected.

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

// compareSiteTarget orders bindings by site, then target.
func compareSiteTarget(a, b Binding) int {
	return cmp.Or(compareRef(a.Site, b.Site), compareRef(a.Target, b.Target))
}

// compareTargetSite orders bindings by target, then site.
func compareTargetSite(a, b Binding) int {
	return cmp.Or(compareRef(a.Target, b.Target), compareRef(a.Site, b.Site))
}

// encodeBinding is a binding's record in a sorted run and in the stored state.
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

// newBindingSort opens a sort of bindings in dir by order.
func newBindingSort(dir string, order func(a, b Binding) int) (*pagination.ExternalSort[Binding], error) {
	return pagination.NewExternalSort(dir, 0, encodeBinding, decodeBinding, order)
}

// keepOne folds two bindings of one site and target into one, keeping the
// technique that sorts first in details, so the published detail does not
// depend on the order a resolver emitted its repeats in.
func keepOne(a, b Binding) (Binding, error) {
	ca, _ := detailCode(a.Detail)
	cb, _ := detailCode(b.Detail)
	if cb < ca {
		return b, nil
	}
	return a, nil
}

// writeState stores run, the unit's folded bindings, and cov to w.
func writeState(w io.Writer, run *pagination.SortedRun[Binding], cov coverage) error {
	bw := bufio.NewWriter(w)
	if _, err := bw.Write(binary.AppendUvarint(nil, uint64(run.Len()))); err != nil {
		return internalErr("link resolution state: " + err.Error())
	}
	err := run.Each(func(b Binding) error {
		rec, err := encodeBinding(b)
		if err != nil {
			return err
		}
		_, err = bw.Write(rec)
		return err
	})
	if err == nil {
		_, err = bw.Write(binary.AppendUvarint(binary.AppendUvarint(nil, uint64(cov.untabled)), uint64(cov.dropped)))
	}
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		return internalErr("link resolution state: " + err.Error())
	}
	return nil
}

// readState streams a stored state's bindings to add, in their stored order,
// and returns its coverage. A state that is short, long or malformed is
// corrupt storage.
func readState(r io.Reader, add func(Binding) error) (coverage, error) {
	br := bufio.NewReader(r)
	n, err := binary.ReadUvarint(br)
	if err != nil {
		return coverage{}, stateCorrupt("its binding count is unreadable")
	}
	for i := uint64(0); i < n; i++ {
		var rec []byte
		for range 4 {
			v, err := binary.ReadUvarint(br)
			if err != nil {
				return coverage{}, stateCorrupt("binding " + strconv.FormatUint(i, 10) + " of " + strconv.FormatUint(n, 10) + " is short")
			}
			rec = binary.AppendUvarint(rec, v)
		}
		code, err := br.ReadByte()
		if err != nil {
			return coverage{}, stateCorrupt("binding " + strconv.FormatUint(i, 10) + " of " + strconv.FormatUint(n, 10) + " is short")
		}
		b, err := decodeBinding(append(rec, code))
		if err != nil {
			return coverage{}, stateCorrupt(err.Error())
		}
		if err := add(b); err != nil {
			return coverage{}, err
		}
	}
	var cov coverage
	for _, v := range [...]*int64{&cov.untabled, &cov.dropped} {
		c, err := binary.ReadUvarint(br)
		if err != nil || c > 1<<63-1 {
			return coverage{}, stateCorrupt("its coverage is unreadable")
		}
		*v = int64(c)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return coverage{}, stateCorrupt("it holds more than its " + strconv.FormatUint(n, 10) + " bindings and its coverage")
	}
	return cov, nil
}

// ValidateState reads a stored resolution state through and reports whether
// it is well formed, so a build can resolve in full instead of handing Link a
// state it would refuse once it had begun publishing.
func ValidateState(r io.Reader) error {
	_, err := readState(r, func(Binding) error { return nil })
	return err
}

func stateCorrupt(msg string) *model.Error {
	return &model.Error{Code: model.CodeStorageCorrupt, Message: "the previous link resolution state is corrupt: " + msg}
}

func internalErr(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
