package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The pinned reader is the generation's parses a search tagger binds to.
var _ lang.Parses = (*store.PinnedReader)(nil)

// putModule publishes the module node a structural parse of ff mints, in
// language.
func (f *fixture) putModule(w *store.UnitWriter, run model.ProviderRunID, ff fileFixture, language string) {
	f.t.Helper()
	fact := f.nodeFact(w, run, "file:"+ff.path, "module", &ff)
	canonical := model.CanonicalNodeKey("file:"+ff.path, "module:"+ff.path)
	id := model.NewNodeID(f.repo, model.NodeModule, canonical)
	fact.CanonicalKey = canonical
	fact.Node.ID, fact.Node.Kind, fact.Node.Language = id, model.NodeModule, language
	fact.Node.Name, fact.Node.QualifiedName = filepath.Base(ff.path), ff.path
	fact.Evidence[0].NodeID = id
	fact.Evidence[0].ID = model.NewEvidenceID(fact.Evidence[0])
	if err := w.PutNodes(f.ctx, []model.NodeFact{fact}); err != nil {
		f.t.Fatalf("PutNodes(module %s): %v", ff.path, err)
	}
}

// TestParsedLanguageIsEachGenerationsOwn pins that a header's parsed language
// is read from the pinned generation's own unit, and that a header the
// generation holds no parse of answers not-found rather than a language.
//
// Mutations it catches: dropping the generation from the lookup (the older
// generation then answers the newer one's language, or the reverse); reading
// the language from anything but the unit the generation selected for the
// path's scope; answering found with an empty language for a path no unit
// parsed.
func TestParsedLanguageIsEachGenerationsOwn(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	h := f.file("inc/a.h", "struct S { int x; };\n")
	other := f.file("inc/b.h", "int g(void);\n")
	snap := f.snapshot("headers", h, other)

	gens := make([]model.GenerationID, 0, 2)
	for i, language := range []string{"c", "cpp"} {
		gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
		if err != nil {
			t.Fatal(err)
		}
		run := f.run(gen)
		w := f.beginScopeKey(gen, run, "cfg-"+language, "file:"+h.path, h)
		f.putModule(w, run, h, language)
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
		var expected model.GenerationID
		if i > 0 {
			expected = gens[i-1]
		}
		f.activate(gen, expected)
		gens = append(gens, gen)
	}

	for i, want := range []string{"c", "cpp"} {
		r, err := f.s.PinGeneration(f.ctx, f.repo, gens[i], time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := r.ParsedLanguage(f.ctx, "treesitter", "file:"+h.path, h.path)
		if err != nil || !ok || got != want {
			t.Errorf("generation %d: ParsedLanguage(%s) = %q, %v, %v; want %q", gens[i], h.path, got, ok, err, want)
		}
		got, ok, err = r.ParsedLanguage(f.ctx, "treesitter", "file:"+other.path, other.path)
		if err != nil || ok {
			t.Errorf("generation %d: ParsedLanguage(%s) with no unit = %q, %v, %v; want not found", gens[i], other.path, got, ok, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
