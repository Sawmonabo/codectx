package delta_test

import "testing"

// TestPackageReexportEditReresolvesRequestInputs is a FastAPI parameter
// typed by a class a package's `__init__.py` re-exports: an edit of the
// re-export to a plain class of another module refreshes to what a full
// build publishes (the parameter becomes a query read) and republishes the
// handler's file. Mutation caught: the package's import read without
// recording the lookup, so the refresh keeps the stale body read.
func TestPackageReexportEditReresolvesRequestInputs(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"app/schemas/user.py":     "from pydantic import BaseModel\n\n\nclass User(BaseModel):\n    name: str\n",
		"app/schemas/plain.py":    "class User:\n    pass\n",
		"app/schemas/__init__.py": "from .user import User\n",
		"app/main.py": "from fastapi import FastAPI\n\nfrom .schemas import User\n\napp = FastAPI()\n\n\n" +
			"@app.post(\"/u\")\ndef create(user: User):\n    return user\n",
		"app/other.py": "def one():\n    return 1\n\n\ndef two():\n    return one()\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "reexport", replace: []sub{{"app/schemas/__init__.py", "from .user import User", "from .plain import User"}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("re-export edited", []linkBuild{edited})
	s.wantRepublished("re-export edited", edited, []string{"app/main.py", "app/schemas/__init__.py", "app/schemas/plain.py", "app/schemas/user.py"}, "app/other.py")
}
