package link

import (
	"bytes"
	"context"
	"errors"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Tables reads the link tables the structural provider stored.
type Tables interface {
	// Table returns the table of fv as the structural unit generation gen
	// selects for fv's file stored it, and false when that unit stored none:
	// a language with no lowering, or a file whose dependence or link pass
	// failed (wire.Done discloses which).
	Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*Table, bool, error)
	// Digest returns the MemberDigest that same unit stored beside the
	// table, without reading or decoding the table, and false when it
	// stored none.
	Digest(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (string, bool, error)
}

// DigestKind is the delta-state kind a structural unit stores its table's
// MemberDigest under, beside the table, so a linking unit's delta reads one
// small artifact per member instead of decoding every table.
const DigestKind = "link.table_digest"

// memberDigestDomain is the hash domain of a member digest.
const memberDigestDomain = "link.member"

// MemberDigest is what a linking unit records for one member
// (model.UnitInput.Digest): the table's Digest, which decides every binding,
// and its Dropped count, which decides the unit's coverage. A unit whose every
// member keeps its digest therefore resolves the same bindings and has the
// same coverage, which is what lets it inherit both.
func MemberDigest(t *Table) (string, error) {
	d, err := t.Digest()
	if err != nil {
		return "", err
	}
	return model.H(memberDigestDomain, d, strconv.Itoa(t.Dropped)), nil
}

// StoreTables reads tables from the store: the structural unit a generation
// selects for a file's scope, then that unit's TableKind or DigestKind state.
func StoreTables(s *sqlite.Store) Tables { return storeTables{s: s} }

type storeTables struct{ s *sqlite.Store }

func (st storeTables) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	buf, ok, err := st.state(ctx, gen, fv, TableKind)
	if err != nil || !ok {
		return nil, false, err
	}
	t, err := DecodeTable(buf)
	if err != nil {
		return nil, false, &model.Error{Code: model.CodeStorageCorrupt, Message: "link table of " + fv.Path + ": " + err.Error()}
	}
	if t.Path != fv.Path {
		return nil, false, &model.Error{Code: model.CodeStorageCorrupt, Message: "the link table stored for " + fv.Path + " names " + t.Path}
	}
	return t, true, nil
}

func (st storeTables) Digest(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (string, bool, error) {
	buf, ok, err := st.state(ctx, gen, fv, DigestKind)
	if err != nil || !ok {
		return "", false, err
	}
	if !model.ValidHexID(string(buf)) {
		return "", false, &model.Error{Code: model.CodeStorageCorrupt, Message: "the link table digest stored for " + fv.Path + " is not a digest"}
	}
	return string(buf), true, nil
}

// state reads the artifact of kind the structural unit gen selects for fv's
// file stored, and false when that unit, or that artifact, is not there.
func (st storeTables) state(ctx context.Context, gen model.GenerationID, fv model.FileVersion, kind string) ([]byte, bool, error) {
	unit, err := st.s.SelectedUnit(ctx, gen, lang.ProviderID, lang.ScopePrefix+fv.Path)
	if notFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var buf bytes.Buffer
	if err := st.s.DeltaState(ctx, unit, kind, &buf); notFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

// notFound reports the store's typed not-found answer.
func notFound(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Details["reason"] == sqlite.ReasonNotFound
}
