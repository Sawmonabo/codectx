package textindex

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The Go reference of the library's scorer (docs/adr/ADR-0013-search-index.md
// decision 2). Every product is wrapped in an explicit float64 conversion so
// the compiler cannot fuse a multiply-add on the platforms where the Go
// specification permits it; the library never fuses.

// refLog is the fdlibm logarithm exactly as Go's portable math.Log writes it,
// the one implementation both sides use.
func refLog(x float64) float64 {
	const (
		ln2Hi = 6.93147180369123816490e-01
		ln2Lo = 1.90821492927058770002e-10
		l1    = 6.666666666666735130e-01
		l2    = 3.999999999940941908e-01
		l3    = 2.857142874366239149e-01
		l4    = 2.222219843214978396e-01
		l5    = 1.818357216161805012e-01
		l6    = 1.531383769920937332e-01
		l7    = 1.479819860511658591e-01
	)
	switch {
	case math.IsNaN(x) || math.IsInf(x, 1):
		return x
	case x < 0:
		return math.NaN()
	case x == 0:
		return math.Inf(-1)
	}
	f1, ki := math.Frexp(x)
	if f1 < math.Sqrt2/2 {
		f1 = float64(f1 * 2)
		ki--
	}
	f := f1 - 1
	k := float64(ki)
	s := f / (2 + f)
	s2 := float64(s * s)
	s4 := float64(s2 * s2)
	t1 := float64(s2 * (l1 + float64(s4*(l3+float64(s4*(l5+float64(s4*l7)))))))
	t2 := float64(s4 * (l2 + float64(s4*(l4+float64(s4*l6)))))
	r := t1 + t2
	hfsq := float64(float64(0.5*f) * f)
	return float64(k*ln2Hi) - ((hfsq - (float64(s*(hfsq+r)) + float64(k*ln2Lo))) - f)
}

