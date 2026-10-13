package worker

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// jsString is the value of an ECMAScript string literal written as raw,
// its quotes included: the quotes removed and every escape sequence decoded
// (ECMA-262 §12.9.4, String Literals). A literal with no backslash is its
// text between the quotes, with no allocation beyond the slice.
func jsString(raw string) string {
	if n := len(raw); n >= 2 && (raw[0] == '"' || raw[0] == '\'' || raw[0] == '`') && raw[n-1] == raw[0] {
		raw = raw[1 : n-1]
	} else {
		raw = strings.Trim(raw, "\"'`")
	}
	return jsUnescape(raw)
}

// jsUnescape decodes the escape sequences of a string literal's or a
// template's characters s: the single-character escapes (\b \t \n \v \f \r
// \" \' \\), \0, legacy octal escapes, \xHH, \uHHHH (a surrogate pair
// joined into one code point), \u{H…}, a line continuation (a backslash
// before a line terminator, which contributes nothing), and any other
// escaped character as itself (NonEscapeCharacter).
func jsUnescape(s string) string {
	i := strings.IndexByte(s, '\\')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	b.WriteString(s[:i])
	var pending rune = -1 // a high surrogate waiting for its low half
	emit := func(r rune) {
		if pending >= 0 {
			if utf16.IsSurrogate(r) && r >= 0xDC00 {
				b.WriteRune(utf16.DecodeRune(pending, r))
				pending = -1
				return
			}
			b.WriteRune(utf8.RuneError)
			pending = -1
		}
		if r >= 0xD800 && r < 0xDC00 {
			pending = r
			return
		}
		b.WriteRune(r)
	}
	flush := func() {
		if pending >= 0 {
			b.WriteRune(utf8.RuneError)
			pending = -1
		}
	}
	for i < len(s) {
		c := s[i]
		if c != '\\' || i+1 == len(s) {
			flush()
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			i += size
			continue
		}
		i++
		switch c = s[i]; c {
		case 'b':
			emit('\b')
			i++
		case 't':
			emit('\t')
			i++
		case 'n':
			emit('\n')
			i++
		case 'v':
			emit('\v')
			i++
		case 'f':
			emit('\f')
			i++
		case 'r':
			emit('\r')
			i++
		case '\r':
			// A line continuation: \ CR LF, \ CR.
			i++
			if i < len(s) && s[i] == '\n' {
				i++
			}
		case '\n':
			i++
		case 'x':
			if v, ok := hexValue(s[i+1:], 2); ok {
				emit(rune(v))
				i += 3
				continue
			}
			emit('x')
			i++
		case 'u':
			if i+1 < len(s) && s[i+1] == '{' {
				if end := strings.IndexByte(s[i+2:], '}'); end > 0 {
					if v, ok := hexValue(s[i+2:i+2+end], end); ok && v <= utf8.MaxRune {
						emit(rune(v))
						i += 3 + end
						continue
					}
				}
			} else if v, ok := hexValue(s[i+1:], 4); ok {
				emit(rune(v))
				i += 5
				continue
			}
			emit('u')
			i++
		default:
			if c >= '0' && c <= '7' {
				// \0 and the legacy octal escapes: up to three digits, a
				// value below 256.
				v, n := 0, 0
				for n < 3 && i+n < len(s) && s[i+n] >= '0' && s[i+n] <= '7' && v*8+int(s[i+n]-'0') < 256 {
					v = v*8 + int(s[i+n]-'0')
					n++
				}
				emit(rune(v))
				i += n
				continue
			}
			// U+2028 and U+2029 continue a line too; any other character
			// escapes to itself.
			r, size := utf8.DecodeRuneInString(s[i:])
			if r != ' ' && r != ' ' {
				emit(r)
			}
			i += size
		}
	}
	flush()
	return b.String()
}

// hexValue is the value of the first n bytes of s as hexadecimal digits,
// false when s is shorter or one is not a digit.
func hexValue(s string, n int) (int, bool) {
	if len(s) < n || n == 0 {
		return 0, false
	}
	v := 0
	for _, c := range []byte(s[:n]) {
		switch {
		case c >= '0' && c <= '9':
			v = v*16 + int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v*16 + int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v*16 + int(c-'A'+10)
		default:
			return 0, false
		}
		if v > utf8.MaxRune {
			return 0, false
		}
	}
	return v, true
}
