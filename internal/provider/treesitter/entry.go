package treesitter

import (
	"bytes"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// entryPoints is the file's program entry points (ADR-0017 decision 8), each
// read from the syntax alone:
//
//   - a Python `if __name__ == "__main__":` at module level, which the worker
//     extracts as an `endpoint` declaration; it routes to what its block calls
//     directly;
//   - Go: a top-level `func main` with a body in `package main`;
//   - Java: every `main` JLS SE 25 §12.1.4 launches, one per class (javaMains);
//   - Rust: a top-level `fn main` with a body;
//   - C and C++: a file-scope `main` definition that is not `static`;
//   - any file whose first bytes are `#!`: a script, which runs from its top,
//     so it routes to the file's module.
func (b *builder) entryPoints() []endpointFact {
	var out []endpointFact
	switch b.lang.Name {
	case "python":
		for i := range b.decls {
			if b.decls[i].kind == model.NodeEndpoint {
				out = append(out, endpointFact{isDecl: true, decl: i, handlers: b.guardCalls(i)})
			}
		}
	case "go":
		if b.ex.done.Package == "main" {
			out = append(out, b.mainFunctions()...)
		}
	case "rust", "c", "cpp":
		out = append(out, b.mainFunctions()...)
	case "java":
		out = append(out, b.javaMains()...)
	}
	if at, ok := b.shebang(); ok {
		out = append(out, endpointFact{descriptor: "script", at: at, handlers: []handler{moduleHandler()}})
	}
	return out
}

// guardCalls are the handlers of a `__main__` guard: what each call its block
// makes directly (a reference whose scope is the guard itself) names, once
// each, in source order. A bare or path-qualified call to a name declared
// exactly once in this file routes to that declaration; any other routes to the callee reference the
// call names.
func (b *builder) guardCalls(guard int) []handler {
	var out []handler
	seen := map[string]bool{}
	for _, r := range b.ex.refs {
		if r.Kind != "call" || r.Scope != guard {
			continue
		}
		targets := b.callTargets(r, b.targets(r))
		key := calleeKey(r, targets)
		if len(targets) == 1 {
			key = "decl:" + string(b.decls[targets[0]].res.Node.ID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(targets) == 1 {
			out = append(out, declHandler(targets[0]))
		} else {
			out = append(out, refHandler(r))
		}
	}
	return out
}

// mainFunctions is the entry point of a top-level function named `main` that
// has a body: not a prototype, macro, method or member, and not
// declared `static` (internal linkage in C and C++, a word no other language
// spells before a top-level function). Its endpoint is at the function's
// declaration and routes to it.
func (b *builder) mainFunctions() []endpointFact {
	var out []endpointFact
	for i := range b.decls {
		d := &b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeFunction || d.Name != "main" || d.Prototype || d.Macro || d.Impl != "" || d.SigEnd >= d.End {
			continue
		}
		if hasWord(b.words(d, d.Start, d.NameStart), "static") {
			continue
		}
		out = append(out, endpointFact{descriptor: "main", at: wire.Span{Start: d.Start, End: d.End}, handlers: []handler{declHandler(i)}})
	}
	return out
}

// shebang is the first line of a file whose first bytes are `#!`, without its
// line terminator. Rust spells an inner attribute `#![...]`, which is no
// shebang (The Rust Reference, Shebang: `#!` followed, after whitespace, by
// `[` begins an attribute).
func (b *builder) shebang() (wire.Span, bool) {
	if !bytes.HasPrefix(b.src, []byte("#!")) {
		return wire.Span{}, false
	}
	if b.lang.Name == "rust" && bytes.HasPrefix(bytes.TrimLeft(b.src[2:], " \t\r\n"), []byte("[")) {
		return wire.Span{}, false
	}
	end := len(b.src)
	if i := bytes.IndexByte(b.src, '\n'); i >= 0 {
		end = i
	}
	if end > 2 && b.src[end-1] == '\r' {
		end--
	}
	return wire.Span{Start: 0, End: uint32(end)}, true
}

// javaMain is one method a Java class may be launched by (JLS SE 25
// §12.1.4): named `main`, not `private`, returning `void`, with a body, and
// with one `String[]` or `String...` parameter (args) or none.
type javaMain struct {
	decl   int
	args   bool
	static bool
}

// javaMains is one entry point per Java class, enum, record or interface of
// this file that declares or inherits a launchable `main`, keyed by the
// class's qualified name, and one for the implicitly declared class of a
// compact source file (a top-level `main`), whose disambiguator is empty.
// JLS §12.1.4 chooses the `main` with a `String[]` parameter over the one
// without, whether declared or inherited, and a declared method overrides an
// inherited one of the same parameters. An interface is launched by a
// `static main` only, having no instance. A class inherits the `main` of its
// superclass; the endpoint is then at the inheriting class and routes to the
// inherited method. A class whose superclass chain leaves this file, with no
// `String[]` main declared nearer, is left to the linker, which reads the
// superclasses the other files declare (mainInherited, link.FactMain); every
// launchable form is marked for it (mainForms).
func (b *builder) javaMains() []endpointFact {
	own := map[int][]javaMain{}
	for i := range b.decls {
		d := &b.decls[i]
		if d.kind != model.NodeMethod || d.Name != "main" {
			continue
		}
		m, ok := b.javaMainForm(i)
		if !ok {
			continue
		}
		if b.mainForms == nil {
			b.mainForms = map[int]uint8{}
		}
		b.mainForms[i] = link.MainNoArgs
		if m.args {
			b.mainForms[i] = link.MainArgs
		}
		if d.Parent >= 0 && b.decls[d.Parent].kind == model.NodeInterface && !m.static {
			continue
		}
		own[d.Parent] = append(own[d.Parent], m)
	}
	var out []endpointFact
	if ms := own[-1]; len(ms) > 0 {
		m := chooseMain(ms)
		d := &b.decls[m.decl]
		out = append(out, endpointFact{descriptor: "main", at: wire.Span{Start: d.Start, End: d.End}, handlers: []handler{declHandler(m.decl)}})
	}
	for c := range b.decls {
		cd := &b.decls[c]
		if cd.kind != model.NodeClass && cd.kind != model.NodeEnum && cd.kind != model.NodeInterface {
			continue
		}
		ms := own[c]
		super, elsewhere := b.javaSuperclass(c)
		for seen := map[int]bool{c: true}; super >= 0 && !seen[super]; super, elsewhere = b.javaSuperclass(super) {
			seen[super] = true
			for _, m := range own[super] {
				if !declaresForm(ms, m.args) {
					ms = append(ms, m)
				}
			}
		}
		if elsewhere && !declaresForm(ms, true) {
			if b.mainInherited == nil {
				b.mainInherited = map[int]bool{}
			}
			b.mainInherited[c] = true
			continue
		}
		if len(ms) == 0 {
			continue
		}
		m := chooseMain(ms)
		at := wire.Span{Start: b.decls[m.decl].Start, End: b.decls[m.decl].End}
		if b.decls[m.decl].Parent != c {
			at = wire.Span{Start: cd.Start, End: cd.End}
		}
		out = append(out, endpointFact{descriptor: "main", disambiguator: cd.Qualified, at: at, handlers: []handler{declHandler(m.decl)}})
	}
	return out
}

// declaresForm reports whether ms holds a main of the parameter form args.
func declaresForm(ms []javaMain, args bool) bool {
	for _, m := range ms {
		if m.args == args {
			return true
		}
	}
	return false
}

// chooseMain is the main JLS §12.1.4 launches of a class's candidates: the
// one with a `String[]` parameter, else the one with none.
func chooseMain(ms []javaMain) javaMain {
	for _, m := range ms {
		if m.args {
			return m
		}
	}
	return ms[0]
}

// javaMainForm reads a method's modifiers, result and parameters from its
// signature bytes, with its annotations (the decorator ranges) blanked and
// comments removed, and reports whether it is a launchable main.
func (b *builder) javaMainForm(i int) (javaMain, bool) {
	d := &b.decls[i]
	if d.SigEnd >= d.End {
		return javaMain{}, false // abstract or native: no body to run
	}
	before := b.words(d, d.Start, d.NameStart)
	if len(before) == 0 || before[len(before)-1] != "void" || hasWord(before, "private") {
		return javaMain{}, false
	}
	args, ok := javaMainParams(stripAnnotations(stripJava(b.blanked(d, d.NameEnd, d.SigEnd))))
	if !ok {
		return javaMain{}, false
	}
	return javaMain{decl: i, args: args, static: hasWord(before, "static")}, true
}

// javaMainParams reads the parameter list that opens s, the text after a
// method's name with its comments and annotations removed: true for one
// `String[]`, `String...` or `String x[]` parameter (`java.lang.String` and
// `final` admitted), false for none, and not ok for anything else.
func javaMainParams(s string) (args, ok bool) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return false, false
	}
	depth, end := 0, -1
	for j := open; j < len(s) && end < 0; j++ {
		switch s[j] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				end = j
			}
		}
	}
	if end < 0 || strings.TrimSpace(s[:open]) != "" {
		return false, false
	}
	p := strings.TrimSpace(s[open+1 : end])
	if p == "" {
		return false, true
	}
	if strings.ContainsAny(p, ",<") {
		return false, false
	}
	// `String [] a`, `String ... a` and `java.lang. String[] a` are spelled
	// without the space before a bracket or a dot, or after a dot.
	for _, r := range []struct{ from, to string }{{" [", "["}, {" ]", "]"}, {" .", "."}, {". ", "."}} {
		for strings.Contains(p, r.from) {
			p = strings.ReplaceAll(p, r.from, r.to)
		}
	}
	var fields []string
	for _, f := range strings.Fields(p) {
		if f != "final" {
			fields = append(fields, f)
		}
	}
	if len(fields) != 2 {
		return false, false
	}
	typ, name := fields[0], fields[1]
	if strings.HasSuffix(name, "[]") {
		typ, name = typ+"[]", strings.TrimSuffix(name, "[]")
	}
	if strings.ContainsAny(name, "[].") {
		return false, false
	}
	switch typ {
	case "String[]", "String...", "java.lang.String[]", "java.lang.String...":
		return true, true
	}
	return false, false
}

