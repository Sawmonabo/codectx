package link

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// External types and methods (ADR-0015 decision 2). A supertype outside the
// indexed project, and a method of one, get a structural node so that a
// hierarchy relation into it has an endpoint and a call on a receiver of that
// type and an override of that method meet at one node. The node is keyed by
// what both sides can spell exactly: the language family, the type's
// qualified name as the family resolves it (hierarchy.externalName) and, for
// a method, the method's name. A call site does not know the parameter types
// an external method declares, so an external method's node is one per type
// and method name, its overloads included.
//
// The node has no file: one external node is named from every file of every
// unit that reaches it, so it is minted with a repository-wide scope
// (externalScope) and republished, with evidence of its own, by every run of
// every unit that targets it.

// externalScope is the scope key every external node is minted in, so the
// node a call in one unit targets is the node an override in another
// targets.
const externalScope = "link:external"

// externalType is the External key of the type qualified, outside the
// project, of the language family family, as a node of kind kind: an
// interface when the source proves it one (a Java or TypeScript
// `implements`, a Java interface's `extends`, a Go interface embedding, a
// Rust trait), a class otherwise.
func externalType(family string, kind model.NodeKind, qualified string) string {
	return "t\x00" + family + "\x00" + string(kind) + "\x00" + qualified
}

// externalMethod is the External key of the method named method of the type
// qualified, outside the project, of the language family family.
func externalMethod(family, qualified, method string) string {
	return "m\x00" + family + "\x00" + qualified + "\x00" + method
}

// externalLanguage is the language an external node of family is published
// in: the family's own name, spelled as a language.
func externalLanguage(family string) string {
	switch family {
	case "js":
		return "javascript"
	}
	return family
}

// externalCandidate is the node candidate of an External key: a type or
// method node keyed by its scope, qualified name, kind and language and no
// file (reconcile.CanonicalKey's structural case), with its descriptor set to
// that qualified name. ok is false for a key no externalType or
// externalMethod wrote.
func externalCandidate(key string) (c model.NodeCandidate, ok bool) {
	parts := strings.Split(key, "\x00")
	c = model.NodeCandidate{ProviderID: ProviderID, ScopeKey: externalScope}
	switch {
	case len(parts) == 4 && parts[0] == "t":
		kind := model.NodeKind(parts[2])
		if !kind.Valid() || parts[3] == "" {
			return model.NodeCandidate{}, false
		}
		c.Kind, c.Language, c.QualifiedName = kind, externalLanguage(parts[1]), parts[3]
		c.Name = lastQualified(parts[3])
	case len(parts) == 4 && parts[0] == "m":
		if parts[2] == "" || parts[3] == "" {
			return model.NodeCandidate{}, false
		}
		c.Kind, c.Language, c.Name = model.NodeMethod, externalLanguage(parts[1]), parts[3]
		sep := "."
		if parts[1] == "c" || parts[1] == "rust" {
			sep = "::"
		}
		c.QualifiedName = parts[2] + sep + parts[3]
	default:
		return model.NodeCandidate{}, false
	}
	c.NativeKey, c.Descriptor = "external:"+c.QualifiedName, c.QualifiedName
	return c, true
}

// lastQualified is the last component of a qualified name, whichever of the
// families' separators (".", "::", "/") it is written with.
func lastQualified(q string) string {
	cut := 0
	for _, sep := range []string{".", "::", "/"} {
		if i := strings.LastIndex(q, sep); i >= 0 && i+len(sep) > cut {
			cut = i + len(sep)
		}
	}
	return q[cut:]
}
