package treesitter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// indexed runs the structural provider over one file and returns the node
// facts it published. Every harness shares one repository identity, so the
// same path in two runs is the same file and two runs' node IDs compare.
func indexed(t *testing.T, p *treesitter.Provider, file, src string) []model.NodeFact {
	t.Helper()
	h := providertest.New(t, map[string]string{file: src})
	u := h.Plan(t, p, lang.ScopePrefix+file, []string{file})
	cap := &capture{UnitOutput: h.Begin(t, u, []string{file})}
	result, err := provider.RunUnit(context.Background(), p, u.Request, cap, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit(%s): %v", file, err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state for %s = %s, want succeeded", file, result.State)
	}
	return cap.nodes
}

// declared is every node named name that is not a variable, which is a
// declaration of the file.
func declared(nodes []model.NodeFact, name string) []model.Node {
	var out []model.Node
	for _, n := range nodes {
		if n.Node.Name == name && n.Node.Kind != model.NodeVariable {
			out = append(out, n.Node)
		}
	}
	return out
}

// within is the source a node's range selects.
func within(t *testing.T, src string, n model.Node) string {
	t.Helper()
	if n.Range == nil || int(n.Range.End.Byte) > len(src) {
		t.Fatalf("node %s has no range inside its file", n.Name)
	}
	return src[n.Range.Start.Byte:n.Range.End.Byte]
}

// byParams maps each declaration named name to its node ID by its parameter
// list as written (the text between its first parentheses), failing on two
// declarations with one list or two with one node.
func byParams(t *testing.T, src string, nodes []model.NodeFact, name string) map[string]model.NodeID {
	t.Helper()
	out := map[string]model.NodeID{}
	seen := map[model.NodeID]bool{}
	for _, n := range declared(nodes, name) {
		text := within(t, src, n)
		open, end := strings.Index(text, "("), strings.Index(text, ")")
		if open < 0 || end < open {
			t.Fatalf("%s has no parameter list: %q", name, text)
		}
		params := text[open+1 : end]
		if _, dup := out[params]; dup {
			t.Fatalf("two declarations %s(%s)", name, params)
		}
		if seen[n.ID] {
			t.Fatalf("two overloads of %s share node %s", name, n.ID)
		}
		out[params], seen[n.ID] = n.ID, true
	}
	return out
}

// TestAnInsertedOverloadKeepsEveryExistingOverloadsNode indexes a file with
// two overloads of parse, then the same file with a third inserted before
// them, in each language that overloads, and asserts that the overloads are
// three nodes and that each existing one kept its node.
//
// Mutation that fails it: disambiguate the declarations sharing a descriptor
// path by their order (the inserted overload takes the first one's number and
// renumbers the rest). Dropping the parameter types from the disambiguator
// fails it too: the duplicates are then settled by content, the inserted one
// keeps the plain identity, and the formerly first moves to a content key.
func TestAnInsertedOverloadKeepsEveryExistingOverloadsNode(t *testing.T) {
	p := newProvider(t)
	for _, c := range []struct {
		file, before, inserted, at string
		existing                   []string
		added                      string
	}{
		{file: "Parse.java",
			before:   "class Parse {\n  int parse(String s) {\n    return 1;\n  }\n\n  int parse(int n) {\n    return 2;\n  }\n}\n",
			at:       "class Parse {\n",
			inserted: "  int parse(double d) {\n    return 3;\n  }\n\n",
			existing: []string{"String s", "int n"}, added: "double d"},
		{file: "parse.cpp",
			before:   "int parse(const char* s) { return 1; }\n\nint parse(int n) { return 2; }\n",
			inserted: "int parse(double d) { return 3; }\n\n",
			existing: []string{"const char* s", "int n"}, added: "double d"},
		{file: "parse.ts",
			before:   "function parse(s: string): number;\nfunction parse(n: number): number;\nfunction parse(x: any): number {\n  return 1;\n}\n",
			inserted: "function parse(b: boolean): number;\n",
			existing: []string{"s: string", "n: number", "x: any"}, added: "b: boolean"},
	} {
		t.Run(c.file, func(t *testing.T) {
			at := len(c.at)
			if !strings.HasPrefix(c.before, c.at) {
				t.Fatalf("the insertion point %q does not begin the file", c.at)
			}
			after := c.before[:at] + c.inserted + c.before[at:]
			old := byParams(t, c.before, indexed(t, p, c.file, c.before), "parse")
			now := byParams(t, after, indexed(t, p, c.file, after), "parse")
			if len(old) != len(c.existing) || len(now) != len(c.existing)+1 || now[c.added] == "" {
				t.Fatalf("parse overloads before %v, after %v: want %v, then %q beside them", old, now, c.existing, c.added)
			}
			for _, params := range c.existing {
				if old[params] == "" || old[params] != now[params] {
					t.Errorf("parse(%s) is node %s before the insertion and %s after it", params, old[params], now[params])
				}
			}
		})
	}
}

