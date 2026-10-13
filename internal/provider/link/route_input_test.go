package link_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// linkedInputs is every request-input read the linker published, written
// "<file> <owner> <framework> <source> <key> | <expression>", sorted.
func (l linked) linkedInputs(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	for _, n := range l.out.nodes {
		var meta map[string]json.RawMessage
		if len(n.Metadata) == 0 || json.Unmarshal(n.Metadata, &meta) != nil || meta[model.MetaInput] == nil {
			continue
		}
		var in model.RequestInput
		if err := json.Unmarshal(meta[model.MetaInput], &in); err != nil {
			t.Fatalf("input %s: %v", n.ID, err)
		}
		if n.Kind != model.NodeVariable {
			t.Errorf("request input %q is a %s node, want variable", n.Name, n.Kind)
		}
		owners := 0
		for _, r := range l.out.rels {
			if r.Relation.Kind == model.RelReads && r.Relation.To == n.ID {
				owners++
				out = append(out, paths[n.FileID]+" "+l.out.nodes[r.Relation.From].Name+" "+in.Framework+" "+
					string(in.Source)+" "+in.Key+" | "+n.Name)
			}
		}
		if owners != 1 {
			t.Errorf("request input %q is read by %d functions, want 1", n.Name, owners)
		}
	}
	slices.Sort(out)
	return out
}

// TestFastAPIParameterTypedFromAnotherFile is FastAPI's rule for a handler
// parameter whose class another file declares: a Pydantic model there (or a
// subclass of one through a further file) is a body, a dataclass a body, an
// enum a query, a request subclass nothing, a class the walk cannot find
// nothing, and a name the route writes a path read. Mutations caught: the
// class not looked up across files (no read), the bases not followed (Admin
// a query), the route's names not consulted (item_id a query), a request
// subclass published, and an unresolvable class guessed as a query.
func TestFastAPIParameterTypedFromAnotherFile(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"app/base.py": `from pydantic import BaseModel


class Model(BaseModel):
    pass
`,
		"app/schemas.py": `import enum
from dataclasses import dataclass

from fastapi import Request

from .base import Model


class User(Model):
    name: str


class Admin(User):
    level: int


@dataclass
class Point:
    x: int


class Color(enum.Enum):
    red = "red"


class Traced(Request):
    pass
`,
		"app/main.py": `from fastapi import FastAPI

from . import schemas
from .schemas import Admin, Color, Point, Traced
from .missing import Ghost

app = FastAPI()


@app.post("/items/{item_id}")
def create(item_id: Color, user: schemas.User, admin: Admin | None, point: Point, color: Color,
           req: Traced, ghost: Ghost):
    return user
`,
	})
	got := l.linkedInputs(t)
	want := []string{
		"app/main.py create fastapi body admin | admin: Admin | None",
		"app/main.py create fastapi body point | point: Point",
		"app/main.py create fastapi body user | user: schemas.User",
		"app/main.py create fastapi path item_id | item_id: Color",
		"app/main.py create fastapi query color | color: Color",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("linked request inputs\ngot  %q\nwant %q", got, want)
	}
}
