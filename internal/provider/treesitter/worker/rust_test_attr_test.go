package worker

import "testing"

// TestRustTestNeedsATestAttributeOrATestOnlyCfg protects the Rust test
// detector: a function is a test when a test attribute marks it (`#[test]`,
// a path ending in `::test`) or a `#[cfg]` it carries holds only in a test
// build; a cfg that holds outside one (`not(test)`, `any(test, ...)`) and
// an attribute that merely spells `test` elsewhere leave it a function.
//
// Mutation: match the word test anywhere in an attribute -> prod, either and
// doc are tests.
// Mutation: evaluate any() as all() -> either is a test.
// Mutation: drop the cfg rule -> helper and both are functions.
func TestRustTestNeedsATestAttributeOrATestOnlyCfg(t *testing.T) {
	src := `#[test]
fn plain() {}
#[rt::test]
async fn framework() {}
#[cfg(test)]
fn helper() {}
#[cfg(all(test, unix))]
fn both() {}
#[cfg(not(test))]
fn prod() {}
#[cfg(any(test, feature = "x"))]
fn either() {}
#[doc = "a test helper"]
fn doc() {}
`
	decls := rustDecls(t, src)
	for name, test := range map[string]bool{"plain": true, "framework": true, "helper": true, "both": true,
		"prod": false, "either": false, "doc": false} {
		if d := declNamed(t, decls, name, 0); (d.Kind == "test") != test || d.Test != test {
			t.Errorf("%s is a %s (test %v), want test %v", name, d.Kind, d.Test, test)
		}
	}
}
