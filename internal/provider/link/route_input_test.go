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

// TestSpringDefaultResolutionAcrossFiles is Spring's catch-all for a
// handler parameter no binding annotation marks, decided across the unit:
// a simple type and an enum of another file bind as a request parameter
// keyed by the name, a class of another file as a model attribute (no key),
// and an interface, an abstract class and a type no member declares
// publish nothing; once any file names an argument resolver, which the
// project's own resolvers run before the catch-all, nothing is published.
// Mutations caught: the enum read as a model attribute, an interface or an
// abstract class published, an undeclared type guessed, and the resolver
// gate dropped (the second unit publishes).
func TestSpringDefaultResolutionAcrossFiles(t *testing.T) {
	files := map[string]string{
		"src/demo/Color.java":  "package demo;\n\npublic enum Color { RED }\n",
		"src/demo/Filter.java": "package demo;\n\npublic class Filter {\n    String q;\n}\n",
		"src/demo/Shape.java":  "package demo;\n\npublic interface Shape {}\n",
		"src/demo/Base.java":   "package demo;\n\npublic abstract class Base {}\n",
		"src/demo/Api.java": `package demo;

import com.example.Page;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

@RestController
class Api {
    @GetMapping("/x")
    String list(String q, int size, Color color, Filter filter, Shape shape, Base base, Page page) {
        return q;
    }
}
`,
	}
	got := linkSources(t, newStructural(t), files).linkedInputs(t)
	want := []string{
		"src/demo/Api.java list spring combined  | Filter filter",
		"src/demo/Api.java list spring combined color | Color color",
		"src/demo/Api.java list spring combined q | String q",
		"src/demo/Api.java list spring combined size | int size",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("linked request inputs\ngot  %q\nwant %q", got, want)
	}
	files["src/demo/Web.java"] = "package demo;\n\nclass Web {\n    void addArgumentResolvers(java.util.List<HandlerMethodArgumentResolver> rs) {\n        done();\n    }\n\n    void done() {}\n}\n"
	if got := linkSources(t, newStructural(t), files).linkedInputs(t); len(got) != 0 {
		t.Fatalf("with an argument resolver named, linked request inputs %q, want none", got)
	}
}
