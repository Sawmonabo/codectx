package manifest

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
)

const (
	ecosystemMaven = "maven"
	languageJava   = "java"

	maxXMLDepth    = 32
	maxXMLElements = 200000
	maxXMLText     = 4096
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
	dependencies, managed               []pomDependency
	properties                          map[string]string
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
	m, ok := parsePom(u.data)
	if !ok {
		u.malformed()
		return nil
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
		if i >= MaxDependencies {
			u.overBound()
			break
		}
		if err := u.pomEdge(ctx, pkg, model.RelDependsOn, d, mavenKind(d)); err != nil {
			return err
		}
	}
	for i, d := range m.managed {
		if i >= MaxDependencies {
			u.overBound()
			break
		}
		if err := u.pomEdge(ctx, pkg, model.RelConfigures, d, mavenKind(d), "managed", "true"); err != nil {
			return err
		}
	}
	for i, mod := range m.modules {
		if i >= MaxEntries {
			u.overBound()
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
func parsePom(data []byte) (pomModel, bool) {
	m := pomModel{properties: map[string]string{}}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var stack []string
	var text strings.Builder
	var cur *pomDependency
	var entryStart int
	elements := 0
	for {
		before := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if len(stack) > maxXMLDepth || (len(stack) == 1 && t.Name.Local != "project") {
				return m, false
			}
			if elements++; elements > maxXMLElements {
				return m, false
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
					if len(m.properties) < MaxEntries {
						m.properties[stack[2]] = val
					}
				}
			}
			stack = stack[:len(stack)-1]
			text.Reset()
		}
	}
	return m, len(stack) == 0 && elements > 0
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
