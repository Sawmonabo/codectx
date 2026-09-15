package manifest

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

const (
	ecosystemMaven = "maven"
	languageJava   = "java"

	maxXMLDepth = 32
	maxXMLText  = 4096
)

// pomDependency is one <dependency> or the <parent> element with its span.
type pomDependency struct {
	group, artifact, version, scope, typ string
	optional                             bool
	start, end                           int
	// depth is len(stack) at the element that opened this dependency, so
	// only its direct children (len(stack) == depth+1 while that child is
	// ending) may set a coordinate.
	depth int
}

type pomModel struct {
	group, artifact, version, packaging string
	parent                              *pomDependency
	modules                             []pomEntry
	// cutProperties counts properties the entry bound kept out of the table,
	// so the unit reports the cut with its count rather than in silence.
	cutProperties         int64
	dependencies, managed []pomDependency
	properties            map[string]string
}

type pomEntry struct {
	value      string
	start, end int
}

// pom reads a pom.xml as a token stream with depth, element and text bounds
// enforced before the decoder can allocate for them. Coordinates, the parent,
// modules, dependencies, dependency management and properties are captured;
// `${property}` references are carried verbatim and marked unresolved, never
// evaluated.
func (u *unit) pom(ctx context.Context) error {
	m, ok, cutElements := parsePom(u.data, u.entries, u.xmlElements)
	if !ok {
		u.malformed()
		return nil
	}
	if m.cutProperties > 0 {
		u.overBound(BoundEntries, u.entries, int64(len(m.properties))+m.cutProperties)
	}
	if cutElements > 0 {
		// The configured element bound cut the token stream. What was parsed
		// before the cut is published; the file is reported partial with
		// CTX_RESOURCE_LIMIT rather than malformed, because a large POM is
		// large, not invalid.
		u.overBound(BoundXMLElements, u.xmlElements, cutElements)
	}
	group, version := m.group, m.version
	var inherited []string
	if m.parent != nil {
		if group == "" {
			group, inherited = m.parent.group, append(inherited, "groupId")
		}
		if version == "" {
			version, inherited = m.parent.version, append(inherited, "version")
		}
	}
	if m.artifact == "" || group == "" {
		u.malformed()
		return nil
	}
	meta := map[string]any{}
	if version != "" {
		meta["version"] = version
	}
	if m.packaging != "" {
		meta["packaging"] = m.packaging
	}
	if len(inherited) > 0 {
		meta["inherited"] = inherited
	}
	if len(m.properties) > 0 {
		meta["properties"] = m.properties
	}
	pkg, err := u.defines(ctx, model.NodePackage, ecosystemMaven+":"+group+":"+m.artifact, m.artifact, languageJava, nil, meta)
	if err != nil {
		return err
	}
	if m.parent != nil {
		if err := u.pomEdge(ctx, pkg, model.RelDependsOn, *m.parent, KindBuild, "role", "parent"); err != nil {
			return err
		}
	}
	for i, d := range m.dependencies {
		if u.cut(BoundDependencies, u.deps, int64(i+1)) {
			break
		}
		if err := u.pomEdge(ctx, pkg, model.RelDependsOn, d, mavenKind(d)); err != nil {
			return err
		}
	}
	for i, d := range m.managed {
		if u.cut(BoundDependencies, u.deps, int64(i+1)) {
			break
		}
		if err := u.pomEdge(ctx, pkg, model.RelConfigures, d, mavenKind(d), "managed", "true"); err != nil {
			return err
		}
	}
	for i, mod := range m.modules {
		if u.cut(BoundEntries, u.entries, int64(i+1)) {
			break
		}
		rng := u.rng(mod.start, mod.end)
		target, ok, err := u.pathTarget(ctx, mod.value, true, rng, model.PrecisionSyntax)
		if err != nil {
			return err
		}
		if !ok {
			u.malformedEntry()
			continue
		}
		if err := u.e.Relation(pkg.ID, model.RelBuilds, target.ID, filesystem.Attrs{Precision: model.PrecisionSyntax, Range: rng, Detail: filesystem.Detail("module", mod.value)}); err != nil {
			return err
		}
	}
	return nil
}

