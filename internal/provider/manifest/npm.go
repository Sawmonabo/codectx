package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	ecosystemNPM = "npm"
	languageJS   = "javascript"
	// maxJSONDepth bounds the token walk that locates dependency ranges.
	maxJSONDepth = 64
)

// npmSections maps a package.json dependency table to its kind.
var npmSections = []struct{ key, kind string }{
	{"dependencies", KindRuntime},
	{"devDependencies", KindDev},
	{"peerDependencies", KindPeer},
	{"optionalDependencies", KindOptional},
}

// packageJSON reads a package.json with the standard decoder. Scripts are
// recorded as metadata and never executed; the workspaces list is metadata
// because expanding its globs would need a repository listing.
func (u *unit) packageJSON(ctx context.Context) error {
	var pkg struct {
		Name       string            `json:"name"`
		Version    string            `json:"version"`
		Private    bool              `json:"private"`
		Workspaces json.RawMessage   `json:"workspaces"`
		Scripts    map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(u.data, &pkg); err != nil {
		u.malformed()
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(u.data, &members); err != nil {
		u.malformed()
		return nil
	}
	sections := map[string]map[string]json.RawMessage{}
	for _, sec := range npmSections {
		raw, ok := members[sec.key]
		if !ok {
			continue
		}
		var deps map[string]json.RawMessage
		if err := json.Unmarshal(raw, &deps); err != nil {
			// A dependency table that is not an object is one unusable entry,
			// not a malformed manifest.
			u.malformedEntry()
			continue
		}
		sections[sec.key] = deps
	}
	keys := make([]string, len(npmSections))
	for i, sec := range npmSections {
		keys[i] = sec.key
	}
	spans, err := jsonSpans(u.data, keys)
	if err != nil {
		u.malformed()
		return nil
	}
	rel := u.e.File().Path
	name, qualified := pkg.Name, ecosystemNPM+":"+pkg.Name
	if name == "" {
		// An unnamed private manifest is identified by where it lives.
		name, qualified = path.Base(path.Dir(rel)), ecosystemNPM+":"+rel
		if path.Dir(rel) == "." {
			name = "."
		}
	}
	meta := map[string]any{}
	if pkg.Version != "" {
		meta["version"] = pkg.Version
	}
	if pkg.Private {
		meta["private"] = true
	}
	if len(pkg.Workspaces) > 0 {
		meta["workspaces"] = pkg.Workspaces
	}
	if len(pkg.Scripts) > 0 {
		meta["scripts"] = pkg.Scripts
	}
	var nameRange *model.SourceRange
	if s, ok := spans.top["name"]; ok {
		nameRange = u.rng(s.start, s.end)
	}
	node, err := u.defines(ctx, model.NodePackage, qualified, name, languageJS, nameRange, meta)
	if err != nil {
		return err
	}
	total := 0
	for _, sec := range npmSections {
		deps := sections[sec.key]
		names := make([]string, 0, len(deps))
		for n := range deps {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if total++; u.cut(BoundDependencies, u.deps, int64(total)) {
				return nil
			}
			var req string
			if err := json.Unmarshal(deps[n], &req); err != nil {
				u.malformedEntry()
				continue
			}
			var rng *model.SourceRange
			if s, ok := spans.nested[sec.key][n]; ok {
				rng = u.rng(s.start, s.end)
			}
			if err := u.edge(ctx, node, model.RelDependsOn, ecosystemNPM, n, languageJS, sec.kind, req, rng); err != nil {
				return err
			}
		}
	}
	return nil
}

// span is a half-open byte interval.
type span struct{ start, end int }

// jsonLayout holds the byte spans of a JSON manifest's top-level members and
// of the members of its dependency tables, from key to value end.
type jsonLayout struct {
	top    map[string]span
	nested map[string]map[string]span
}

// jsonSpans walks the document's tokens once, recording the member spans of
// the top-level objects named by tables. encoding/json reports the offset
// after each token; the key's opening quote is found by scanning back from
// its closing quote, which is exact because a JSON string ends at the first
// unescaped quote.
func jsonSpans(data []byte, tables []string) (jsonLayout, error) {
	out := jsonLayout{top: map[string]span{}, nested: map[string]map[string]span{}}
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return out, malformedJSON(err)
	}
	wanted := map[string]bool{}
	for _, key := range tables {
		wanted[key] = true
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return out, malformedJSON(err)
		}
		if tok == json.Delim('}') {
			return out, nil
		}
		key, ok := tok.(string)
		if !ok {
			return out, malformedJSON(nil)
		}
		start := keyStart(data, int(dec.InputOffset()))
		if wanted[key] {
			inner, err := innerSpans(dec, data)
			if err != nil {
				return out, err
			}
			if inner != nil {
				out.nested[key] = inner
			}
		} else if err := skipValue(dec, 1); err != nil {
			return out, err
		}
		out.top[key] = span{start, int(dec.InputOffset())}
	}
}

// innerSpans records the member spans of an object value, or skips a value
// of another type.
func innerSpans(dec *json.Decoder, data []byte) (map[string]span, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, malformedJSON(err)
	}
	if tok != json.Delim('{') {
		if d, ok := tok.(json.Delim); ok && d == '[' {
			return nil, skipRest(dec, 2)
		}
		return nil, nil
	}
	out := map[string]span{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, malformedJSON(err)
		}
		if tok == json.Delim('}') {
			return out, nil
		}
		key, ok := tok.(string)
		if !ok {
			return nil, malformedJSON(nil)
		}
		start := keyStart(data, int(dec.InputOffset()))
		if err := skipValue(dec, 2); err != nil {
			return nil, err
		}
		out[key] = span{start, int(dec.InputOffset())}
	}
}

// skipValue consumes one value of any type. depth is the nesting depth of
// the object or array holding the value, the document's top-level object
// being 1.
func skipValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return malformedJSON(err)
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		return skipRest(dec, depth+1)
	}
	return nil
}

// skipRest consumes tokens until the container just opened closes. depth
// is that container's nesting depth; the parser bound applies to every
// container opened inside it.
func skipRest(dec *json.Decoder, depth int) error {
	for open := 1; open > 0; {
		if depth+open-1 > maxJSONDepth {
			return &model.Error{Code: model.CodeResourceLimit, Message: "the JSON manifest nests deeper than the parser bound"}
		}
		tok, err := dec.Token()
		if err != nil {
			return malformedJSON(err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				open++
			default:
				open--
			}
		}
	}
	return nil
}

// keyStart returns the offset of the opening quote of the string token that
// ends at end (the offset after its closing quote).
func keyStart(data []byte, end int) int {
	i := end - 1
	for j := i - 1; j >= 0; j-- {
		if data[j] != '"' {
			continue
		}
		slashes := 0
		for k := j - 1; k >= 0 && data[k] == '\\'; k-- {
			slashes++
		}
		if slashes%2 == 0 {
			return j
		}
	}
	return i
}

func malformedJSON(err error) error {
	if err == nil || err == io.EOF {
		return &model.Error{Code: model.CodeArgumentInvalid, Message: "the JSON manifest is not an object"}
	}
	return &model.Error{Code: model.CodeArgumentInvalid, Message: "the JSON manifest is malformed: " + err.Error()}
}
