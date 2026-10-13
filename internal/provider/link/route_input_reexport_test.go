package link_test

import (
	"slices"
	"testing"
)

// TestFastAPIParameterTypedThroughAPackageReexport is a handler parameter
// typed by a class the handler's file imports from a package whose
// `__init__.py` binds it by an import of its own (`from .user import User`,
// `from .item import *`): FastAPI reads the package's attribute, so the
// class is the one the re-export names. A name two star imports may supply,
// and one the package declares itself, decide nothing.
// Mutation: classOf follows no re-export -> user, other, item and tag
// publish nothing.
// Mutation: star imports not followed -> item and tag publish nothing.
// Mutation: a star import decides when several modules supply the name ->
// dup is published.
// Mutation: a name the package declares itself is followed through its
// import -> shadow is published as a body.
func TestFastAPIParameterTypedThroughAPackageReexport(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"app/schemas/user.py": "from pydantic import BaseModel\n\n\nclass User(BaseModel):\n    name: str\n",
		"app/schemas/item.py": "from pydantic import BaseModel\n\n\nclass Item(BaseModel):\n    name: str\n\n\nclass Tag:\n    pass\n",
		"app/schemas/one.py":  "from pydantic import BaseModel\n\n\nclass Dup(BaseModel):\n    pass\n",
		"app/schemas/two.py":  "class Dup:\n    pass\n",
		"app/schemas/__init__.py": `from .user import User
from .user import User as Shadow
from .item import *
from .one import *
from .two import *


def Shadow():
    pass
`,
		"app/main.py": `from fastapi import FastAPI

from . import schemas
from .schemas import User, Item

app = FastAPI()


@app.post("/items")
def create(user: User, other: schemas.User, item: Item, tag: schemas.Tag, dup: schemas.Dup,
           shadow: schemas.Shadow):
    return user
`,
	})
	got := l.linkedInputs(t)
	want := []string{
		"app/main.py create fastapi body item | item: Item",
		"app/main.py create fastapi body other | other: schemas.User",
		"app/main.py create fastapi body user | user: User",
		"app/main.py create fastapi query tag | tag: schemas.Tag",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("linked request inputs\ngot  %q\nwant %q", got, want)
	}
}
