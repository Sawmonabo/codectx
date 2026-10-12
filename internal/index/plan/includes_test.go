package plan

import (
	"bytes"
	"context"
	"io"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
)

// filesView is a snapshot view over a map of path to content: the reads a
// planner makes of a pinned snapshot, and the refusal a path it does not hold
// gets.
type filesView map[string]string

const viewRepo model.RepositoryID = "repo"

func (v filesView) Header() model.Snapshot { return model.Snapshot{RepositoryID: viewRepo} }

func (v filesView) EachFile(context.Context, model.FileSelection, func(model.FileVersion) error) error {
	return nil
}

func (v filesView) version(path string) model.FileVersion {
	return model.FileVersion{ID: model.NewFileID(viewRepo, path), Path: path,
		ContentHash: string(model.NewFileID("content", v[path])), Size: int64(len(v[path]))}
}

func (v filesView) Open(_ context.Context, id model.FileID) (io.ReadCloser, model.FileVersion, error) {
	for path, content := range v {
		if model.NewFileID(viewRepo, path) == id {
			return io.NopCloser(bytes.NewReader([]byte(content))), v.version(path), nil
		}
	}
	return nil, model.FileVersion{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "not in the snapshot"}
}

func (v filesView) ReadRange(context.Context, model.FileID, model.ByteRange) ([]byte, model.FileVersion, error) {
	return nil, model.FileVersion{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "not read here"}
}

// TestARequirementsUnitIsKeyedByTheFilesItIncludes protects the refresh of a
// requirements file whose included file changed.
//
// The unit publishes the packages its `-r` includes declare, so an edit to an
// included file alone must move the including unit's identity, or the plan
// reuses it with the old packages. Its inputs are its own file and every file
// it includes transitively; an include the snapshot does not hold, or one
// that names no workspace file, is not an input; and an include cycle ends
// with each of its files an input once. The identity is taken after the
// unit's spill round trip, the form the executor derives it from.
//
// Mutation proof: plan the manifest unit over its own file only (drop the
// manifestInputs call in fileUnits, or the Plan.includes lookup in
// unitSequence/unit) and the edit to base.txt keeps the identity and the
// input count is 1; follow no visited set in manifest.Includes and the cycle
// never ends.
func TestARequirementsUnitIsKeyedByTheFilesItIncludes(t *testing.T) {
	plan := func(view filesView) (model.UnitID, []model.FileID) {
		t.Helper()
		sorter, err := pagination.NewExternalSort(t.TempDir(), 0, encodeUnit, decodeUnit, compareUnit)
		if err != nil {
			t.Fatalf("NewExternalSort: %v", err)
		}
		defer func() { _ = sorter.Close() }()
		b := &builder{in: Inputs{View: view}, allUnits: sorter, providerOrder: map[string]int{},
			plan: Plan{Reuse: map[string]model.UnitID{}, Unplanned: map[string]int{},
				includes: map[int64][]model.UnitInput{}},
			fileProviders: []fileProvider{{id: manifest.ID, version: "1", gate: func(model.FileVersion) bool { return true }}}}
		if err := b.fileUnits(context.Background(), view.version("requirements.txt")); err != nil {
			t.Fatalf("fileUnits: %v", err)
		}
		run, err := sorter.Sorted()
		if err != nil {
			t.Fatalf("Sorted: %v", err)
		}
		defer func() { _ = run.Close() }()
		var id model.UnitID
		var inputs []model.FileID
		if err := run.Each(func(rec fileUnitRecord) error {
			u := rec.unit(b.plan.includes[rec.Seq])
			if err := u.Inputs(func(in model.UnitInput) error { inputs = append(inputs, in.FileID); return nil }); err != nil {
				return err
			}
			if int64(len(inputs)) != u.InputCount {
				t.Fatalf("the unit declares %d inputs and counts %d", len(inputs), u.InputCount)
			}
			spec, err := u.Spec("config")
			id = spec.ID
			return err
		}); err != nil {
			t.Fatalf("deriving the requirements unit: %v", err)
		}
		return id, inputs
	}
	idsOf := func(paths ...string) []model.FileID {
		ids := make([]model.FileID, 0, len(paths))
		for _, p := range paths {
			ids = append(ids, model.NewFileID(viewRepo, p))
		}
		slices.SortFunc(ids, compareID)
		return ids
	}

	root := "-r base.txt\n-r missing.txt\n-r https://example.invalid/r.txt\nflask\n"
	before, inputs := plan(filesView{"requirements.txt": root, "base.txt": "-r nested/more.txt\nrequests\n",
		"nested/more.txt": "click\n"})
	if want := idsOf("requirements.txt", "base.txt", "nested/more.txt"); !slices.Equal(inputs, want) {
		t.Fatalf("the requirements unit declares %v, want its own file and both included files %v", inputs, want)
	}
	after, _ := plan(filesView{"requirements.txt": root, "base.txt": "-r nested/more.txt\nrequests\n",
		"nested/more.txt": "click==8\n"})
	if before == after {
		t.Fatal("an edit to a file the requirements file includes kept the including unit's identity: " +
			"the plan would reuse it with the packages the file no longer declares")
	}

	_, inputs = plan(filesView{"requirements.txt": "-r b.txt\nflask\n", "b.txt": "-r requirements.txt\n-c requirements.txt\n"})
	if want := idsOf("requirements.txt", "b.txt"); !slices.Equal(inputs, want) {
		t.Fatalf("the include cycle declares %v, want each of its files once %v", inputs, want)
	}
}
