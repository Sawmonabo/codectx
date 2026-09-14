package delta

import (
	"bufio"
	"io"
	"os"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The unit input manifest is a delta-state artifact of the dependence applier:
// the file identities and content hashes one sealed unit declared, so the next
// refresh can name the buckets that did not survive.
//
// Storage records the same rows in unit_inputs but exposes no reader for them,
// and the only other source — the previous generation's snapshot — is not
// reachable from a unit id. Keeping the manifest with the unit it describes is
// what makes the dependence carry-over computable at all; Store.UnitInputs
// would replace it (see the lane report).
//
// It is a file, never a Go slice: a unit declares every source file of its
// project and Section 6 forbids retaining that list in the heap. It is written
// while storage streams the inputs it is already reading, and it is read back
// as one merge join against the fresh one — the same shape as the SCIP
// document manifest and the dependence key set.
const (
	inputManifestHeader = "codectx-unit-inputs v1"
	inputManifestSep    = "  "
)

// teeInputs wraps an input stream so the manifest is written as storage
// consumes it. The rows are written in the order they arrive, which
// UnitWriter.streamInputs requires to be ascending by FileID and refuses
// otherwise, so the file is sorted or the unit never opens.
func teeInputs(path string, inputs func(yield func(model.UnitInput) error) error) func(yield func(model.UnitInput) error) error {
	return func(yield func(model.UnitInput) error) error {
		f, err := os.Create(path)
		if err != nil {
			return internal("unit input manifest: " + err.Error())
		}
		defer f.Close()
		w := bufio.NewWriter(f)
		if _, err := w.WriteString(inputManifestHeader + "\n"); err != nil {
			return internal("unit input manifest: " + err.Error())
		}
		err = inputs(func(in model.UnitInput) error {
			if _, err := w.WriteString(string(in.FileID) + inputManifestSep + in.ContentHash + "\n"); err != nil {
				return internal("unit input manifest: " + err.Error())
			}
			return yield(in)
		})
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return internal("unit input manifest: " + err.Error())
		}
		if err := f.Sync(); err != nil {
			return internal("unit input manifest: " + err.Error())
		}
		return nil
	}
}

// diffInputs names every file the predecessor declared that this unit does not
// declare with the same bytes: the changed ones and the ones that are gone. A
// file only the fresh manifest holds is an addition and names nothing of the
// predecessor, so it is not in the result.
//
// The result is a slice because sqlite.Replaced takes one, and it is bounded
// by the number of files that actually changed rather than by the repository.
func diffInputs(prev, fresh string) ([]model.FileID, error) {
	old, err := openInputRows(prev)
	if err != nil {
		return nil, err
	}
	defer old.close()
	now, err := openInputRows(fresh)
	if err != nil {
		return nil, err
	}
	defer now.close()

	var out []model.FileID
	a, aok, err := old.next()
	if err != nil {
		return nil, err
	}
	b, bok, err := now.next()
	if err != nil {
		return nil, err
	}
	for aok {
		switch {
		case !bok || a.file < b.file:
			// The predecessor declared it and this unit does not.
			out = append(out, model.FileID(a.file))
			a, aok, err = old.next()
		case b.file < a.file:
			b, bok, err = now.next()
		default:
			if a.hash != b.hash {
				out = append(out, model.FileID(a.file))
			}
			if a, aok, err = old.next(); err == nil {
				b, bok, err = now.next()
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

type inputRow struct{ file, hash string }

type inputRows struct {
	f    *os.File
	sc   *bufio.Scanner
	last string
}

func openInputRows(path string) (*inputRows, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, internal("unit input manifest: " + err.Error())
	}
	sc := inputScanner(f)
	if !sc.Scan() || sc.Text() != inputManifestHeader {
		f.Close()
		if err := sc.Err(); err != nil {
			return nil, internal("unit input manifest: " + err.Error())
		}
		return nil, invalid("unit input manifest " + path + " does not start with " + inputManifestHeader)
	}
	return &inputRows{f: f, sc: sc}, nil
}

// inputScanner bounds a row at two hex digests and the separator, so a corrupt
// manifest cannot size an allocation.
func inputScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256), 2*model.IDHexLen+len(inputManifestSep))
	return sc
}

// next reads one row and validates it. The rows are required to ascend so the
// merge join above is a join and not a silent mismatch.
func (r *inputRows) next() (inputRow, bool, error) {
	if !r.sc.Scan() {
		if err := r.sc.Err(); err != nil {
			return inputRow{}, false, internal("unit input manifest: " + err.Error())
		}
		return inputRow{}, false, nil
	}
	file, hash, ok := strings.Cut(r.sc.Text(), inputManifestSep)
	if !ok || !model.ValidHexID(file) || !model.ValidHexID(hash) {
		return inputRow{}, false, invalid("a unit input manifest row is not a file id and a content hash")
	}
	if file <= r.last {
		return inputRow{}, false, invalid("a unit input manifest is not sorted by file id")
	}
	r.last = file
	return inputRow{file: file, hash: hash}, true, nil
}

func (r *inputRows) close() {
	if r.f != nil {
		r.f.Close()
	}
}