// javaSuperclass is the class of this file a Java class extends, or -1: the
// type its signature names after `extends` (type parameters skipped), matched
// to the one class of this file of that simple name, or, when several share
// it, to the one whose qualified name ends with the name as written.
// elsewhere reports a class that extends a type no one class of this file
// is.
func (b *builder) javaSuperclass(c int) (super int, elsewhere bool) {
	d := &b.decls[c]
	if d.kind != model.NodeClass {
		return -1, false
	}
	s := strings.TrimSpace(stripAnnotations(stripJava(b.blanked(d, d.NameEnd, d.SigEnd))))
	if strings.HasPrefix(s, "<") {
		depth, j := 0, 0
		for ; j < len(s); j++ {
			if s[j] == '<' {
				depth++
			} else if s[j] == '>' {
				if depth--; depth == 0 {
					break
				}
			}
		}
		if j >= len(s) {
			return -1, false
		}
		s = strings.TrimSpace(s[j+1:])
	}
	rest, ok := strings.CutPrefix(s, "extends")
	if !ok || rest == "" || !isSpace(rest[0]) {
		return -1, false
	}
	written, _ := qualifiedPrefix(strings.TrimLeft(rest, " \t\r\n"))
	simple := written[strings.LastIndexByte(written, '.')+1:]
	if simple == "" {
		return -1, true
	}
	var named, qualified []int
	for _, j := range b.byName[simple] {
		if b.decls[j].kind != model.NodeClass || j == c {
			continue
		}
		named = append(named, j)
		if strings.HasSuffix("."+b.decls[j].Qualified, "."+written) {
			qualified = append(qualified, j)
		}
	}
	switch {
	case len(named) == 1:
		return named[0], false
	case len(qualified) == 1:
		return qualified[0], false
	}
	return -1, true
}

