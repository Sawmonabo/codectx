package delta_test

import (
	"bytes"
	"maps"
	"strings"
	"testing"

	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// javaAnonymousFile declares an anonymous class body and an enum constant
// with a body, each named by the offset of its name token, and a call on
// each.
const javaAnonymousFile = `package app;

public class Anon {
  enum Op {
    PLUS {
      int apply(int a) {
        return a + 1;
      }
    };

    int apply(int a) {
      return a;
    }
  }

  public static int use() {
    Runnable r = new Runnable() {
      public void run() {
        System.out.println("x");
      }
    };
    r.run();
    return Op.PLUS.apply(1);
  }
}
`

// TestShiftedCodeKeepsTableDigests builds every language's project, then
// the same project with every source file shifted (a blank line before its
// first line, and its first return statement lengthened by a space), and
// compares each member digest, the fold of a table's interface and sites
// summaries, between the two: an edit that only moves code moves no
// summary. Each refresh must also equal the full build of its tree
// (session.check), which a Java anonymous body whose synthetic type moved
// with its offset would not.
//
// Mutations that fail it: leave a range, a name range or a canonical key in
// either summary (any member's digest moves); name a Java anonymous body by
// its offset in a summary (Anon.java's digest moves).
func TestShiftedCodeKeepsTableDigests(t *testing.T) {
	for _, sc := range differentialScripts {
		t.Run(sc.name, func(t *testing.T) {
			files := map[string]string{}
			maps.Copy(files, sc.base)
			if sc.name == "java" {
				files["src/app/Anon.java"] = javaAnonymousFile
			}
			shifted := map[string]string{}
			for p, src := range files {
				if link.FamilyOf(srclang.Of(p)) != "" {
					src = "\n" + strings.Replace(src, "return ", "return  ", 1)
				}
				shifted[p] = src
			}
			s := newSession(t)
			before := s.advance(files)
			s.check("base", before)
			after := s.advance(shifted)
			s.check("shifted", after)
			for _, a := range after {
				var b *linkBuild
				for i := range before {
					if before[i].scope == a.scope {
						b = &before[i]
					}
				}
				if b == nil {
					t.Fatalf("the shifted tree has the unit %s the base tree does not", a.scope)
				}
				was, now := s.inputDigests(b.unit, files), s.inputDigests(a.unit, files)
				if len(was) == 0 {
					t.Fatalf("the unit %s recorded no member digest", a.scope)
				}
				for p, d := range was {
					if !bytes.Equal(d, now[p]) {
						t.Errorf("%s's member digest moved when its code only moved", p)
					}
				}
			}
		})
	}
}

// inputDigests is every member digest unit recorded, by the path files
// holds it under.
func (s *session) inputDigests(unit model.UnitID, files map[string]string) map[string][]byte {
	s.t.Helper()
	paths := map[string]string{}
	for p := range files {
		paths[string(model.NewFileID(s.f.repo, p))] = p
	}
	rows, err := s.db.Query(`SELECT lower(hex(file_id)), digest FROM unit_inputs WHERE unit_id = ? AND digest IS NOT NULL`,
		unitRowID(s.t, s.db, unit))
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var d []byte
		if err := rows.Scan(&id, &d); err != nil {
			s.t.Fatal(err)
		}
		out[paths[id]] = d
	}
	if err := rows.Err(); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// TestDynamicImportFragmentEditRepublishes edits only the literal fragment
// of a dynamic import, which is in neither summary: the table's digest holds
// and nothing re-resolves, but the edited file is republished from its fresh
// table, so its open import site carries the new fragment, and the refresh
// equals the full build of the same tree (session.check). The other file is
// carried.
//
// Mutations that fail it: publish a content-changed member whose digest
// held from its carried facts alone (the open import site keeps the old
// fragment and differs from the full build); republish the other file.
func TestDynamicImportFragmentEditRepublishes(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"loader.py": "import importlib\n\n\ndef load(name):\n    return importlib.import_module(\"plugins.\" + name)\n",
		"main.py":   "from loader import load\n\n\ndef main():\n    load(\"a\")\n",
	}
	s.check("base", s.advance(files))
	files = edit{name: "fragment", replace: []sub{{"loader.py", "\"plugins.\"", "\"addons.\""}}}.apply(t, files)
	b := s.only(s.advance(files))
	s.check("fragment edit", []linkBuild{b})
	var open int
	if err := s.db.QueryRow(`SELECT count(*) FROM node_facts WHERE unit_id = ? AND name = 'import'`,
		unitRowID(t, s.db, b.unit)).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 1 {
		t.Fatalf("the refreshed unit holds %d open import sites, want loader.py's one", open)
	}
	s.wantRepublished("fragment edit", b, []string{"loader.py"}, "main.py")
}
