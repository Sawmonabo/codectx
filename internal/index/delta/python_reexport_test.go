package delta_test

import "testing"

// TestPackageReexportEditRebindsCalls is a call of a function and a method
// call on a class a package's `__init__.py` re-exports: an edit of the
// re-exports to other modules refreshes to what a full build publishes
// (the calls bound to the new modules' declarations) and republishes the
// calling file. Mutation caught: the package's import or re-export read
// without recording the lookup, so the refresh keeps the stale bindings.
func TestPackageReexportEditRebindsCalls(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"app/pkg/a.py":        "def f():\n    pass\n\n\nclass C:\n    def m(self):\n        pass\n",
		"app/pkg/b.py":        "def f():\n    pass\n\n\nclass C:\n    def m(self):\n        pass\n",
		"app/pkg/__init__.py": "from .a import f, C\n",
		"app/main.py":         "from app.pkg import f, C\n\n\ndef run():\n    f()\n    C().m()\n",
		"app/other.py":        "def one():\n    return 1\n\n\ndef two():\n    return one()\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "reexport", replace: []sub{{"app/pkg/__init__.py", "from .a import f, C", "from .b import f, C"}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("re-export edited", []linkBuild{edited})
}