// qualifiedPrefix is the dotted name s opens with, whitespace around its
// dots removed (`a.b.C` of `a . b.C<T> implements I`), and the bytes of s it
// spans.
func qualifiedPrefix(s string) (string, int) {
	var name strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == '.' || isIdentByte(c):
			name.WriteByte(c)
			i++
			continue
		case isSpace(c):
			j := i
			for j < len(s) && isSpace(s[j]) {
				j++
			}
			if strings.HasSuffix(name.String(), ".") || j < len(s) && s[j] == '.' {
				i = j
				continue
			}
		}
		break
	}
	return name.String(), i
}

// isSpace reports an ASCII whitespace byte; isIdentByte a byte of a Java or
// C identifier (any byte of a multi-byte rune included).
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// words is the identifier words of the pinned bytes [start, end) of d's
// signature, its decorators blanked and its comments removed: the modifiers
// and result a C, C++ or Java declaration spells before its name.
func (b *builder) words(d *declFact, start, end uint32) []string {
	return strings.FieldsFunc(stripJava(b.blanked(d, start, end)), func(r rune) bool { return r < 0x80 && !isIdentByte(byte(r)) })
}

// blanked is the pinned bytes [start, end) with every decorator of d inside
// them replaced by spaces, so an annotation's arguments (`@A("private")`)
// never read as the declaration's own words.
func (b *builder) blanked(d *declFact, start, end uint32) string {
	out := []byte(string(b.src[start:end]))
	for _, w := range d.Decorators {
		s, e := max(w.Start, start), min(w.End, end)
		for j := s; j < e; j++ {
			out[j-start] = ' '
		}
	}
	return string(out)
}