// TestABodyEditKeepsDeclarationAndParameterNodes indexes a file in every
// language, then the same file with a longer body in its first function and
// an edited body in the second, and asserts that every declaration kept its
// node, the ones after the edit included, and so did the second function's
// parameter p.
//
// Mutations that fail it: key a declaration on its byte range (a's length
// moves a, f and every declaration after them); key a parameter on its range
// rather than its callable and position (p moves with the edit before it).
func TestABodyEditKeepsDeclarationAndParameterNodes(t *testing.T) {
	p := newProvider(t)
	for _, c := range []struct {
		file, before string
		edits        [][2]string
		names        []string
	}{
		{file: "keep.go",
			before: "package p\n\nfunc a() int { return 1 }\n\nfunc f(p int) int {\n\tx := p\n\treturn x\n}\n\ntype Later struct{}\n",
			edits:  [][2]string{{"return 1 }", "return 1 + 2 + 3 }"}, {"x := p\n", "x := p + 1\n"}},
			names:  []string{"a", "f", "Later"}},
		{file: "keep.py",
			before: "def a():\n    return 1\n\n\ndef f(p):\n    x = p\n    return x\n\n\nclass Later:\n    pass\n",
			edits:  [][2]string{{"return 1\n", "return 1 + 2 + 3\n"}, {"x = p\n", "x = p + 1\n"}},
			names:  []string{"a", "f", "Later"}},
		{file: "keep.js",
			before: "function a() {\n  return 1;\n}\n\nfunction f(p) {\n  let x = p;\n  return x;\n}\n\nclass Later {}\n",
			edits:  [][2]string{{"return 1;", "return 1 + 2 + 3;"}, {"let x = p;", "let x = p + 1;"}},
			names:  []string{"a", "f", "Later"}},
		{file: "keep.ts",
			before: "function a(): number {\n  return 1;\n}\n\nfunction f(p: number): number {\n  let x = p;\n  return x;\n}\n\nclass Later {}\n",
			edits:  [][2]string{{"return 1;", "return 1 + 2 + 3;"}, {"let x = p;", "let x = p + 1;"}},
			names:  []string{"a", "f", "Later"}},
		{file: "Keep.java",
			before: "class Host {\n  int a() {\n    return 1;\n  }\n\n  int f(int p) {\n    int x = p;\n    return x;\n  }\n}\n\nclass Later {}\n",
			edits:  [][2]string{{"return 1;", "return 1 + 2 + 3;"}, {"int x = p;", "int x = p + 1;"}},
			names:  []string{"Host", "a", "f", "Later"}},
		{file: "keep.rs",
			before: "fn a() -> i32 {\n    1\n}\n\nfn f(p: i32) -> i32 {\n    let x = p;\n    x\n}\n\nstruct Later;\n",
			edits:  [][2]string{{"    1\n", "    1 + 2 + 3\n"}, {"let x = p;", "let x = p + 1;"}},
			names:  []string{"a", "f", "Later"}},
		{file: "keep.c",
			before: "int a(void) { return 1; }\n\nint f(int p) {\n  int x = p;\n  return x;\n}\n\nstruct Later { int v; };\n",
			edits:  [][2]string{{"return 1; }", "return 1 + 2 + 3; }"}, {"int x = p;", "int x = p + 1;"}},
			names:  []string{"a", "f", "Later"}},
		{file: "keep.cpp",
			before: "int a() { return 1; }\n\nint f(int p) {\n  int x = p;\n  return x;\n}\n\nstruct Later { int v; };\n",
			edits:  [][2]string{{"return 1; }", "return 1 + 2 + 3; }"}, {"int x = p;", "int x = p + 1;"}},
			names:  []string{"a", "f", "Later"}},
	} {
		t.Run(c.file, func(t *testing.T) {
			after := c.before
			for _, e := range c.edits {
				if strings.Count(after, e[0]) != 1 {
					t.Fatalf("%q is not in the file exactly once", e[0])
				}
				after = strings.Replace(after, e[0], e[1], 1)
			}
			old, now := indexed(t, p, c.file, c.before), indexed(t, p, c.file, after)
			for _, name := range c.names {
				o, n := declared(old, name), declared(now, name)
				if len(o) != 1 || len(n) != 1 {
					t.Fatalf("%s is %d nodes before the edit and %d after it, want one each", name, len(o), len(n))
				}
				if o[0].ID != n[0].ID {
					t.Errorf("%s is node %s before the body edit and %s after it", name, o[0].ID, n[0].ID)
				}
			}
			if o, n := parameter(t, old), parameter(t, now); o != n {
				t.Errorf("the parameter p is node %s before the body edit and %s after it", o, n)
			}
		})
	}
}

