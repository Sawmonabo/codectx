package admission

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/bits"
	"slices"
	"sync"
)

// PriorBytesPerSourceByte is the structural prior's slope (ADR-0012 decision
// 5, "First file"): the bytes one source byte adds to a parse, derived from
// the parse-tree runtime's node layout and not measured on any repository.
const PriorBytesPerSourceByte = 107

// The need model's design constants (ADR-0012 decision 5, "The model's design
// constants"). None is fitted to a repository.
const (
	// bucketGrowth is the ratio between one bucket's edges: buckets are 5%
	// wide, which bounds a prediction's rounding to one bucket.
	bucketGrowth = 1.05
	// reservedQuantile is the weighted percentile reserved once the model is
	// warm: it trades overruns against held-back concurrency.
	reservedQuantile = 0.99
	// percentileObservations is where the sample maximum gives way to the
	// percentile: the first count at which a p99 rests on more than one
	// sample.
	percentileObservations = 100
)

// PriorNeed is the structural prior's per-file increment for a file of
// sourceBytes bytes: what a first file of a language never seen in this
// repository reserves above its worker's base. It saturates at the largest
// int64 rather than overflowing, and an empty or negative size needs nothing.
func PriorNeed(sourceBytes int64) int64 {
	if sourceBytes <= 0 {
		return 0
	}
	if sourceBytes > math.MaxInt64/PriorBytesPerSourceByte {
		return math.MaxInt64
	}
	return sourceBytes * PriorBytesPerSourceByte
}

// SizeClassOf is the file-size class a file of sourceBytes bytes is learned
// under: its power of two, the bit length of the size, so class c holds the
// sizes in [2^(c-1), 2^c). The classes are structural and carry no boundary
// constant. An empty file, and a negative size, is class 0.
func SizeClassOf(sourceBytes int64) int {
	if sourceBytes <= 0 {
		return 0
	}
	return bits.Len64(uint64(sourceBytes))
}

// NeedHistogram is the decaying histogram of observed need per source byte
// for one (repository, language, grammar fingerprint, size class).
//
// Ratio bucket i covers [1.05^i, 1.05^(i+1)) bytes of need per source byte, i
// negative below one byte per byte; a file that allocated nothing new has a
// zero bucket of its own, below every ratio bucket. The bucket set is sparse:
// it holds only the buckets observed, and a bucket whose weight decays below
// the smallest representable weight is dropped. It is safe for concurrent use.
// The zero value is an empty model.
type NeedHistogram struct {
	mu sync.Mutex
	// observations counts every file folded in, undecayed: the count the
	// cutover from the sample maximum to the percentile is taken on.
	observations int64
	// zero is the zero bucket's decayed weight.
	zero float64
	// buckets are the ratio buckets' decayed weights by index; every weight
	// held is positive.
	buckets map[int]float64
}

// Observe folds one file's observed need into the histogram: every earlier
// weight is multiplied by 2^(-1/halfLifeFiles), then the file's bucket gains
// weight one. A half-life below one file is one file. An empty or negative
// source size has no need per byte, and a negative need is no measurement;
// neither is folded in.
func (h *NeedHistogram) Observe(needBytes, sourceBytes, halfLifeFiles int64) {
	if sourceBytes <= 0 || needBytes < 0 {
		return
	}
	decay := math.Exp2(-1 / float64(max(halfLifeFiles, 1)))
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.buckets == nil {
		h.buckets = map[int]float64{}
	}
	h.zero *= decay
	for i, w := range h.buckets {
		if w *= decay; w > 0 {
			h.buckets[i] = w
		} else {
			delete(h.buckets, i)
		}
	}
	h.observations++
	if needBytes == 0 {
		h.zero++
		return
	}
	h.buckets[bucketOf(float64(needBytes)/float64(sourceBytes))]++
}

