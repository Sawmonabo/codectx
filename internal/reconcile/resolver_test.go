package reconcile_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/reconcile"
)

// declaring is a file-scoped provider that publishes one function "Sym" per
// file and aliases the package-scoped native key "Sym" to it, so two files
// produce two equally supported identities for one symbol.
var declaring = providertest.Func{
	Desc: model.ProviderDescriptor{ID: "decl", Version: "1", Capabilities: []string{"structure"}, InvalidationScope: model.InvalidationFile, Required: true},
	IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
		var fv model.FileVersion
		err := req.Content.EachFile(ctx, model.FileSelection{Paths: []string{req.Unit.ScopeKey}}, func(f model.FileVersion) error {
			fv = f
			return nil
		})
		if err != nil {
			return model.ProviderResult{}, err
		}
		rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: uint64(fv.Size), Line: 1}}
		res, err := req.Resolver.Resolve(ctx, model.NodeCandidate{ProviderID: "decl", ScopeKey: fv.Path, NativeKey: "Sym", Kind: model.NodeFunction,
			Name: "Sym", QualifiedName: fv.Path + ".Sym", FileID: fv.ID, ContentHash: fv.ContentHash, Range: rng})
		if err != nil {
			return model.ProviderResult{}, err
		}
		fact := model.NodeFact{Node: res.Node, CanonicalKey: res.CanonicalKey,
			Evidence: []model.Evidence{providertest.Evidence(req, res.Node.ID, model.PrecisionSyntax, fv, rng)}}
		if err := sink.PutNodes(ctx, []model.NodeFact{fact}); err != nil {
			return model.ProviderResult{}, err
		}
		if err := sink.PutAliases(ctx, []model.NativeAlias{{ScopeKey: "pkg", NativeKey: "Sym", NodeID: res.Node.ID}}); err != nil {
			return model.ProviderResult{}, err
		}
		return providertest.Succeeded(req, 2, 0), nil
	},
}

// TestResolutionIsIndependentOfDependencyCompletionOrder protects the Section
// 9.4 determinism rule: "do not choose by worker completion order". Failure
// mode: a resolver that returned whichever alias its dependency wrote first,
// or ordered the ambiguous list by row order, would give a source-equivalent
// unit different canonical identities on every run, so unit reuse would
// compare unequal keys, may_refer_to edges would point at different nodes per
// build and two generations of the same bytes would disagree on identity.
func TestResolutionIsIndependentOfDependencyCompletionOrder(t *testing.T) {
	files := map[string]string{"a.go": "package p\n", "b.go": "package p\n\n"}
	orders := [][]string{{"a.go", "b.go"}, {"b.go", "a.go"}}
	var results []model.Resolution
	for _, order := range orders {
		h := providertest.New(t, files)
		var deps []model.UnitID
		for _, path := range order {
			result, unit, err := h.Run(t, declaring, path, []string{path})
			if err != nil || result.State != model.RunSucceeded {
				t.Fatalf("declaring unit %s: %v %v", path, result.State, err)
			}
			deps = append(deps, unit)
		}
		r, err := reconcile.New(h.Store, h.Repo, deps)
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Resolve(context.Background(), model.NodeCandidate{ProviderID: "scip", ScopeKey: "pkg", NativeKey: "Sym",
			Kind: model.NodeFunction, Name: "Sym"})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if res.Basis != model.MatchNativeKey || len(res.Ambiguous) != 1 || res.CanonicalKey == "" {
			t.Fatalf("resolution = %+v, want a native-key match with one retained ambiguous alternative", res)
		}
		results = append(results, res)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatalf("resolution depends on dependency completion order:\n%+v\n%+v", results[0], results[1])
	}
}