// parameter is the node of the one variable named p.
func parameter(t *testing.T, nodes []model.NodeFact) model.NodeID {
	t.Helper()
	var id model.NodeID
	for _, n := range nodes {
		if n.Node.Kind != model.NodeVariable || n.Node.Name != "p" {
			continue
		}
		if id != "" && id != n.Node.ID {
			t.Fatalf("two variable nodes named p; nodes: %s", names(nodes))
		}
		id = n.Node.ID
	}
	if id == "" {
		t.Fatalf("no variable p; nodes: %s", names(nodes))
	}
	return id
}

// TestTwoArmsOfAConditionalDefinitionAreTwoStableNodes indexes a Python file
// that defines load in both arms of an if, then edits the else arm, then the
// if arm, and asserts that the two definitions are two nodes and that each
// keeps its node through both edits.
//
// Mutation that fails it: drop the enclosing branch from the disambiguator
// (the two definitions are then exact duplicates settled by content, and the
// else arm's definition, keyed by its bytes, moves when its body is edited).
func TestTwoArmsOfAConditionalDefinitionAreTwoStableNodes(t *testing.T) {
	p := newProvider(t)
	const file = "load.py"
	versions := []string{
		"import sys\n\nif sys.platform == \"win32\":\n    def load():\n        return 1\nelse:\n    def load():\n        return 2\n",
	}
	versions = append(versions, strings.Replace(versions[0], "return 2\n", "return 2 + 20\n", 1))
	versions = append(versions, strings.Replace(versions[1], "return 1\n", "return 1 + 10\n", 1))
	var ifArm, elseArm model.NodeID
	for i, src := range versions {
		arms := map[string]model.NodeID{}
		for _, n := range declared(indexed(t, p, file, src), "load") {
			switch body := within(t, src, n); {
			case strings.Contains(body, "return 1"):
				arms["if"] = n.ID
			case strings.Contains(body, "return 2"):
				arms["else"] = n.ID
			}
		}
		if len(arms) != 2 || arms["if"] == arms["else"] {
			t.Fatalf("version %d: the definitions of load are %v, want one node per arm", i, arms)
		}
		if i == 0 {
			ifArm, elseArm = arms["if"], arms["else"]
			continue
		}
		if arms["if"] != ifArm || arms["else"] != elseArm {
			t.Errorf("version %d: the arms are %s and %s, were %s and %s: an edit inside one arm moved a definition",
				i, arms["if"], arms["else"], ifArm, elseArm)
		}
	}
}