// pomEdge emits one Maven coordinate edge; a property reference in any
// coordinate is recorded as unresolved.
func (u *unit) pomEdge(ctx context.Context, from model.Node, rel model.RelationKind, d pomDependency, kind string, extra ...string) error {
	if d.group == "" || d.artifact == "" {
		u.malformedEntry()
		return nil
	}
	detail := append([]string{"scope", d.scope, "type", d.typ}, extra...)
	if strings.Contains(d.group+d.artifact+d.version, "${") {
		detail = append(detail, "unresolved", "property")
	}
	if d.optional {
		detail = append(detail, "optional", "true")
	}
	return u.edge(ctx, from, rel, ecosystemMaven, d.group+":"+d.artifact, languageJava, kind, d.version, u.rng(d.start, d.end), detail...)
}

// mavenKind maps a Maven scope to a dependency kind. An optional dependency
// is optional whatever its scope.
func mavenKind(d pomDependency) string {
	if d.optional {
		return KindOptional
	}
	switch d.scope {
	case "test":
		return KindTest
	case "provided", "system", "import":
		return KindBuild
	default:
		return KindRuntime
	}
}

// parsePom walks the token stream once. Offsets come from the decoder: the
// offset before a StartElement token is the start of its tag (the preceding
// character data is its own token), and the offset after an EndElement is
// the end of the element.
// parsePom's third result is the element count that crossed elements, which
// is providers.manifest.max_xml_elements, and zero when the bound did not cut
// the walk. The bound is unlimited by default: how many elements a POM has is
// a property of the repository, and a user-set value cuts the stream rather
// than refusing the file.
func parsePom(data []byte, entries, elements config.Limit) (pomModel, bool, int64) {
	m := pomModel{properties: map[string]string{}}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []string
	var text strings.Builder
	var cur *pomDependency
	var entryStart int
	var seen int64
	var cut int64
	for cut == 0 {
		before := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, false, 0
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if len(stack) > maxXMLDepth || (len(stack) == 1 && t.Name.Local != "project") {
				return m, false, 0
			}
			if seen++; elements.Exceeded(seen) {
				// Stop reading and report the count that crossed the bound.
				// Refusing here said the file was malformed, which a large
				// but perfectly valid POM is not.
				cut = seen
				break
			}
			text.Reset()
			switch strings.Join(stack, "/") {
			case "project/dependencies/dependency", "project/dependencyManagement/dependencies/dependency", "project/parent":
				cur = &pomDependency{start: before, depth: len(stack)}
			case "project/modules/module":
				entryStart = before
			}
		case xml.CharData:
			if text.Len() < maxXMLText {
				text.Write(t)
			}
		case xml.EndElement:
			p := strings.Join(stack, "/")
			val := strings.TrimSpace(text.String())
			end := int(dec.InputOffset())
			switch p {
			case "project/groupId":
				m.group = val
			case "project/artifactId":
				m.artifact = val
			case "project/version":
				m.version = val
			case "project/packaging":
				m.packaging = val
			case "project/modules/module":
				m.modules = append(m.modules, pomEntry{val, entryStart, end})
			case "project/parent":
				cur.end = end
				m.parent, cur = cur, nil
			case "project/dependencies/dependency":
				cur.end = end
				m.dependencies, cur = append(m.dependencies, *cur), nil
			case "project/dependencyManagement/dependencies/dependency":
				cur.end = end
				m.managed, cur = append(m.managed, *cur), nil
			default:
				switch {
				// Only a direct child of the open <dependency>/<parent>
				// carries its coordinates: a <groupId> nested inside
				// <exclusions><exclusion> describes the exclusion, and
				// letting it through would overwrite the real dependency.
				case cur != nil && len(stack) == cur.depth+1:
					setCoordinate(cur, stack[len(stack)-1], val)
				case len(stack) == 3 && stack[1] == "properties":
					if entries.Exceeded(int64(len(m.properties) + 1)) {
						m.cutProperties++
					} else {
						m.properties[stack[2]] = val
					}
				}
			}
			stack = stack[:len(stack)-1]
			text.Reset()
		}
	}
	if cut > 0 {
		// A cut stream leaves the element stack open, which is the
		// well-formedness test below. The file parsed cleanly up to the
		// bound, so it is valid and partial, not malformed.
		return m, true, cut
	}
	return m, len(stack) == 0 && seen > 0, 0
}

func setCoordinate(d *pomDependency, field, val string) {
	switch field {
	case "groupId":
		d.group = val
	case "artifactId":
		d.artifact = val
	case "version":
		d.version = val
	case "scope":
		d.scope = val
	case "type":
		d.typ = val
	case "optional":
		d.optional, _ = strconv.ParseBool(val)
	}
}
