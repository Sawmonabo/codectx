package lang

import "strings"

// RustGlob is the name RustUse yields for a glob leaf (`a::*`), whose path
// is the module every member of which it imports.
const RustGlob = "*"

// RustUse yields every leaf of the Rust use tree p (a use declaration's
// argument as written): the name the leaf binds and the full `::` path it
// names. `a::{b, c as d}` binds b to a::b and d to a::c; a list is split only
// at its own commas, so `{b::{c, d}, e}` binds c, d and e. `self` in a list
// binds the list's own path under its last segment, so `std::io::{self,
// Write}` binds io and Write, never "self". A glob yields RustGlob with the
// module it imports from; `as _` and a `self` with no path before it bind
// nothing. The parent's link tables and the worker's import names both read
// use trees through it, so the two never disagree on what a use binds.
func RustUse(p string, yield func(name, path string)) { rustUse("", p, yield) }

// rustUse is RustUse for the use tree p inside a list whose path so far is
// prefix.
func rustUse(prefix, p string, yield func(name, path string)) {
	join := func(a, b string) string {
		if a == "" {
			return b
		}
		if b == "" {
			return a
		}
		return a + "::" + b
	}
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '{'); i >= 0 {
		prefix = join(prefix, strings.TrimSuffix(strings.TrimSpace(p[:i]), "::"))
		inner := strings.TrimSuffix(strings.TrimSpace(p[i+1:]), "}")
		depth, from := 0, 0
		for j := 0; j <= len(inner); j++ {
			if j < len(inner) {
				switch inner[j] {
				case '{':
					depth++
					continue
				case '}':
					depth--
					continue
				case ',':
					if depth > 0 {
						continue
					}
				default:
					continue
				}
			}
			if item := strings.TrimSpace(inner[from:j]); item != "" {
				rustUse(prefix, item, yield)
			}
			from = j + 1
		}
		return
	}
	if f := strings.Fields(p); len(f) >= 3 && f[len(f)-2] == "as" {
		// `self as b` in a list renames the list's own path.
		target := strings.Join(f[:len(f)-2], "")
		if target == "self" {
			target = ""
		}
		if alias, full := f[len(f)-1], join(prefix, target); alias != "_" && full != "" {
			yield(alias, full)
		}
		return
	}
	leaf := p
	if i := strings.LastIndex(p, "::"); i >= 0 {
		leaf = p[i+2:]
	}
	switch leaf {
	case RustGlob:
		yield(RustGlob, join(prefix, strings.TrimSuffix(strings.TrimSuffix(p, RustGlob), "::")))
	case "self":
		if prefix != "" {
			yield(prefix[strings.LastIndex(prefix, ":")+1:], prefix)
		}
	default:
		yield(leaf, join(prefix, p))
	}
}
