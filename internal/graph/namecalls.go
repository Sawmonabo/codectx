package graph

import (
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The name-only matching rules of ADR-0015 decision 3, shared by both
// directions: the candidates of a site (callees, Candidates) and the sites a
// declaration is a candidate of (callers, references, impact).

// nodeMeta is the part of a node's metadata the matching reads.
type nodeMeta struct {
	Resolution model.NameResolution `json:"resolution"`
	Reason     model.OpenSiteReason `json:"reason"`
	Shape      model.CallShape      `json:"shape"`
	Arguments  int64                `json:"arguments"`
	Named      bool                 `json:"named"`
	Callee     *string              `json:"callee"`
	Fragments  *[]string            `json:"fragments"`
	// FragmentsRows is set instead of Fragments when the site's fragment
	// list is stored as metadata rows (model.SplitMetadata).
	FragmentsRows *int64 `json:"fragments_rows"`
}

func readMeta(n model.Node) (nodeMeta, bool) {
	var m nodeMeta
	if len(n.Metadata) == 0 || json.Unmarshal(n.Metadata, &m) != nil {
		return nodeMeta{}, false
	}
	return m, true
}

// openSiteMeta reads an open call site's metadata: unresolved, with a shape.
// The structural `call:<name>` placeholder is unresolved too, but it carries a
// `callee` and no shape, and an open import site carries fragments.
func openSiteMeta(n model.Node) (nodeMeta, bool) {
	m, ok := readMeta(n)
	if !ok || m.Resolution != model.ResolutionUnresolved || !m.Shape.Valid() || m.Fragments != nil || m.FragmentsRows != nil {
		return nodeMeta{}, false
	}
	return m, true
}

// isOpenCallSite reports whether n is an open call site.
func isOpenCallSite(n model.Node) bool {
	_, ok := openSiteMeta(n)
	return ok
}

// openImportFragments reads an open import site's inline fragments, or, when
// they are stored as metadata rows, how many rows hold them (rows > 0, nil
// fragments). ok is false for a node that is not an open import site.
func openImportFragments(n model.Node) (fragments []string, rows int64, ok bool) {
	m, read := readMeta(n)
	if !read || m.Resolution != model.ResolutionUnresolved || m.Reason != model.ReasonComputed {
		return nil, 0, false
	}
	switch {
	case m.Fragments != nil:
		return *m.Fragments, 0, true
	case m.FragmentsRows != nil && *m.FragmentsRows > 0:
		return nil, *m.FragmentsRows, true
	}
	return nil, 0, false
}

// isCallPlaceholder reports whether n is the structural provider's
// `call:<name>` placeholder: a node that names a callee and declares nothing.
func isCallPlaceholder(n model.Node) bool {
	m, ok := readMeta(n)
	return ok && m.Callee != nil && !m.Shape.Valid()
}

// isDeclared reports whether n declares something: it lives in a file and was
// not minted to stand for a name -- an open site, a placeholder or an
// external type carries a resolution, a declaration none.
func isDeclared(n model.Node) bool {
	if n.FileID == "" {
		return false
	}
	m, ok := readMeta(n)
	return !ok || m.Resolution == ""
}

// languageFamily is the family a language's names are visible across: C and
// C++ call each other, JavaScript and TypeScript import each other; every
// other language is its own family.
func languageFamily(language string) string {
	switch language {
	case "c", "cpp":
		return "c"
	case "javascript", "typescript", "tsx":
		return "javascript"
	}
	return language
}

// staticFamily reports whether a family checks argument counts at compile
// time, so a declaration whose parameters cannot take the site's arguments is
// not a candidate.
func staticFamily(family string) bool {
	switch family {
	case "c", "go", "java", "rust":
		return true
	}
	return false
}

// fits reports whether declaration n is a candidate of the site: the same
// name, a kind the shape can call, the same language family and, in a static
// family, a parameter list that takes the site's arguments.
func (s callSite) fits(n model.Node) bool {
	if n.Name != s.Name || !isDeclared(n) || n.ID == s.ID {
		return false
	}
	if s.Reason == model.ReasonReflective {
		// A reflective call passes the method's arguments through the
		// API's own (`invoke(obj, args)`, `Call(args)`): its count says
		// nothing of the method's arity.
		return slices.Contains(shapeKinds(s.Shape), n.Kind) && languageFamily(n.Language) == languageFamily(s.Language)
	}
	if !slices.Contains(shapeKinds(s.Shape), n.Kind) {
		return false
	}
	family := languageFamily(s.Language)
	if languageFamily(n.Language) != family {
		return false
	}
	if staticFamily(family) && n.Kind != model.NodeClass && n.Kind != model.NodeStruct {
		lo, hi, ok := arity(n.Signature, n.Name, s.Shape == model.CallShapeMember)
		if ok && (s.Arguments < lo || (hi >= 0 && s.Arguments > hi)) {
			return false
		}
	}
	return true
}

// closeness is the step of declaration d relative to the site.
func (s callSite) closeness(d Declaration) int {
	switch {
	case s.FileID != "" && d.Node.FileID == s.FileID:
		return 0
	case s.Dir != "" && d.Path != "" && path.Dir(d.Path) == s.Dir:
		return 1
	case s.Linked[d.Node.FileID]:
		return 2
	case s.Unit != "" && d.Unit == s.Unit:
		return 3
	}
	return 4
}

// arity reads the parameter count a declaration's signature takes: lo is the
// required count and hi the most it takes, -1 for no bound (a variadic
// parameter). ok is false when the signature does not show a parameter list
// after the name -- a truncated or unusual one -- and then the declaration is
// kept, because a wrong drop would turn a possible caller into none.
//
// The list is the first parenthesis after the name, so a Go receiver before
// the name is skipped; a C `(void)` takes none; a parameter carrying a default
// (`= value`) is optional; `...` is variadic; and a Rust `self` receiver is
// the receiver of a member call, not an argument.
func arity(signature, name string, member bool) (lo, hi int64, ok bool) {
	at := nameIndex(signature, name)
	if at < 0 {
		return 0, 0, false
	}
	rest := signature[at+len(name):]
	open := strings.IndexByte(rest, '(')
	if open < 0 {
		return 0, 0, false
	}
	// Generic parameters between the name and the list (`foo<T>(`) are
	// skipped; anything else there means this is not the parameter list.
	if between := strings.TrimSpace(rest[:open]); between != "" && !strings.HasPrefix(between, "<") {
		return 0, 0, false
	}
	params, closed := splitParams(rest[open+1:])
	if !closed {
		return 0, 0, false
	}
	if len(params) == 1 && (params[0] == "" || params[0] == "void") {
		return 0, 0, true
	}
	variadic := false
	for i, p := range params {
		switch {
		case p == "":
			return 0, 0, false
		case i == 0 && member && rustSelf(p):
			continue
		case strings.Contains(p, "..."):
			variadic = true
		case strings.Contains(p, "="):
			hi++
		default:
			lo++
			hi++
		}
	}
	if variadic {
		hi = -1
	}
	return lo, hi, true
}

// nameIndex finds name in signature as a whole word.
func nameIndex(signature, name string) int {
	for from := 0; from < len(signature); {
		i := strings.Index(signature[from:], name)
		if i < 0 {
			return -1
		}
		i += from
		end := i + len(name)
		if (i == 0 || !wordByte(signature[i-1])) && (end == len(signature) || !wordByte(signature[end])) {
			return i
		}
		from = i + 1
	}
	return -1
}

func wordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// splitParams splits a parameter list at its top-level commas, up to the
// parenthesis that closes it, trimming each part.
func splitParams(s string) ([]string, bool) {
	var out []string
	depth, angle, start := 0, 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 && s[i] == ')' {
				return append(out, strings.TrimSpace(s[start:i])), true
			}
			depth--
		case '<':
			angle++
		case '>':
			if angle > 0 && (i == 0 || s[i-1] != '-') {
				angle--
			}
		case ',':
			if depth == 0 && angle == 0 {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return nil, false
}

// rustSelf reports whether p is a Rust method receiver.
func rustSelf(p string) bool {
	p = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(p, "&"), "mut "))
	if strings.HasPrefix(p, "'") {
		if sp := strings.IndexByte(p, ' '); sp > 0 {
			p = strings.TrimSpace(strings.TrimPrefix(p[sp+1:], "mut "))
		}
	}
	return p == "self" || strings.HasPrefix(p, "self:") || strings.HasPrefix(p, "self :")
}
