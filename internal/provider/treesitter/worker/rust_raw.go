package worker

import "strings"

// A Rust raw identifier is its identifier written with an `r#` prefix,
// which "is not included as part of the actual identifier" (the Rust
// Reference's "Identifiers"): `r#Foo` and `Foo` are one name, and `r#match`
// is the name `match`. Every Rust name the worker reads, a declaration's,
// a use's, a path's, a type's or a trait's, is read through rawIdent or
// rawPath, so the two spellings match wherever names are compared, and a
// span still covers the bytes as written.

// rawIdent is the Rust identifier text b without a raw identifier's `r#`
// prefix: b from its third byte, so no copy is made. Any other text, a raw
// string literal's `r#"` among it, is b.
func rawIdent(b []byte) []byte {
	if len(b) > 2 && b[0] == 'r' && b[1] == '#' && rsIdentStart(b[2]) {
		return b[2:]
	}
	return b
}

// rawPath is the Rust path or type text s with the `r#` prefix of each raw
// identifier in it removed, one starting s or after a byte of no word; s
// itself, without a copy, when it holds none.
func rawPath(s string) string {
	if !strings.Contains(s, "r#") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 'r' && i+2 < len(s) && s[i+1] == '#' && rsIdentStart(s[i+2]) && (i == 0 || !rsWord(s[i-1])) {
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// rsIdentStart reports a byte that may start a Rust identifier: a letter,
// `_`, or the first byte of a non-ASCII character (XID_Start).
func rsIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
