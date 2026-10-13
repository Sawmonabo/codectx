package worker

import "testing"

// TestJSString pins ECMA-262 §12.9.4: a module specifier is the string
// literal's value, its escapes decoded.
func TestJSString(t *testing.T) {
	for _, c := range []struct {
		raw, want, mutation string
	}{
		{`"./a"`, "./a", "the quotes kept"},
		{`'./\x61'`, "./a", `\xHH left as written`},
		{`"./a"`, "./a", `\uHHHH left as written`},
		{`"./\u{1F600}"`, "./\U0001F600", `\u{…} left as written`},
		{`"./😀"`, "./\U0001F600", "a surrogate pair decoded as two replacement characters"},
		{`"a\.b"`, "a.b", "a NonEscapeCharacter kept with its backslash"},
		{`"a\\b"`, `a\b`, "an escaped backslash decoded twice or dropped"},
		{"\"a\\\nb\"", "ab", "a line continuation kept as a newline"},
		{`"\101\0"`, "A\x00", "a legacy octal escape or \\0 left as written"},
		{"`./\\x61`", "./a", "a template literal's escapes left as written"},
	} {
		if got := jsString(c.raw); got != c.want {
			t.Errorf("jsString(%q) = %q, want %q (mutation: %s)", c.raw, got, c.want, c.mutation)
		}
	}
}
