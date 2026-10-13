package context

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unsafe"

	"github.com/Sawmonabo/codectx/internal/model"
)

// recordCodec is the binary layout of one sort record type: its fields in
// declaration order, each written as the smallest exact form of its kind -- a
// zig-zag varint for an integer, one byte for a bool, a length-prefixed byte
// string for a string, a count-prefixed list of those for a string slice, and
// a nested struct as its own fields in place.
//
// The layout is derived from the struct by reflection once per type and then
// run over field offsets, so it cannot drift from the struct it encodes, which
// is the property the JSON form was kept for, and it costs neither the field
// names nor the text scan JSON pays per record. The records never leave the
// process: a run is written and read back by the same binary, so the layout
// carries no version and no field tags.
//
// Round-trip: every value comes back exactly, byte for byte, with one
// normalization -- an empty slice decodes as nil -- which is the one the JSON
// form's omitempty made and no producer distinguishes.
type recordCodec struct {
	fields []codecField
	// layout names the type's fields, their kinds and their order, nested
	// structs included. A run written under one layout and read under another
	// would decode as wrong values rather than fail, so a run that outlives
	// the call that wrote it -- a continuation's checkpoint -- carries the
	// layout's digest and is refused by a build whose layout differs.
	layout string
}

type codecField struct {
	offset uintptr
	kind   reflect.Kind
	// nested is the layout of a struct field, nil otherwise.
	nested *recordCodec
}

// recordCodecs caches one layout per record type.
var recordCodecs sync.Map // reflect.Type -> *recordCodec

// codecFor answers T's layout, deriving it on first use. A type with a field
// of a kind the layout does not carry is a programming error in this package,
// reported as an internal error the first time the type is used.
func codecFor[T any]() (*recordCodec, error) {
	t := reflect.TypeFor[T]()
	if c, ok := recordCodecs.Load(t); ok {
		return c.(*recordCodec), nil
	}
	c, desc, err := deriveCodec(t)
	if err == nil {
		sum := sha256.Sum256([]byte(desc))
		c.layout = hex.EncodeToString(sum[:16])
	}
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "a context compile record has no binary layout: " + err.Error()}
	}
	actual, _ := recordCodecs.LoadOrStore(t, c)
	return actual.(*recordCodec), nil
}

// deriveCodec builds t's layout and the description its digest is taken over.
func deriveCodec(t reflect.Type) (*recordCodec, string, error) {
	if t.Kind() != reflect.Struct {
		return nil, "", fmt.Errorf("%s is not a struct", t)
	}
	c := &recordCodec{fields: make([]codecField, 0, t.NumField())}
	var desc strings.Builder
	desc.WriteString(t.String() + "{")
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		cf := codecField{offset: f.Offset, kind: f.Type.Kind()}
		desc.WriteString(f.Name + ":" + cf.kind.String())
		switch cf.kind {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int32, reflect.Int64, reflect.String:
		case reflect.Slice:
			if f.Type.Elem().Kind() != reflect.String {
				return nil, "", fmt.Errorf("%s.%s is a slice of %s", t, f.Name, f.Type.Elem())
			}
		case reflect.Struct:
			nested, nestedDesc, err := deriveCodec(f.Type)
			if err != nil {
				return nil, "", err
			}
			cf.nested = nested
			desc.WriteString(nestedDesc)
		default:
			return nil, "", fmt.Errorf("%s.%s has kind %s", t, f.Name, cf.kind)
		}
		desc.WriteString(";")
		c.fields = append(c.fields, cf)
	}
	desc.WriteString("}")
	return c, desc.String(), nil
}

// encodedSize is the exact length append will write for the record at p, so
// the record's buffer is allocated once at its final size.
func (c *recordCodec) encodedSize(p unsafe.Pointer) int {
	n := 0
	for _, f := range c.fields {
		fp := unsafe.Add(p, f.offset)
		switch f.kind {
		case reflect.Bool:
			n++
		case reflect.Int:
			n += varintLen(int64(*(*int)(fp)))
		case reflect.Int8:
			n += varintLen(int64(*(*int8)(fp)))
		case reflect.Int32:
			n += varintLen(int64(*(*int32)(fp)))
		case reflect.Int64:
			n += varintLen(*(*int64)(fp))
		case reflect.String:
			s := *(*string)(fp)
			n += uvarintLen(uint64(len(s))) + len(s)
		case reflect.Slice:
			list := *(*[]string)(fp)
			n += uvarintLen(uint64(len(list)))
			for _, s := range list {
				n += uvarintLen(uint64(len(s))) + len(s)
			}
		case reflect.Struct:
			n += f.nested.encodedSize(fp)
		}
	}
	return n
}

func (c *recordCodec) append(b []byte, p unsafe.Pointer) []byte {
	for _, f := range c.fields {
		fp := unsafe.Add(p, f.offset)
		switch f.kind {
		case reflect.Bool:
			if *(*bool)(fp) {
				b = append(b, 1)
			} else {
				b = append(b, 0)
			}
		case reflect.Int:
			b = binary.AppendVarint(b, int64(*(*int)(fp)))
		case reflect.Int8:
			b = binary.AppendVarint(b, int64(*(*int8)(fp)))
		case reflect.Int32:
			b = binary.AppendVarint(b, int64(*(*int32)(fp)))
		case reflect.Int64:
			b = binary.AppendVarint(b, *(*int64)(fp))
		case reflect.String:
			s := *(*string)(fp)
			b = binary.AppendUvarint(b, uint64(len(s)))
			b = append(b, s...)
		case reflect.Slice:
			list := *(*[]string)(fp)
			b = binary.AppendUvarint(b, uint64(len(list)))
			for _, s := range list {
				b = binary.AppendUvarint(b, uint64(len(s)))
				b = append(b, s...)
			}
		case reflect.Struct:
			b = f.nested.append(b, fp)
		}
	}
	return b
}