// stripJava removes the comments of C-family source text, `//` to the end of
// its line and `/* */`, each replaced by a space. String and character
// literals are kept whole, so a `//` inside one is not a comment.
func stripJava(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case strings.HasPrefix(s[i:], "//"):
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case strings.HasPrefix(s[i:], "/*"):
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				i += j + 3
			} else {
				i = len(s)
			}
			out.WriteByte(' ')
		case s[i] == '"' || s[i] == '\'':
			q := s[i]
			out.WriteByte(q)
			for i++; i < len(s) && s[i] != q; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					out.WriteByte(s[i])
					i++
				}
				out.WriteByte(s[i])
			}
			if i < len(s) {
				out.WriteByte(q)
			}
		default:
			out.WriteByte(s[i])
		}
	}
	return out.String()
}

// stripAnnotations removes every Java annotation from a parameter list's
// text: `@` and the qualified name after it, and the parenthesized arguments
// that follow (strings in them kept whole), each replaced by a space.
func stripAnnotations(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '@' {
			out.WriteByte(s[i])
			continue
		}
		i++
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		_, n := qualifiedPrefix(s[i:])
		i += n
		j := i
		for j < len(s) && isSpace(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '(' {
			depth := 0
			for ; j < len(s); j++ {
				switch s[j] {
				case '"', '\'':
					q := s[j]
					for j++; j < len(s) && s[j] != q; j++ {
						if s[j] == '\\' {
							j++
						}
					}
				case '(':
					depth++
				case ')':
					depth--
				}
				if depth == 0 {
					break
				}
			}
			i = j + 1
		}
		out.WriteByte(' ')
		i--
	}
	return out.String()
}

// hasWord reports whether words holds w.
func hasWord(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}
