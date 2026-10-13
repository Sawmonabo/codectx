package treesitter_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// decoratorNames are the names of the decorators published under key on the
// one node of kind named name.
func decoratorNames(t *testing.T, c *capture, kind model.NodeKind, name, key string) []string {
	t.Helper()
	raw, err := json.Marshal(metadata(t, c.node(t, kind, name))[key])
	if err != nil {
		t.Fatal(err)
	}
	var decs []model.Decorator
	if err := json.Unmarshal(raw, &decs); err != nil {
		t.Fatalf("%s %s %s: %v", name, key, raw, err)
	}
	var out []string
	for _, d := range decs {
		out = append(out, d.Name)
	}
	return out
}

// checkDecorators checks one node's decorators under key.
func checkDecorators(t *testing.T, c *capture, kind model.NodeKind, name, key string, want ...string) {
	t.Helper()
	if got := decoratorNames(t, c, kind, name, key); !slices.Equal(got, want) {
		t.Errorf("%s %s %s = %v, want %v", kind, name, key, got, want)
	}
}

// TestTypeScriptParameterAndClassExpressionDecorators is TypeScript's
// parameter decorators, a parameter property's included, each on its
// parameter's variable node and none on the method, and a class
// expression's decorators on the declaration that holds the class.
// Mutations caught: parameter decorators not read (no decorators on b or
// x), read onto the method (the method lists Body), and a class
// expression's decorators not read (C has none).
func TestTypeScriptParameterAndClassExpressionDecorators(t *testing.T) {
	c := extracted(t, newProvider(t), "api.ts", `class Api {
  @Get()
  find(@Body() b: string, plain: number) { return b + plain; }

  constructor(@Inject(Token) private readonly x: string) {}
}

module.exports = @sealed class Store {};
`)
	checkDecorators(t, c, model.NodeVariable, "b", model.MetaDecorators, "Body")
	checkDecorators(t, c, model.NodeVariable, "x", model.MetaDecorators, "Inject")
	checkDecorators(t, c, model.NodeVariable, "plain", model.MetaDecorators)
	checkDecorators(t, c, model.NodeMethod, "find", model.MetaDecorators, "Get")
	checkDecorators(t, c, model.NodeClass, "exports", model.MetaDecorators, "sealed")
}

// TestJavaParameterAndTypeUseAnnotations is a Java parameter's annotations
// on its variable node and the type-use annotations of a method's
// signature under type_annotations, apart from its own annotations.
// Mutations caught: parameter annotations not read, type-use annotations
// read as the method's own (its decorators list NonNull), and a generic
// argument's or an array dimension's annotation missed.
func TestJavaParameterAndTypeUseAnnotations(t *testing.T) {
	c := extracted(t, newProvider(t), "A.java", `class A {
    @Deprecated
    java.util.@NonNull List<@Inner String> m(@RequestParam("q") String q, @B int @C [] arr) {
        return null;
    }
}
`)
	checkDecorators(t, c, model.NodeMethod, "m", model.MetaDecorators, "Deprecated")
	checkDecorators(t, c, model.NodeMethod, "m", model.MetaTypeAnnotations, "NonNull", "Inner", "C")
	checkDecorators(t, c, model.NodeVariable, "q", model.MetaDecorators, "RequestParam")
	checkDecorators(t, c, model.NodeVariable, "arr", model.MetaDecorators, "B")
}

// TestRustInnerAttributes is Rust's inner attributes: a file's apply to its
// module, and a function's or module's body's apply to that item, after
// its outer attributes. Mutation caught: inner attributes not read (the
// module and f list none).
func TestRustInnerAttributes(t *testing.T) {
	c := extracted(t, newProvider(t), "lib.rs", `#![allow(dead_code)]

#[inline]
fn f() {
    #![allow(unused)]
}

mod m {
    #![deny(missing_docs)]
}
`)
	checkDecorators(t, c, model.NodeModule, "lib.rs", model.MetaDecorators, "allow")
	checkDecorators(t, c, model.NodeFunction, "f", model.MetaDecorators, "inline", "allow")
	checkDecorators(t, c, model.NodeModule, "m", model.MetaDecorators, "deny")
}

// TestAlignas is the alignment specifier as a decorator of the variable,
// the struct and the field it is written on, with its operand as the
// argument, and C's `_Alignas` spelling as written. Mutation caught:
// alignas not read (no decorators).
func TestAlignas(t *testing.T) {
	src := `alignas(16) int x;
struct alignas(8) S { alignas(4) int f; };
`
	c := extracted(t, newProvider(t), "a.cpp", src)
	checkDecorators(t, c, model.NodeVariable, "x", model.MetaDecorators, "alignas")
	checkDecorators(t, c, model.NodeStruct, "S", model.MetaDecorators, "alignas")
	checkDecorators(t, c, model.NodeField, "f", model.MetaDecorators, "alignas")
	if got := decoratorsOf(t, c, src, model.NodeVariable, "x"); len(got) != 1 || len(got[0]) != 3 || got[0][2] != "16" {
		t.Errorf("x decorators %q, want alignas with the argument 16", got)
	}
	cc := extracted(t, newProvider(t), "a.c", "_Alignas(8) int y;\n")
	checkDecorators(t, cc, model.NodeVariable, "y", model.MetaDecorators, "_Alignas")
}
