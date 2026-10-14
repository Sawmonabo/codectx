package lang

import (
	"slices"
	"strings"
)

// rustPointers are the standard library's smart pointers. A method call on
// one reaches, by auto-dereference, the methods of the value it owns (the
// Rust Reference's "Method-call expressions"), so a receiver declared as
// one carries the owned value's type: `Box<dyn Handler>` is Handler.
var rustPointers = []string{"Box", "Rc", "Arc", "Pin"}

// rustPointerMethods are the methods every smart pointer of rustPointers
// answers itself, through the standard traits it implements (Clone, Deref,
// DerefMut, AsRef, AsMut, Borrow, BorrowMut, PartialEq, PartialOrd, Ord,
// Hash, Debug, Display, Pointer, Any) and the blanket conversions (Into,
// TryInto, ToOwned, ToString). The method-call lookup tries the pointer's
// own type before it dereferences, so such a call on a pointer is the
// pointer's method, never the owned value's.
var rustPointerMethods = []string{"clone", "clone_from", "deref", "deref_mut", "as_ref", "as_mut", "borrow",
	"borrow_mut", "eq", "ne", "partial_cmp", "lt", "le", "gt", "ge", "cmp", "max", "min", "clamp", "hash", "fmt",
	"type_id", "into", "try_into", "to_owned", "to_string"}

// RustPointer reports that the type path name (`Rc`, `std::rc::Rc`, no
// generic arguments) is a standard smart pointer.
func RustPointer(name string) bool {
	if i := strings.LastIndex(name, "::"); i >= 0 {
		name = name[i+2:]
	}
	return slices.Contains(rustPointers, strings.TrimSpace(name))
}

// RustPointerMethod reports that a call of method on a smart pointer is the
// pointer's own method (rustPointerMethods).
func RustPointerMethod(method string) bool { return slices.Contains(rustPointerMethods, method) }

// RustPointee is the type a smart pointer spelled as written owns, with one
// level removed (`Rc<RefCell<T>>` is `RefCell<T>`, `&Box<dyn H>` is `dyn H`),
// and true; false when written is not a smart pointer of one type argument.
func RustPointee(written string) (string, bool) {
	s := strings.TrimLeft(strings.TrimSpace(written), "&* ")
	s = strings.TrimSpace(strings.TrimPrefix(s, "mut "))
	open := strings.IndexByte(s, '<')
	if open < 0 || !strings.HasSuffix(s, ">") || !RustPointer(s[:open]) {
		return "", false
	}
	inner := s[open+1 : len(s)-1]
	depth := 0
	for _, c := range inner {
		switch c {
		case '<', '(', '[':
			depth++
		case '>', ')', ']':
			depth--
		case ',':
			if depth == 0 {
				return "", false
			}
		}
	}
	return strings.TrimSpace(inner), true
}

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
	if before, after, ok := strings.Cut(p, "{"); ok {
		prefix = join(prefix, strings.TrimSuffix(strings.TrimSpace(before), "::"))
		inner := strings.TrimSuffix(strings.TrimSpace(after), "}")
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
