package treesitter

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// TestECMAImportSyntax pins the form each ECMAScript import record's text
// gives, which selects a package's `import` or `require` condition.
func TestECMAImportSyntax(t *testing.T) {
	for _, c := range []struct {
		language, text, want, mutation string
	}{
		{"javascript", `import { a } from "require-me"`, "", "a declaration whose source names require read as a require"},
		{"typescript", `import("./x")`, link.ImportDynamic, "a dynamic import read as a declaration"},
		{"tsx", `import ("./x")`, link.ImportDynamic, "a dynamic import with a space read as a declaration"},
		{"javascript", `require("./x")`, link.ImportRequire, "a require call read as an import"},
		{"typescript", `a = require("./x")`, link.ImportRequire, "an import-equals clause read as an import"},
		{"python", `import os`, "", "a form given to another language"},
	} {
		if got := ecmaImportSyntax(c.language, c.text); got != c.want {
			t.Errorf("%s %q -> %q, want %q (mutation: %s)", c.language, c.text, got, c.want, c.mutation)
		}
	}
}
