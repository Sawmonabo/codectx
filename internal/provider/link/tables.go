package link

import (
	"bytes"
	"context"
	"errors"

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
}

// StoreTables reads tables from the store: the structural unit a generation
// selects for a file's scope, then that unit's TableKind state.
func StoreTables(s *sqlite.Store) Tables { return storeTables{s: s} }

type storeTables struct{ s *sqlite.Store }

func (st storeTables) Table(ctx context.Context, gen model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	unit, err := st.s.SelectedUnit(ctx, gen, lang.ProviderID, lang.ScopePrefix+fv.Path)
	if notFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var buf bytes.Buffer
	if err := st.s.DeltaState(ctx, unit, TableKind, &buf); notFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	t, err := DecodeTable(buf.Bytes())
	if err != nil {
		return nil, false, &model.Error{Code: model.CodeStorageCorrupt, Message: "link table of " + fv.Path + ": " + err.Error()}
	}
	if t.Path != fv.Path {
		return nil, false, &model.Error{Code: model.CodeStorageCorrupt, Message: "the link table stored for " + fv.Path + " names " + t.Path}
	}
	return t, true, nil
}

// notFound reports the store's typed not-found answer.
func notFound(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Details["reason"] == sqlite.ReasonNotFound
}
