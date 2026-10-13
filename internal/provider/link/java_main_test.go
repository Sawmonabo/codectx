package link_test

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestJavaMainInheritedAcrossFiles checks that a Java class whose
// superclass chain leaves its file is launched by the main JLS §12.1.4
// chooses along the whole chain: the nearest `String[]` main, through
// several files, before a no-parameter main the class declares itself, and
// the class's own no-parameter main when its superclass is outside the
// project. The structural provider keeps the classes its own file decides
// (a `String[]` main declared nearer than the file's edge), and a chain
// with no main publishes nothing.
//
// Mutation: the structural provider hands no class off -> the linker
// publishes no main endpoint.
// Mutation: hand off a class that declares a `String[]` main -> Mid has a
// second main endpoint, the linker's.
// Mutation: choose the nearest main of any form -> NoArgs routes to its own
// main, not Base's.
// Mutation: read one superclass, not the chain -> GrandChild publishes
// nothing.
// Mutation: publish nothing when the chain leaves the project -> Lone
// publishes nothing.
func TestJavaMainInheritedAcrossFiles(t *testing.T) {
	files := map[string]string{
		"p/Base.java": `package p;
public class Base { // @base
    public static void main(String[] args) {} // @basemain
}
`,
		"p/Child.java": `package p;
public class Child extends Base {} // @child
`,
		"p/GrandChild.java": `package p;
public class GrandChild extends Child {} // @grand
`,
		"p/NoArgs.java": `package p;
public class NoArgs extends Base { // @noargs
    void main() {} // @noargsmain
}
`,
		"p/Mid.java": `package p;
public class Mid extends Base { // @mid
    public static void main(String[] a) {} // @midmain
}
`,
		"p/Leaf.java": `package p;
public class Leaf extends Mid {} // @leaf
`,
		"p/Lone.java": `package p;
public class Lone extends java.util.ArrayList<String> { // @lone
    void main() {} // @lonemain
}
`,
		"p/Quiet.java": `package p;
public class Quiet extends Empty {} // @quiet
`,
		"p/Empty.java": `package p;
public class Empty {} // @empty
`,
	}
	l := linkSources(t, newStructural(t), files)
	paths := l.paths(t)
	at := func(n model.Node) string {
		p := paths[n.FileID]
		return labelAt(p, l.files[p], int(n.Range.Start.Byte))
	}
	var linked []string
	for _, r := range l.out.rels {
		from := l.out.nodes[r.Relation.From]
		if r.Relation.Kind != model.RelRoutesTo || from.Kind != model.NodeEndpoint || from.Name != "main" {
			continue
		}
		for _, e := range r.Evidence {
			linked = append(linked, at(from)+" -> "+l.target(paths, l.out.nodes[r.Relation.To])+" "+e.Detail+setSuffix(e))
		}
	}
	slices.Sort(linked)
	want := []string{
		"child -> basemain main hierarchy",
		"grand -> basemain main hierarchy",
		"leaf -> midmain main hierarchy",
		"lone -> lonemain main hierarchy",
		"noargs -> basemain main hierarchy",
	}
	if !slices.Equal(linked, want) {
		t.Errorf("linker main endpoints:\n got %q\nwant %q", linked, want)
	}
	var structural []string
	for _, n := range l.structural.nodes {
		if n.Kind == model.NodeEndpoint && n.Name == "main" {
			structural = append(structural, at(n))
		}
	}
	slices.Sort(structural)
	if want := []string{"basemain", "midmain"}; !slices.Equal(structural, want) {
		t.Errorf("structural main endpoints at %q, want %q", structural, want)
	}
}

// TestJavaMainEndpointIsOneNodeWhicheverProviderDecides links Flip.java
// twice: declaring a no-parameter main, so the linker resolves its class's
// main through Base in another file, then a `String[]` main, so the
// structural provider decides it. The class's main endpoint is one node
// either way, so an edit that moves the decision between the providers
// moves no endpoint.
//
// Mutation: mint the linker's endpoint without the structural provider's
// descriptor and disambiguator -> the two node identities differ.
func TestJavaMainEndpointIsOneNodeWhicheverProviderDecides(t *testing.T) {
	base := "package p;\npublic class Base {\n    public static void main(String[] args) {}\n}\n"
	mainIn := func(c *capture, file model.FileID) []model.NodeID {
		var out []model.NodeID
		for _, n := range c.nodes {
			if n.Kind == model.NodeEndpoint && n.Name == "main" && n.FileID == file {
				out = append(out, n.ID)
			}
		}
		return out
	}
	linked := linkSources(t, newStructural(t), map[string]string{"p/Base.java": base,
		"p/Flip.java": "package p;\npublic class Flip extends Base {\n    void main() {}\n}\n"})
	flip := linked.h.File(t, "p/Flip.java").ID
	byLinker := mainIn(linked.out, flip)
	if len(byLinker) != 1 || len(mainIn(linked.structural, flip)) != 0 {
		t.Fatalf("Flip's main endpoints: linker %v, structural %v; want the linker's alone", byLinker, mainIn(linked.structural, flip))
	}
	own := linkSources(t, newStructural(t), map[string]string{"p/Base.java": base,
		"p/Flip.java": "package p;\npublic class Flip extends Base {\n    public static void main(String[] a) {}\n}\n"})
	byStructural := mainIn(own.structural, own.h.File(t, "p/Flip.java").ID)
	if len(byStructural) != 1 {
		t.Fatalf("Flip's structural main endpoints %v, want one", byStructural)
	}
	if byLinker[0] != byStructural[0] {
		t.Errorf("Flip's main endpoint is %s when the linker decides it and %s when the structural provider does",
			byLinker[0], byStructural[0])
	}
}