// bucketOf is the ratio bucket holding ratio, which must be positive and
// finite: a need over a source size, both int64, always is. The logarithm
// places it; the edges themselves settle a ratio the logarithm's rounding puts
// one bucket off.
func bucketOf(ratio float64) int {
	i := int(math.Floor(math.Log(ratio) / math.Log(bucketGrowth)))
	for math.Pow(bucketGrowth, float64(i)) > ratio {
		i--
	}
	for math.Pow(bucketGrowth, float64(i+1)) <= ratio {
		i++
	}
	return i
}

// Predict is the need the histogram reserves for a file of sourceBytes bytes,
// and false when it holds no observation. Below 100 observations it is the
// sample maximum, the highest occupied bucket's upper edge; from 100 on it is
// the weighted p99, the lowest upper edge at which the cumulative weight
// reaches 99% of the total. Either ratio is multiplied by the source bytes and
// rounded up, saturating at the largest int64. The zero bucket's upper edge is
// zero, and an empty or negative size needs nothing.
func (h *NeedHistogram) Predict(sourceBytes int64) (int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.observations == 0 {
		return 0, false
	}
	if sourceBytes <= 0 {
		return 0, true
	}
	idx := h.sortedIndices()
	var ratio float64
	switch {
	case h.observations < percentileObservations:
		if n := len(idx); n > 0 {
			ratio = math.Pow(bucketGrowth, float64(idx[n-1]+1))
		}
	default:
		total := h.zero
		for _, i := range idx {
			total += h.buckets[i]
		}
		cum := h.zero
		if cum < reservedQuantile*total {
			for _, i := range idx {
				cum += h.buckets[i]
				if cum >= reservedQuantile*total {
					ratio = math.Pow(bucketGrowth, float64(i+1))
					break
				}
			}
		}
	}
	need := math.Ceil(ratio * float64(sourceBytes))
	if need >= math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(need), true
}

// sortedIndices is the ratio buckets' indices, ascending. The mutex must be
// held.
func (h *NeedHistogram) sortedIndices() []int {
	idx := make([]int, 0, len(h.buckets))
	for i := range h.buckets {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	return idx
}

// needState is the histogram's encoding: the observation count, the zero
// bucket's weight and the ratio buckets ascending by index, so one model
// always encodes to the same bytes.
type needState struct {
	Observations int64        `json:"observations"`
	Zero         float64      `json:"zero"`
	Buckets      []needBucket `json:"buckets"`
}

type needBucket struct {
	Index  int     `json:"index"`
	Weight float64 `json:"weight"`
}

// MarshalBinary encodes the histogram for the run ledger's observation store.
func (h *NeedHistogram) MarshalBinary() ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := needState{Observations: h.observations, Zero: h.zero, Buckets: make([]needBucket, 0, len(h.buckets))}
	for _, i := range h.sortedIndices() {
		st.Buckets = append(st.Buckets, needBucket{Index: i, Weight: h.buckets[i]})
	}
	return json.Marshal(st)
}

// errNeedState is every refusal of an encoding that is not one MarshalBinary
// could have written.
var errNeedState = errors.New("admission: the need model's encoded state is malformed")

// UnmarshalBinary decodes what MarshalBinary encoded, replacing the whole
// histogram. An encoding MarshalBinary could not have written -- unparsable,
// with unknown or trailing content, a negative count or weight, a zero ratio
// weight, indices out of ascending order, or weights without observations or
// observations without weights -- is refused with an error and leaves the
// histogram as it was; it never decodes to an empty model.
func (h *NeedHistogram) UnmarshalBinary(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var st needState
	if err := dec.Decode(&st); err != nil {
		return errors.Join(errNeedState, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return errNeedState
	}
	if st.Observations < 0 || st.Zero < 0 {
		return errNeedState
	}
	buckets := make(map[int]float64, len(st.Buckets))
	for k, b := range st.Buckets {
		if b.Weight <= 0 || k > 0 && b.Index <= st.Buckets[k-1].Index {
			return errNeedState
		}
		buckets[b.Index] = b.Weight
	}
	if (st.Observations == 0) != (st.Zero == 0 && len(buckets) == 0) {
		return errNeedState
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.observations, h.zero, h.buckets = st.Observations, st.Zero, buckets
	return nil
}
