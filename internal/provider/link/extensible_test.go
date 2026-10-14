package link_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestMethodsSayWhetherCodeOutsideMayOverrideThem pins, in every language
// with overriding, which method nodes the structural provider marks
// `extensible`: a method code outside the indexed project can override or
// implement, so the overrides the index lists for it may not be all of
// them. Closed cases stay unmarked: a final, static or private method, one
// of a final, sealed, enum or record class, of a type outside the language's
// export rule (a package-private Java class, an unexported Go interface or
// method, a package main or internal Go package, an unexported ECMAScript
// class, a Rust trait that is not `pub` or that is sealed (a supertrait code outside
// the crate cannot name: of a private module, or not `pub`, and not
// re-exported), a C++ class in a source file or an
// unnamed namespace), and a type's own method where the language has no
// overriding of it (a Go or Rust concrete method).
//
// Mutations caught: no mark at all (every open name fails); marking by the
// language's export flag alone (a final or private Java method, an
// unexported Go method of an exported interface, a C++ final member is
// marked); ignoring the enclosing type (a method of a final or package-
// private Java class, of a sealed interface or of a Python @final class is
// marked); reading a sealed Rust trait as open (Shape::area is marked); sealing
// through a re-exported or public module (Keyed::keyed, Marked::marked
// unmarked); taking a `pub(crate) use` for a re-export (Tagged::tagged is
// marked).
func TestMethodsSayWhetherCodeOutsideMayOverrideThem(t *testing.T) {
	structural := newStructural(t)
	cases := []struct {
		name         string
		files        map[string]string
		open, closed []string
	}{
		{"java", map[string]string{
			"Handler.java": "public interface Handler { void serve(); default void log() {} static void make() {} private void own() {} }\n",
			"Base.java": "public class Base { public void run() {} protected void hook() {} void pkg() {} private void hide() {}\n" +
				"  public final void fixed() {} public static void util() {} }\n",
			"Closed.java": "public final class Closed { public void run() {} }\n",
			"Hidden.java": "class Hidden { public void run() {} }\n",
			"Shape.java":  "public sealed interface Shape permits Circle { double area(); }\n",
			"Circle.java": "final class Circle implements Shape { public double area() { return 1; } }\n",
			"Open.java":   "public non-sealed class Open { public void run() {} }\n",
			"Color.java":  "public enum Color { RED; public void paint() {} }\n",
			"P.java":      "public record P(int x) { public int twice() { return 2 * x; } }\n",
			"Outer.java": "public class Outer { public static class Inner { public void go() {} }\n" +
				"  private static class Secret { public void go() {} } }\n",
		},
			[]string{"Handler.serve", "Handler.log", "Base.run", "Base.hook", "Open.run", "Outer.Inner.go"},
			[]string{"Handler.make", "Handler.own", "Base.pkg", "Base.hide", "Base.fixed", "Base.util", "Closed.run",
				"Hidden.run", "Shape.area", "Circle.area", "Color.paint", "P.twice", "Outer.Secret.go"}},
		{"go", map[string]string{
			"go.mod":          "module example.com/p\n\ngo 1.22\n",
			"p.go":            "package p\n\ntype Handler interface {\n\tServe()\n\tserve()\n}\n\ntype handler interface{ Serve() }\n\ntype T struct{}\n\nfunc (T) Serve() {}\n",
			"cmd/x/main.go":   "package main\n\ntype Plugin interface{ Run() }\n\nfunc main() {}\n",
			"internal/q/q.go": "package q\n\ntype I interface{ Do() }\n",
		},
			[]string{"p.Handler.Serve"},
			[]string{"p.Handler.serve", "p.handler.Serve", "p.T.Serve", "main.Plugin.Run", "q.I.Do"}},
		{"typescript", map[string]string{"h.ts": "export interface Handler { serve(): void }\n" +
			"export class Base { run() {} private hide() {} static make() {} protected hook() {} }\n" +
			"class Local { run() {} }\nexport abstract class Abs { abstract go(): void }\nfunction use(b: Base) { b.run(); }\n"},
			[]string{"Handler.serve", "Base.run", "Base.hook", "Abs.go"},
			[]string{"Base.hide", "Base.make", "Local.run"}},
		{"javascript", map[string]string{"h.js": "export class Base { run() {} #own() {} static make() {} }\nclass Local { run() {} }\nfunction use() { new Local().run(); }\n"},
			[]string{"Base.run"},
			[]string{"Base.#own", "Base.make", "Local.run"}},
		{"python", map[string]string{"h.py": "from typing import final\n\n\nclass Handler:\n    def serve(self):\n        pass\n\n" +
			"    def __init__(self):\n        pass\n\n    def __own(self):\n        pass\n\n    @final\n    def fixed(self):\n        pass\n\n\n" +
			"@final\nclass Closed:\n    def serve(self):\n        pass\n\n\ndef outer():\n    class Local:\n        def go(self):\n            pass\n\n\ndef use(h: Handler):\n    h.serve()\n"},
			[]string{"Handler.serve", "Handler.__init__"},
			[]string{"Handler.__own", "Handler.fixed", "Closed.serve", "outer.Local.go"}},
		{"rust", map[string]string{
			"Cargo.toml": "[package]\nname = \"r\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			"src/lib.rs": "pub trait Handler { fn serve(&self); fn log(&self) {} }\npub(crate) trait Inner { fn go(&self); }\n" +
				"trait Local { fn go(&self); }\npub struct T;\nimpl Handler for T { fn serve(&self) {} }\n",
		},
			[]string{"Handler::serve", "Handler::log"},
			[]string{"Inner::go", "Local::go", "T::serve"}},
		{"rust-sealed", map[string]string{
			"Cargo.toml": "[package]\nname = \"r\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			"src/lib.rs": "mod private { pub trait Sealed {} }\n" +
				"pub trait Shape: private::Sealed { fn area(&self) -> f64; }\n" +
				"pub trait Framed: Send + self::private::Sealed { fn frame(&self); }\n" +
				"pub(crate) trait Hidden {}\npub trait Inward: Hidden { fn inward(&self); }\n" +
				"use private::Sealed;\npub trait Imported: Sealed { fn imported(&self); }\n" +
				"pub mod open { pub trait Marker {} }\npub trait Marked: open::Marker { fn marked(&self); }\n" +
				"mod leaked { pub trait Key {} }\npub use leaked::Key;\npub trait Keyed: leaked::Key { fn keyed(&self); }\n" +
				"mod inner { pub trait Tag {} }\npub(crate) use inner::Tag;\npub trait Tagged: inner::Tag { fn tagged(&self); }\n" +
				"pub trait Plain: Send + Clone { fn plain(&self); }\n",
		},
			[]string{"Marked::marked", "Keyed::keyed", "Plain::plain"},
			[]string{"Shape::area", "Framed::frame", "Inward::inward", "Imported::imported", "Tagged::tagged"}},
		{"cpp", map[string]string{
			"h.h": "class Handler { public: virtual void serve(); virtual void fixed() final; static void make(); };\n" +
				"class Closed final { public: virtual void serve(); };\n",
			"a.cpp": "#include \"h.h\"\nclass Local { public: virtual void run(); };\nnamespace { class Anon { public: virtual void run(); }; }\n",
		},
			[]string{"Handler::serve"},
			[]string{"Handler::fixed", "Handler::make", "Closed::serve", "Local::run", "Anon::run"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := linkSources(t, structural, c.files)
			marked := map[string]bool{}
			var methods []string
			for _, n := range l.structural.nodes {
				if n.Kind != model.NodeMethod || n.QualifiedName == "" {
					continue
				}
				methods = append(methods, n.QualifiedName)
				var meta struct {
					Extensible bool `json:"extensible"`
				}
				_ = json.Unmarshal(n.Metadata, &meta)
				marked[n.QualifiedName] = marked[n.QualifiedName] || meta.Extensible
			}
			slices.Sort(methods)
			for _, name := range c.open {
				if !slices.Contains(methods, name) {
					t.Errorf("no method node %s among %s", name, strings.Join(methods, ", "))
				} else if !marked[name] {
					t.Errorf("%s is not marked extensible; code outside the project can override it", name)
				}
			}
			for _, name := range c.closed {
				if !slices.Contains(methods, name) {
					t.Errorf("no method node %s among %s", name, strings.Join(methods, ", "))
				} else if marked[name] {
					t.Errorf("%s is marked extensible; no code outside the project can override it", name)
				}
			}
		})
	}
}
