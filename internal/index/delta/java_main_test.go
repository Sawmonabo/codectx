package delta_test

import "testing"

// TestSuperclassMainEditReresolvesInheritedMains refreshes a Java linking
// unit after its superclass's `main` is renamed away, then declared again
// with no parameter, and compares each refresh with a full build of the
// same tree (session.check). Child inherits Base's `String[]` main, and
// NoArgs prefers it to its own no-parameter main; with Base's main gone,
// Child has no main and NoArgs is launched by its own; with Base's
// no-parameter main, Child inherits it and NoArgs's own overrides it. Only
// Base.java changed, so the two classes' files re-resolve because their
// resolution read Base's mains, and Other.java, which read nothing of Base,
// is carried.
//
// Mutation: read a class's mains without recording the read -> the refresh
// carries Child's and NoArgs's endpoints to Base's renamed method and
// differs from the full build.
func TestSuperclassMainEditReresolvesInheritedMains(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"p/Base.java":   "package p;\npublic class Base {\n    public static void main(String[] args) {}\n}\n",
		"p/Child.java":  "package p;\npublic class Child extends Base {}\n",
		"p/NoArgs.java": "package p;\npublic class NoArgs extends Base {\n    void main() {}\n}\n",
		"p/Other.java":  "package p;\npublic class Other {\n    int size() { return two(); }\n    int two() { return 2; }\n}\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})

	files = edit{name: "rename", replace: []sub{{"p/Base.java", "void main(String[] args)", "void launch(String[] args)"}}}.apply(t, files)
	renamed := s.only(s.advance(files))
	s.check("superclass main renamed", []linkBuild{renamed})
	s.wantRepublished("superclass main renamed", renamed, []string{"p/Base.java", "p/Child.java", "p/NoArgs.java"}, "p/Other.java")

	files = edit{name: "form", replace: []sub{{"p/Base.java", "void launch(String[] args)", "void main()"}}}.apply(t, files)
	s.check("superclass main without parameters", []linkBuild{s.only(s.advance(files))})
}
