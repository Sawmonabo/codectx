package worker

import "testing"

// TestRustRawNamesKeepRawStrings protects what a raw identifier's reading
// leaves alone: `r#` is dropped only where an identifier follows it, so a
// raw string literal (`r#"…"#`, `br#"…"#`) keeps its text, and only at the
// start of a word, so an identifier merely ending in `r` is untouched.
//
// Mutation: drop every `r#` -> the raw strings lose their prefix.
// Mutation: drop `r#` inside a word -> `for#x`'s `r` is lost.
// Mutation: read a path's first identifier alone -> `a::r#b` keeps `r#`.
func TestRustRawNamesKeepRawStrings(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"r#match", "match"},
		{"r#Raw::r#async::Thing<r#T>", "Raw::async::Thing<T>"},
		{"a::r#b", "a::b"},
		{`r#"text"#`, `r#"text"#`},
		{`br#"text"#`, `br#"text"#`},
		{"for#x", "for#x"},
		{"plain::path", "plain::path"},
	} {
		if got := rawPath(c.in); got != c.want {
			t.Errorf("rawPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, c := range []struct{ in, want string }{{"r#match", "match"}, {`r#"x"#`, `r#"x"#`}, {"run", "run"}} {
		if got := string(rawIdent([]byte(c.in))); got != c.want {
			t.Errorf("rawIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
