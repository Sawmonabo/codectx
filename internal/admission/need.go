package admission

// PriorBytesPerSourceByte is the structural prior's slope (ADR-0012 decision
// 5, "First file"): the bytes one source byte adds to a parse, derived from
// the parse-tree runtime's node layout and not measured on any repository.
const PriorBytesPerSourceByte = 107

// PriorNeed is the structural prior's per-file increment for a file of
// sourceBytes bytes: what a first file of a language never seen in this
// repository reserves above its worker's base.
func PriorNeed(sourceBytes int64) int64 {
	panic("admission: PriorNeed is not built yet")
}

// SizeClassOf is the file-size class a file of sourceBytes bytes is learned
// under.
func SizeClassOf(sourceBytes int64) int {
	panic("admission: SizeClassOf is not built yet")
}

// NeedHistogram is the decaying histogram of observed need per source byte
// for one (repository, language, grammar fingerprint, size class).
type NeedHistogram struct{}

// Observe folds one file's observed need into the histogram, decaying every
// earlier observation by a half-life of halfLifeFiles files.
func (h *NeedHistogram) Observe(needBytes, sourceBytes, halfLifeFiles int64) {
	panic("admission: Observe is not built yet")
}

// Predict is the need the histogram reserves for a file of sourceBytes bytes,
// and false when it holds no observation.
func (h *NeedHistogram) Predict(sourceBytes int64) (int64, bool) {
	panic("admission: Predict is not built yet")
}

// MarshalBinary encodes the histogram for the run ledger's observation store.
func (h *NeedHistogram) MarshalBinary() ([]byte, error) {
	panic("admission: MarshalBinary is not built yet")
}

// UnmarshalBinary decodes what MarshalBinary encoded.
func (h *NeedHistogram) UnmarshalBinary(data []byte) error {
	panic("admission: UnmarshalBinary is not built yet")
}
