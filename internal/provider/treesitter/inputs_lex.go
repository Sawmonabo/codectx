package treesitter

import "strings"

// token is one lexical token of a source the request-input table reads:
// an identifier, a string literal, a number, or one punctuation byte, with
// whether a line break precedes it. Comments and whitespace are not tokens.
type token struct {
	kind       byte
	start, end uint32
	nl         bool
}

// The token kinds.
const (
	tokIdent  byte = 'i'
	tokString byte = 's'
	tokNumber byte = 'n'
	tokPunct  byte = 'p'
)

// lexSource is the tokens of src in one pass, comments and string bodies
// skipped by the language's own rules: Python's `#` comments, prefixed and
// triple-quoted strings; the `//` and `/* */` comments of the C family;
// JavaScript's template literals with their `${}` holes, Go's raw strings,
// Java's text blocks. A regular-expression literal is not told apart from a
// division, so a quote inside one is read as a string's start.
func lexSource(language string, src []byte) []token {
	py := language == "python"
	var out []token
	nl := false
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			nl = true
			i++
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v' || py && c == '\\':
			i++
			continue
		case py && c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		case !py && c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		case !py && c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(string(src[i+2:]), "*/")
			next := len(src)
			if end >= 0 {
				next = i + 2 + end + 2
			}
			if strings.IndexByte(string(src[i:next]), '\n') >= 0 {
				nl = true
			}
			i = next
			continue
		case isQuote(language, c):
			end := stringEnd(language, src, i)
			out = append(out, token{kind: tokString, start: uint32(i), end: uint32(end), nl: nl})
			i = end
		case isIdent(c, true) || c >= 0x80:
			j := i + 1
			for j < len(src) && (isIdent(src[j], false) || src[j] >= 0x80) {
				j++
			}
			if py && j-i <= 2 && j < len(src) && (src[j] == '"' || src[j] == '\'') && strings.Trim(string(src[i:j]), "rRbBuUfF") == "" {
				end := stringEnd(language, src, j)
				out = append(out, token{kind: tokString, start: uint32(i), end: uint32(end), nl: nl})
				i = end
				break
			}
			out = append(out, token{kind: tokIdent, start: uint32(i), end: uint32(j), nl: nl})
			i = j
		case '0' <= c && c <= '9':
			j := i + 1
			for j < len(src) && (isIdent(src[j], false) || src[j] == '.') {
				j++
			}
			out = append(out, token{kind: tokNumber, start: uint32(i), end: uint32(j), nl: nl})
			i = j
		default:
			out = append(out, token{kind: tokPunct, start: uint32(i), end: uint32(i + 1), nl: nl})
			i++
		}
		nl = false
	}
	return out
}

// isQuote reports whether c opens a string or character literal.
func isQuote(language string, c byte) bool {
	switch c {
	case '"', '\'':
		return true
	case '`':
		return language == "go" || jsLanguage(language)
	}
	return false
}

// jsLanguage reports an ECMAScript language.
func jsLanguage(language string) bool {
	return language == "javascript" || language == "typescript" || language == "tsx"
}

// stringEnd is the offset just past the literal that opens at src[i]. A
// literal that does not close ends at the line break (or, for one that may
// span lines, at the end of the source).
func stringEnd(language string, src []byte, i int) int {
	q := src[i]
	if (language == "python" || language == "java" && q == '"') && i+2 < len(src) && src[i+1] == q && src[i+2] == q {
		for j := i + 3; j < len(src); j++ {
			if src[j] == '\\' {
				j++
				continue
			}
			if j+2 < len(src) && src[j] == q && src[j+1] == q && src[j+2] == q {
				return j + 3
			}
		}
		return len(src)
	}
	if q == '`' && language == "go" {
		if end := strings.IndexByte(string(src[i+1:]), '`'); end >= 0 {
			return i + 1 + end + 1
		}
		return len(src)
	}
	if q == '`' {
		return templateEnd(language, src, i)
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case q:
			return j + 1
		case '\n':
			return j
		}
	}
	return len(src)
}

// templateEnd is the offset just past the ECMAScript template literal that
// opens at src[i]: each `${` hole is skipped to its matching brace, its own
// literals skipped whole.
func templateEnd(language string, src []byte, i int) int {
	for j := i + 1; j < len(src); j++ {
		switch {
		case src[j] == '\\':
			j++
		case src[j] == '`':
			return j + 1
		case src[j] == '$' && j+1 < len(src) && src[j+1] == '{':
			depth := 0
			for j++; j < len(src); j++ {
				c := src[j]
				if isQuote(language, c) {
					j = stringEnd(language, src, j) - 1
					continue
				}
				if c == '{' {
					depth++
				} else if c == '}' {
					if depth--; depth == 0 {
						break
					}
				}
			}
		}
	}
	return len(src)
}
