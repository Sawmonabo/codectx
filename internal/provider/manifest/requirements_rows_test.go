package manifest_test

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestIncludedLinesAreLocatedInTheirOwnFile pins that an uninterpreted line
// of an included requirements file is a row located in that file, which is
// one of the unit's inputs, and that two identical such lines are two rows.
// Mutation caught: publishing an included line with no range (the two
// identical lines then share one evidence identity without their ordinals and
// collapse), or locating it in the unit's own file at offsets that are not
// its own.
func TestIncludedLinesAreLocatedInTheirOwnFile(t *testing.T) {
	const own, inc = "requirements.txt", "extra/more.txt"
	files := map[string]string{
		own: "-r extra/more.txt\nrequests>=2\n",
		inc: "--find-links https://mirror.example/a\n--find-links https://mirror.example/a\n",
	}
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	h := providertest.New(t, files)
	u := h.Plan(t, mf, filesystem.ScopeKey(own), []string{own, inc})
	cap := &capture{}
	out := h.Begin(t, u, []string{own, inc})
	result, err := provider.RunUnit(context.Background(), mf, u.Request, &captureOutput{UnitOutput: out, c: cap}, providertest.Limits, h.Pool)
	if err != nil || result.State != model.RunSucceeded {
		t.Fatalf("RunUnit: %v (%s)", err, result.State)
	}
	incFile := h.File(t, inc)
	var located []uint64
	for _, evs := range cap.nodeEvidence {
		for _, ev := range evs {
			r, ok := model.ParseMetadataRow(ev.Detail)
			if !ok || r.Field != model.MetaUnresolvedLines {
				continue
			}
			if ev.FileID != incFile.ID || ev.Range == nil {
				t.Fatalf("row %d is in file %s with range %v, want located in %s", r.Ordinal, ev.FileID, ev.Range, inc)
			}
			located = append(located, ev.Range.Start.Byte)
		}
	}
	if len(located) != 2 || located[0] == located[1] {
		t.Fatalf("the two identical included lines are rows at %v, want two rows at two offsets", located)
	}
}
