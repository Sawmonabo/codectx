package graph

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestAWalkTornByAPowerLossIsNeverRead pins what lets a walk's state go
// unsynced (routeTable.sync, the level files, the bitsets): after the restart
// a power loss implies, the retained walk a continuation names was written by
// another boot, and resuming it answers CTX_CURSOR_INVALID -- re-run from the
// first page -- however its files were torn. Here every file of the walk is
// zero-filled, as an unsynced file can come back, and its header names another
// boot.
//
// Mutation caught: Spools.live ignoring the header's boot, so the zero-filled
// levels, bitsets and route rows are read as the walk.
func TestAWalkTornByAPowerLossIsNeverRead(t *testing.T) {
	if !pagination.BootKnown() {
		t.Log("this platform has no boot identity; the route table keeps its sync and the walk is not boot-scoped")
		return
	}
	f, chain := newChainFixture(12)
	e, root := chainEngine(t, f, 4, 2)
	req := model.ImpactRequest{GenerationID: 1, Start: chain[:1], Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}}
	res, err := e.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("first leg: %v", err)
	}
	if res.Meta.NextCursor == "" || len(res.Entries) != 0 {
		t.Fatalf("the first leg served %d entries and next %q, want a walk continuation", len(res.Entries),
			res.Meta.NextCursor)
	}
	torn := 0
	for _, name := range spoolEntries(t, root) {
		dir := filepath.Join(root, name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			if filepath.Base(p) == "header" {
				return rewriteBoot(p, "another-boot")
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			torn++
			return os.WriteFile(p, make([]byte, info.Size()), 0o600)
		}); err != nil {
			t.Fatalf("tear %s: %v", dir, err)
		}
	}
	if torn == 0 {
		t.Fatal("the walk continuation retained no file to tear; the test proves nothing")
	}
	req = model.ImpactRequest{Start: chain[:1], Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}, Page: model.PageRequest{Cursor: res.Meta.NextCursor}}
	_, err = e.Impact(context.Background(), req)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeCursorInvalid {
		t.Fatalf("resuming a walk another boot wrote and a power loss tore answered %v, want %s",
			err, model.CodeCursorInvalid)
	}
}

// rewriteBoot rewrites the header frame at path as boot would have written it.
func rewriteBoot(path, boot string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(raw[:4])
	var h map[string]any
	if err := json.Unmarshal(raw[4:4+n], &h); err != nil {
		return err
	}
	h["boot"] = boot
	head, err := json.Marshal(h)
	if err != nil {
		return err
	}
	frame := binary.BigEndian.AppendUint32(nil, uint32(len(head)))
	return os.WriteFile(path, append(append(frame, head...), raw[4+n:]...), 0o600)
}
