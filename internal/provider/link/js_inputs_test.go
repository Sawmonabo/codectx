package link

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// mapView is a snapshot of the given files that counts its whole-manifest
// walks and each file's opens.
type mapView struct {
	files  []model.FileVersion
	data   map[model.FileID]string
	walks  int
	opened map[string]int
}

func newMapView(files map[string]string) *mapView {
	v := &mapView{data: map[model.FileID]string{}, opened: map[string]int{}}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for i, p := range paths {
		id := model.FileID("f" + strconv.Itoa(i))
		v.files = append(v.files, model.FileVersion{ID: id, Path: p, Size: int64(len(files[p]))})
		v.data[id] = files[p]
	}
	return v
}

func (v *mapView) Header() model.Snapshot { return model.Snapshot{} }

func (v *mapView) EachFile(_ context.Context, sel model.FileSelection, yield func(model.FileVersion) error) error {
	if len(sel.Paths) == 0 {
		v.walks++
	}
	for _, fv := range v.files {
		if len(sel.Paths) > 0 && !slices.Contains(sel.Paths, fv.Path) {
			continue
		}
		if err := yield(fv); err != nil {
			return err
		}
	}
	return nil
}

func (v *mapView) Open(_ context.Context, id model.FileID) (io.ReadCloser, model.FileVersion, error) {
	for _, fv := range v.files {
		if fv.ID == id {
			v.opened[fv.Path]++
			return io.NopCloser(strings.NewReader(v.data[id])), fv, nil
		}
	}
	return nil, model.FileVersion{}, errors.New("no such file")
}

func (v *mapView) ReadRange(context.Context, model.FileID, model.ByteRange) ([]byte, model.FileVersion, error) {
	return nil, model.FileVersion{}, errors.New("not read by these tests")
}

// jsUnit is a JavaScript unit at the repository root over view, whose
// members are its ECMAScript source files.
func jsUnit(t *testing.T, view *mapView) *unit {
	t.Helper()
	return &unit{family: FamilyJavaScript, content: view, dir: t.TempDir(),
		members: func(yield func(model.FileVersion) error) error {
			for _, fv := range view.files {
				if stripExtension(fv.Path) != fv.Path {
					if err := yield(fv); err != nil {
						return err
					}
				}
			}
			return nil
		}}
}

// TestOneConfigReadPerLink pins that an ECMAScript unit's configuration is
// read once per link: the manifest fields and the hierarchy's resolver share
// one walk of the manifest and one read of each config file.
func TestOneConfigReadPerLink(t *testing.T) {
	view := newMapView(map[string]string{
		"package.json":            `{"name": "app"}`,
		"tsconfig.json":           `{"compilerOptions": {"moduleResolution": "bundler"}}`,
		"src/a.ts":                "export const a = 1\n",
		"packages/p/package.json": `{"name": "p"}`,
	})
	u := jsUnit(t, view)
	ctx := context.Background()
	if _, err := manifestFields(ctx, u); err != nil {
		t.Fatal(err)
	}
	r, err := newESResolver(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if view.walks != 1 || view.opened["tsconfig.json"] != 1 || view.opened["src/a.ts"] != 0 {
		// Mutation: the resolver reads the configuration again (no
		// esInputsOf memo) -> two walks and tsconfig.json read twice.
		t.Fatalf("walks = %d, opens = %v; want 1 walk and tsconfig.json read once", view.walks, view.opened)
	}
	if u.es != nil {
		// Mutation: the unit keeps the configuration after handing it to
		// the resolver -> it outlives the hierarchy.
		t.Fatal("the unit still holds the configuration the resolver took")
	}
}