func refIDF(n, df uint64) float64 {
	if n == 0 {
		return 0
	}
	df = min(df, n)
	return refLog(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

func refSaturation(wtf float64, dl uint64, avgdl float64) float64 {
	if wtf <= 0 || avgdl <= 0 {
		return 0
	}
	return float64(wtf*2.2) / (wtf + float64(1.2*(0.25+float64(0.75*float64(dl))/avgdl)))
}

func refQuantize(score float64) int64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return int64(math.Round(float64(score * 1e6)))
}

// TestScoreVectorMatchesGoReferenceBitForBit checks the library's arithmetic
// against the Go reference on a wide sweep: every df for small corpora, a
// dense low-df band and a stride for corpora up to a million documents, and
// varied frequencies and lengths. It is the input region where Go's math.Log
// and the platform logarithm disagree in the last bit on about 1% of inputs.
//
// Mutation caught: the library using the platform logarithm (an unpinned log)
// instead of the pinned one.
func TestScoreVectorMatchesGoReferenceBitForBit(t *testing.T) {
	wtfs := []float64{1, 2, 3, 5, 7, 12, 25, 101, 1e4}
	dls := []uint64{0, 1, 3, 17, 64, 250, 4096, 1 << 20}
	avgs := []float64{1, 7.25, 31.5, 123.456, 1e3 / 3}
	var in []scoreInput
	row := 0
	add := func(n, df uint64) {
		in = append(in, scoreInput{Docs: n, DF: df, WTF: wtfs[row%len(wtfs)], DL: dls[(row/3)%len(dls)], AvgDL: avgs[(row/7)%len(avgs)]})
		row++
	}
	for _, n := range []uint64{1, 2, 3, 10, 99, 100, 1000, 1999} {
		for df := uint64(0); df <= n; df++ {
			add(n, df)
		}
	}
	for _, n := range []uint64{12345, 99991, 100000, 546613, 999983, 1000000} {
		for df := uint64(0); df <= 5000; df++ {
			add(n, df)
		}
		for df := uint64(5001); df <= n; df += 997 {
			add(n, df)
		}
		add(n, n)
	}
	out, err := scoreVector(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("%d outputs for %d inputs", len(out), len(in))
	}
	bad := 0
	for i, v := range in {
		idf := refIDF(v.Docs, v.DF)
		score := float64(idf * refSaturation(v.WTF, v.DL, v.AvgDL))
		micros := refQuantize(score)
		got := out[i]
		if math.Float64bits(got.IDF) != math.Float64bits(idf) || math.Float64bits(got.Score) != math.Float64bits(score) || got.Micros != micros {
			if bad < 10 {
				t.Errorf("input %+v: library idf %v score %v micros %d, reference %v %v %d", v, got.IDF, got.Score, got.Micros, idf, score, micros)
			}
			bad++
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d inputs disagree", bad, len(in))
	}
}

// refDoc is one document of the fixed ASCII corpus, by column.
type refDoc struct {
	doc  Document
	cols [5][]string
}

func asciiTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
}

// TestPageScoresMatchGoReference indexes a fixed corpus of lowercase ASCII
// words (so tokenization is plain word splitting, with no identifier parts)
// and checks every hit's quantized score against the Go reference of BM25F
// computed from the corpus directly, under both length rules.
//
// Mutation caught: an unpinned log in the library's scorer (and any drift of
// the library's df, N, average length or weighted frequency from the formula).
func TestPageScoresMatchGoReference(t *testing.T) {
	vocab := []string{"alpha", "beta", "gamma", "delta", "omega", "sigma", "kappa", "lambda", "theta", "zeta", "rho", "tau"}
	var docs []refDoc
	for i := 0; i < 400; i++ {
		w := func(k int) string { return vocab[(i*7+k*k*3+i/5)%len(vocab)] }
		name := w(1)
		body := make([]string, 0, 30)
		for k := 0; k < 3+(i*13)%27; k++ {
			body = append(body, w(k+2))
		}
		d := Document{
			DocID:         uint64(i + 1),
			SearchKey:     fmt.Sprintf("k%04d", i),
			NodeID:        model.NodeID(fmt.Sprintf("n%04d", i)),
			FileID:        model.FileID(fmt.Sprintf("f%03d", i/4)),
			Path:          fmt.Sprintf("dir%d/%s%d.go", i%5, w(9), i/4),
			Kind:          model.NodeFunction,
			Name:          name,
			QualifiedName: "pkg." + name + " " + w(4),
			Signature:     "func " + name + "(" + w(5) + " int)",
			Bytes:         model.ByteRange{Start: uint64(i * 10), End: uint64(i*10 + 9)},
			Body:          strings.Join(body, " "),
		}
		docs = append(docs, refDoc{doc: d, cols: [5][]string{
			asciiTokens(d.Name), asciiTokens(d.QualifiedName), asciiTokens(d.Signature), asciiTokens(d.Path), asciiTokens(d.Body),
		}})
	}
	dir := t.TempDir()
	x := openWritable(t, dir, 1)
	ctx := context.Background()
	for u := 0; u < 4; u++ {
		var batch []Document
		for _, d := range docs[u*100 : (u+1)*100] {
			batch = append(batch, d.doc)
		}
		if err := x.AddUnit(ctx, fmt.Sprintf("unit%d", u), batch); err != nil {
			t.Fatal(err)
		}
		if err := x.Commit(ctx, fmt.Sprint(u+1)); err != nil {
			t.Fatal(err)
		}
	}
	all := visibleOf(1, 400)
	v, err := x.OpenView(1, all)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()

	weights := [5]uint64{5, 5, 2, 2, 1}
	for _, rule := range []LengthRule{LengthTotal, LengthWeighted} {
		dl := func(d refDoc) uint64 {
			var n uint64
			for c, toks := range d.cols {
				if rule == LengthWeighted {
					n += weights[c] * uint64(len(toks))
				} else {
					n += uint64(len(toks))
				}
			}
			return n
		}
		var sum uint64
		for _, d := range docs {
			sum += dl(d)
		}
		avgdl := float64(sum) / float64(len(docs))
		for _, q := range []string{"alpha", "gamma delta", "zeta rho tau", "omega sigma kappa lambda", "theta pkg func", "dir3 int"} {
			terms := asciiTokens(q)
			df := make([]uint64, len(terms))
			wtf := make([][]float64, len(docs))
			for i, d := range docs {
				wtf[i] = make([]float64, len(terms))
				for j, term := range terms {
					for c, toks := range d.cols {
						for _, tok := range toks {
							if tok == term {
								wtf[i][j] += float64(weights[c])
							}
						}
					}
					if wtf[i][j] > 0 {
						df[j]++
					}
				}
			}
			want := map[string]int64{}
			for i, d := range docs {
				var score float64
				matched := false
				for j := range terms {
					if wtf[i][j] > 0 {
						matched = true
						score += float64(refIDF(uint64(len(docs)), df[j]) * refSaturation(wtf[i][j], dl(d), avgdl))
					}
				}
				if matched {
					want[d.doc.SearchKey] = refQuantize(score)
				}
			}
			page, err := v.Search(ctx, Request{Query: q, Depth: len(docs) + 1, LengthRule: rule})
			if err != nil {
				t.Fatal(err)
			}
			if page.Terms != len(terms) || page.More || len(page.Hits) != len(want) {
				t.Fatalf("rule %d query %q: terms %d more %v hits %d, want terms %d, %d hits", rule, q, page.Terms, page.More, len(page.Hits), len(terms), len(want))
			}
			for _, h := range page.Hits {
				if w, ok := want[h.SearchKey]; !ok || w != h.ScoreMicros {
					t.Fatalf("rule %d query %q hit %s: library %d micros, reference %d (present %v)", rule, q, h.SearchKey, h.ScoreMicros, w, ok)
				}
			}
		}
	}
}
