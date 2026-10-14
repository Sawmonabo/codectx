package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

// jkind is the kind of one JSON value (jval).
type jkind uint8

const (
	jNull jkind = iota
	jBool
	jNumber
	jString
	jArray
	jObject
)

// jval is one JSON value with an object's members in source order: a
// package's `exports` and `imports` conditions match in the order the object
// writes them (Node.js "Conditional exports"), which a Go map would lose.
// An object's member i is keys[i] with value items[i]; an array's elements
// are items.
type jval struct {
	kind  jkind
	str   string
	b     bool
	keys  []string
	items []jval
}

// get is the value of an object's member key, and false when v is no
// object or has no such member. A repeated key keeps its last value, as
// JSON.parse does.
func (v jval) get(key string) (jval, bool) {
	if v.kind != jObject {
		return jval{}, false
	}
	for i, v0 := range slices.Backward(v.keys) {
		if v0 == key {
			return v.items[i], true
		}
	}
	return jval{}, false
}

// text is a string member's value, empty for any other kind or a missing
// member.
func (v jval) text(key string) string {
	m, ok := v.get(key)
	if !ok || m.kind != jString {
		return ""
	}
	return m.str
}

// strings is an array member's string elements, or a string member as one
// element; ok reports that the member is present as either.
func (v jval) strings(key string) (out []string, ok bool) {
	m, found := v.get(key)
	switch {
	case !found:
		return nil, false
	case m.kind == jString:
		return []string{m.str}, true
	case m.kind != jArray:
		return nil, false
	}
	for _, x := range m.items {
		if x.kind == jString {
			out = append(out, x.str)
		}
	}
	return out, true
}

// flag is a boolean member: 1 true, 0 false, -1 absent or not a boolean.
func (v jval) flag(key string) int8 {
	m, ok := v.get(key)
	if !ok || m.kind != jBool {
		return -1
	}
	if m.b {
		return 1
	}
	return 0
}

// parseJSONC parses data as JSON with comments: `//` and `/* */` comments and
// a trailing comma before a closing bracket or brace are allowed, as
// TypeScript reads tsconfig.json and jsconfig.json, and a leading byte order
// mark is skipped. The text is first rewritten into plain JSON of the same
// length (standardJSON), then decoded token by token so object members keep
// their order.
func parseJSONC(data []byte) (jval, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	dec := json.NewDecoder(bytes.NewReader(standardJSON(data)))
	dec.UseNumber()
	v, err := decodeJValue(dec)
	if err != nil {
		return jval{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return jval{}, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// standardJSON is data with every comment and every trailing comma replaced
// by spaces, in one pass that keeps the bytes of strings as they are; a
// newline inside a comment is kept. Plain JSON is returned unchanged.
func standardJSON(data []byte) []byte {
	out := bytes.Clone(data)
	comma := -1
	for i := 0; i < len(out); i++ {
		c := out[i]
		switch {
		case c == '"':
			comma = -1
			for i++; i < len(out) && out[i] != '"'; i++ {
				if out[i] == '\\' {
					i++
				}
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '/':
			for ; i < len(out) && out[i] != '\n'; i++ {
				out[i] = ' '
			}
		case c == '/' && i+1 < len(out) && out[i+1] == '*':
			out[i], out[i+1] = ' ', ' '
			for i += 2; i < len(out) && !(out[i] == '*' && i+1 < len(out) && out[i+1] == '/'); i++ {
				if out[i] != '\n' {
					out[i] = ' '
				}
			}
			if i < len(out) {
				out[i], out[i+1] = ' ', ' '
				i++
			}
		case c == ',':
			comma = i
		case c == '}' || c == ']':
			if comma >= 0 {
				out[comma] = ' '
			}
			comma = -1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
		default:
			comma = -1
		}
	}
	return out
}

// decodeJValue reads one value from dec.
func decodeJValue(dec *json.Decoder) (jval, error) {
	tok, err := dec.Token()
	if err != nil {
		return jval{}, err
	}
	switch t := tok.(type) {
	case nil:
		return jval{kind: jNull}, nil
	case bool:
		return jval{kind: jBool, b: t}, nil
	case json.Number:
		return jval{kind: jNumber, str: t.String()}, nil
	case string:
		return jval{kind: jString, str: t}, nil
	case json.Delim:
		switch t {
		case '[':
			v := jval{kind: jArray}
			for dec.More() {
				x, err := decodeJValue(dec)
				if err != nil {
					return jval{}, err
				}
				v.items = append(v.items, x)
			}
			_, err := dec.Token()
			return v, err
		case '{':
			v := jval{kind: jObject}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return jval{}, err
				}
				key, _ := k.(string)
				x, err := decodeJValue(dec)
				if err != nil {
					return jval{}, err
				}
				v.keys = append(v.keys, key)
				v.items = append(v.items, x)
			}
			_, err := dec.Token()
			return v, err
		}
	}
	return jval{}, errors.New("unexpected JSON token")
}

// canonicalJSON is v written back as compact JSON with members in source
// order, the stable spelling a manifest field's value hashes.
func canonicalJSON(v jval) string {
	var b bytes.Buffer
	var write func(v jval)
	write = func(v jval) {
		switch v.kind {
		case jNull:
			b.WriteString("null")
		case jBool:
			if v.b {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case jNumber:
			b.WriteString(v.str)
		case jString:
			s, _ := json.Marshal(v.str)
			b.Write(s)
		case jArray:
			b.WriteByte('[')
			for i, x := range v.items {
				if i > 0 {
					b.WriteByte(',')
				}
				write(x)
			}
			b.WriteByte(']')
		case jObject:
			b.WriteByte('{')
			for i, k := range v.keys {
				if i > 0 {
					b.WriteByte(',')
				}
				s, _ := json.Marshal(k)
				b.Write(s)
				b.WriteByte(':')
				write(v.items[i])
			}
			b.WriteByte('}')
		}
	}
	write(v)
	return b.String()
}
