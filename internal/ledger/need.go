package ledger

import "context"

// NeedKey names one learned need model within a repository: the language, the
// grammar fingerprint the files were parsed under, and the file-size class.
type NeedKey struct {
	Language    string
	Fingerprint string
	SizeClass   int
}

// NeedObservation is one parsed file's measured need, with the model state
// after it was folded in. State is the encoded model, opaque to this package.
type NeedObservation struct {
	Key NeedKey
	// ReservedBytes is the per-file increment the file was admitted with, and
	// NeedBytes what the worker measured it to use above its base.
	ReservedBytes int64
	NeedBytes     int64
	State         []byte
}

// RepositoryID is the repository this run records, as the hex the ledger's
// readers take.
func (r *Run) RepositoryID() string {
	panic("ledger: RepositoryID is not built yet")
}

// ObserveNeed records one file's observation. The model state it carries is
// written to the observation store in the same transaction as the
// observation, whatever becomes of the generation it was taken under.
func (r *Run) ObserveNeed(o NeedObservation) {
	panic("ledger: ObserveNeed is not built yet")
}

// NeedModel is the persisted model state for key in this run's repository,
// and false when none is recorded.
func (r *Run) NeedModel(ctx context.Context, key NeedKey) ([]byte, bool, error) {
	panic("ledger: NeedModel is not built yet")
}