// recordReader walks one encoded record. Every string of the record is a
// slice of one copy of the record's bytes, so a decode allocates that copy
// once rather than once per string.
type recordReader struct {
	src string
	at  int
}

func (r *recordReader) varint() (int64, error) {
	v, n := varintFromString(r.src[r.at:])
	if n <= 0 {
		return 0, errRecordTruncated
	}
	r.at += n
	return v, nil
}

func (r *recordReader) str() (string, error) {
	l, n := uvarintFromString(r.src[r.at:])
	if n <= 0 || l > uint64(len(r.src)-r.at-n) {
		return "", errRecordTruncated
	}
	start := r.at + n
	r.at = start + int(l)
	return r.src[start:r.at], nil
}

func (c *recordCodec) read(r *recordReader, p unsafe.Pointer) error {
	for _, f := range c.fields {
		fp := unsafe.Add(p, f.offset)
		switch f.kind {
		case reflect.Bool:
			if r.at >= len(r.src) || r.src[r.at] > 1 {
				return errRecordTruncated
			}
			*(*bool)(fp) = r.src[r.at] == 1
			r.at++
		case reflect.Int, reflect.Int8, reflect.Int32, reflect.Int64:
			v, err := r.varint()
			if err != nil {
				return err
			}
			switch f.kind {
			case reflect.Int:
				*(*int)(fp) = int(v)
			case reflect.Int8:
				*(*int8)(fp) = int8(v)
			case reflect.Int32:
				*(*int32)(fp) = int32(v)
			default:
				*(*int64)(fp) = v
			}
		case reflect.String:
			s, err := r.str()
			if err != nil {
				return err
			}
			*(*string)(fp) = s
		case reflect.Slice:
			count, n := uvarintFromString(r.src[r.at:])
			// Every element costs at least its one length byte, which bounds
			// the allocation by the record's own size.
			if n <= 0 || count > uint64(len(r.src)-r.at-n) {
				return errRecordTruncated
			}
			r.at += n
			if count == 0 {
				*(*[]string)(fp) = nil
				continue
			}
			list := make([]string, count)
			for i := range list {
				s, err := r.str()
				if err != nil {
					return err
				}
				list[i] = s
			}
			*(*[]string)(fp) = list
		case reflect.Struct:
			if err := f.nested.read(r, fp); err != nil {
				return err
			}
		}
	}
	return nil
}

var errRecordTruncated = fmt.Errorf("the record ends before its layout does")

// recordLayout is the digest of T's layout (recordCodec.layout).
func recordLayout[T any]() (string, error) {
	c, err := codecFor[T]()
	if err != nil {
		return "", err
	}
	return c.layout, nil
}

// recordCodecFuncs answers the encode and decode functions a sort of T is
// opened with, resolving T's layout once for the sort rather than per record.
func recordCodecFuncs[T any]() (func(T) ([]byte, error), func([]byte) (T, error), error) {
	c, err := codecFor[T]()
	if err != nil {
		return nil, nil, err
	}
	encode := func(v T) ([]byte, error) {
		p := unsafe.Pointer(&v)
		return c.append(make([]byte, 0, c.encodedSize(p)), p), nil
	}
	decode := func(b []byte) (T, error) {
		var v T
		r := recordReader{src: string(b)}
		if err := c.read(&r, unsafe.Pointer(&v)); err != nil || r.at != len(r.src) {
			if err == nil {
				err = fmt.Errorf("%d bytes follow the record", len(r.src)-r.at)
			}
			var zero T
			return zero, &model.Error{Code: model.CodeInternal,
				Message: "decoding a context compile record from its external sort: " + err.Error()}
		}
		return v, nil
	}
	return encode, decode, nil
}

func varintLen(v int64) int {
	u := uint64(v) << 1
	if v < 0 {
		u = ^u
	}
	return uvarintLen(u)
}

func uvarintLen(u uint64) int {
	n := 1
	for u >= 0x80 {
		u >>= 7
		n++
	}
	return n
}

// uvarintFromString is binary.Uvarint over a string, so the reader walks its
// one copy of the record without converting slices back to bytes.
func uvarintFromString(s string) (uint64, int) {
	var x uint64
	var shift uint
	for i := 0; i < len(s) && i < binary.MaxVarintLen64; i++ {
		c := s[i]
		if c < 0x80 {
			if i == binary.MaxVarintLen64-1 && c > 1 {
				return 0, -(i + 1)
			}
			return x | uint64(c)<<shift, i + 1
		}
		x |= uint64(c&0x7f) << shift
		shift += 7
	}
	return 0, 0
}

func varintFromString(s string) (int64, int) {
	ux, n := uvarintFromString(s)
	x := int64(ux >> 1)
	if ux&1 != 0 {
		x = ^x
	}
	return x, n
}
