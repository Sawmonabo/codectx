package delta_test

import "testing"

// TestModelBaseEditReresolvesRequestInputs is a FastAPI parameter typed by
// a class of another file whose base, in a third file, decides it: an edit
// that makes that base no Pydantic model refreshes to what a full build
// publishes (the parameter becomes a query read), and republishes the
// handler's file. Mutation caught: the class lookups not traced, so the
// refresh keeps the stale body read.
func TestModelBaseEditReresolvesRequestInputs(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"app/base.py":    "from pydantic import BaseModel\n\n\nclass Model(BaseModel):\n    pass\n",
		"app/schemas.py": "from .base import Model\n\n\nclass User(Model):\n    name: str\n",
		"app/main.py": "from fastapi import FastAPI\n\nfrom .schemas import User\n\napp = FastAPI()\n\n\n" +
			"@app.post(\"/u\")\ndef create(user: User):\n    return user\n",
		"app/other.py": "def one():\n    return 1\n\n\ndef two():\n    return one()\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "base", replace: []sub{{"app/base.py", "class Model(BaseModel):", "class Model:"}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("model base edited", []linkBuild{edited})
	s.wantRepublished("model base edited", edited, []string{"app/base.py", "app/main.py"}, "app/other.py")
}
