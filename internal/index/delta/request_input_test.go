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

// TestSpringDefaultResolutionRefreshes is Spring's catch-all decided across
// files under edits: an enum of another file turned into a class, the class
// made abstract, then a file that names an argument resolver added, each refresh equal to a full
// build. Mutation caught: the type-kind or resolver lookups not traced, so
// a refresh keeps the stale request parameter or publishes past the
// project's resolver.
func TestSpringDefaultResolutionRefreshes(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"src/demo/Color.java": "package demo;\n\npublic enum Color { RED }\n",
		"src/demo/Api.java": "package demo;\n\nimport org.springframework.web.bind.annotation.GetMapping;\n" +
			"import org.springframework.web.bind.annotation.RestController;\n\n@RestController\nclass Api {\n" +
			"    @GetMapping(\"/x\")\n    String list(Color color) {\n        return name();\n    }\n\n    String name() {\n        return \"\";\n    }\n}\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "class", replace: []sub{{"src/demo/Color.java", "public enum Color { RED }", "public class Color {}"}}}.apply(t, files)
	s.check("enum turned class", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "abstract", replace: []sub{{"src/demo/Color.java", "public class Color {}", "public abstract class Color {}"}}}.apply(t, files)
	s.check("class made abstract", []linkBuild{s.only(s.advance(files))})
	files["src/demo/Web.java"] = "package demo;\n\nclass Web {\n    void add(java.util.List<HandlerMethodArgumentResolver> rs) {\n        done();\n    }\n\n    void done() {}\n}\n"
	s.check("resolver added", []linkBuild{s.only(s.advance(files))})
}
