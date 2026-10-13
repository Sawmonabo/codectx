package context

import (
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// fillRecord sets every field of the struct at v to a value drawn from r:
// integers across their whole range including the extremes, strings of
// arbitrary bytes (NUL and invalid UTF-8 among them, which a text codec would
// rewrite), string lists of zero to three elements (zero as nil, the one
// normalization the layout makes) and nested structs filled the same way.
func fillRecord(r *rand.Rand, v reflect.Value) {
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(r.IntN(2) == 1)
		case reflect.Int, reflect.Int8, reflect.Int32, reflect.Int64:
			bits := f.Type().Bits()
			lo, hi := int64(-1)<<(bits-1), int64(math.MaxInt64)>>(64-bits)
			switch r.IntN(4) {
			case 0:
				f.SetInt(lo)
			case 1:
				f.SetInt(hi)
			case 2:
				f.SetInt(0)
			default:
				f.SetInt(r.Int64N(hi) - r.Int64N(hi))
			}
		case reflect.String:
			f.SetString(randomBytes(r))
		case reflect.Slice:
			n := r.IntN(4)
			if n == 0 {
				f.Set(reflect.Zero(f.Type()))
				continue
			}
			list := reflect.MakeSlice(f.Type(), n, n)
			for j := 0; j < n; j++ {
				list.Index(j).SetString(randomBytes(r))
			}
			f.Set(list)
		case reflect.Struct:
			fillRecord(r, f)
		}
	}
}

func randomBytes(r *rand.Rand) string {
	n := r.IntN(300) // past 127, so a length takes a two-byte varint
	if r.IntN(3) == 0 {
		n = 0
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return string(b)
}

// TestRecordCodecRoundTripsEveryRecordExactly is the exactness proof of the
// sort record layout: for every record type the compile sorts, any value
// decodes to itself, field for field, and encodes to the same bytes twice;
// every strict prefix of an encoding and an encoding with a byte appended are
// refused rather than read as a record.
//
// Mutation caught: a field the layout skips or reads at the wrong width, a
// length or count read without its bound, a bool byte read as anything but 0
// or 1, or a decode that accepts trailing bytes.
func TestRecordCodecRoundTripsEveryRecordExactly(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	checkCodec[candRec](t, r)
	checkCodec[hopRec](t, r)
	checkCodec[nodeRec](t, r)
	checkCodec[pathRec](t, r)
	checkCodec[reasonRec](t, r)
	checkCodec[relAttrRec](t, r)
	checkCodec[pkgEdgeRec](t, r)
	checkCodec[pkgCountRec](t, r)
	checkCodec[groupRec](t, r)
	checkCodec[decisionRec](t, r)
	checkCodec[sizedRec](t, r)
	checkCodec[keepRec](t, r)
	checkCodec[dropRec](t, r)
	checkCodec[scoredRec](t, r)
	checkCodec[idRec](t, r)
	checkCodec[model.ExcludedContextEntry](t, r)
}

func checkCodec[T any](t *testing.T, r *rand.Rand) {
	t.Helper()
	encode, decode, err := recordCodecFuncs[T]()
	if err != nil {
		t.Fatalf("%T: %v", *new(T), err)
	}
	for i := 0; i < 200; i++ {
		var want T
		fillRecord(r, reflect.ValueOf(&want).Elem())
		b, err := encode(want)
		if err != nil {
			t.Fatalf("%T: encode: %v", want, err)
		}
		got, err := decode(b)
		if err != nil {
			t.Fatalf("%T: decode: %v", want, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%T: round trip changed the record\n got: %+v\nwant: %+v", want, got, want)
		}
		again, _ := encode(want)
		if string(again) != string(b) {
			t.Fatalf("%T: encoding is not deterministic", want)
		}
		for cut := 0; cut < len(b); cut++ {
			if _, err := decode(b[:cut]); err == nil {
				t.Fatalf("%T: a %d-byte prefix of a %d-byte record decoded", want, cut, len(b))
			}
		}
		if _, err := decode(append(b[:len(b):len(b)], 0)); err == nil {
			t.Fatalf("%T: a record with a trailing byte decoded", want)
		}
	}
}

// TestRestoreRefusesARunOfAnotherLayout guards the continuation fence. A
// checkpointed run is binary and names no fields, so a build whose record
// struct changed would read it as wrong values; the run's layout digest must
// match the restoring build's, or the continuation is expired.
//
// Mutation caught: dropping the layout comparison in restoreSort, which
// adopts the run and fails here.
func TestRestoreRefusesARunOfAnotherLayout(t *testing.T) {
	s := openSorts(t, config.Config{})
	dir := t.TempDir()
	runs, err := checkpointStream(dir, "fence", 0, func(yield func(nodeRec) error) error {
		return yield(nodeRec{NodeID: "n1", Seq: 1})
	}, lessNodeID, sizeOfNode)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if len(runs) == 0 {
		t.Fatalf("the checkpoint wrote no run")
	}
	if _, err := restoreSort(s, dir, "fence", runs, lessNodeID, sizeOfNode); err != nil {
		t.Fatalf("restore under the writing layout: %v", err)
	}
	runs[0].Layout = "another build"
	_, err = restoreSort(s, dir, "fence", runs, lessNodeID, sizeOfNode)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCursorInvalid {
		t.Fatalf("a run of another layout must read as an expired continuation, got %v", err)
	}
}
