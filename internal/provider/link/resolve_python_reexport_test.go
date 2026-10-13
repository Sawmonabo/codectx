package link_test

import (
	"slices"
	"strings"
	"testing"
)

// TestPythonImportThroughAPackageReexport is a package whose `__init__.py`
// re-exports a class by name, a module's functions by a star import and a
// submodule, and binds one name twice; a main module imports each through
// the package. `from app.schemas import User` binds the class user.py
// declares (Python's from-import reads the package's attribute), so the
// construction, the import and a method call on the constructed value bind
// there; the name bound twice binds nothing. Goldens from a probe of the
// pinned grammar.
// Mutation: the call resolver follows no re-export -> User and helper
// unbound, main.py imports only the package.
// Mutation: no Python re-export in the type index -> u.save() open (its
// receiver's class unknown).
// Mutation: a star import not followed -> helper unbound.
// Mutation: a name two imports bind followed -> ghost bound.
func TestPythonImportThroughAPackageReexport(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"app/schemas/user.py": `class User:
    def __init__(self):
        pass

    def save(self):
        pass
`,
		"app/schemas/util.py":  "def helper():\n    pass\n",
		"app/schemas/views.py": "def index():\n    pass\n",
		"app/schemas/a.py":     "def ghost():\n    pass\n",
		"app/schemas/b.py":     "def ghost():\n    pass\n",
		"app/schemas/__init__.py": `from .user import User
from .util import *
from . import views
from .a import ghost
from .b import ghost
`,
		"app/main.py": `from app.schemas import User, helper, views, ghost
import app.schemas as schemas


def run():
    u = User()
    u.save()
    helper()
    views.index()
    schemas.helper()
    ghost()
`,
	})
	bindings := l.bindings(t)
	want := []string{
		"app/main.py:10 helper -> app/schemas/util.py:1 helper exact",
		"app/main.py:6 User -> app/schemas/user.py:2 __init__ exact",
		"app/main.py:7 save -> app/schemas/user.py:5 save exact",
		"app/main.py:8 helper -> app/schemas/util.py:1 helper exact",
		"app/main.py:9 index -> app/schemas/views.py:1 index exact",
	}
	if !slices.Equal(bindings, want) {
		t.Errorf("bindings\ngot  %q\nwant %q", bindings, want)
	}
	var imports []string
	for _, f := range l.facts(t) {
		if strings.HasPrefix(f, "app/main.py imports ") {
			imports = append(imports, f)
		}
	}
	wantImports := []string{
		"app/main.py imports app/schemas/__init__.py",
		"app/main.py imports app/schemas/user.py",
		"app/main.py imports app/schemas/util.py",
		"app/main.py imports app/schemas/views.py",
	}
	if !slices.Equal(imports, wantImports) {
		t.Errorf("imports\ngot  %q\nwant %q", imports, wantImports)
	}
}
