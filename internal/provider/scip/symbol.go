package scip

import (
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// descriptorSuffix is the SCIP descriptor grammar's kind marker: the last
// descriptor of a symbol says whether it names a namespace, type, term,
// method, type parameter, parameter, meta entity or macro.
type descriptorSuffix byte

const (
	suffixNone descriptorSuffix = iota
	suffixNamespace
	suffixType
	suffixTerm
	suffixMethod
	suffixTypeParameter
	suffixParameter
	suffixMeta
	suffixMacro
)

// symbol is a parsed SCIP symbol string (the grammar in scip.proto):
//
//	<scheme> ' ' <manager> ' ' <package-name> ' ' <version> ' ' <descriptor>+
//	'local ' <local-id>
//
// Spaces inside the four leading components are escaped as double spaces.
// Descriptors are kept as their raw string: it is the package-qualified name
// of the entity and the value every SCIP producer agrees on.
type symbol struct {
	raw   string
	local bool
	// scheme, manager, name and version are the unescaped leading components
	// of a global symbol; "." stands for an empty manager, name or version.
	scheme, manager, name, version string
	// descriptors is the raw descriptor string; lastName and lastSuffix
	// describe its final descriptor, prevSuffix the one before it.
	descriptors string
	lastName    string
	lastSuffix  descriptorSuffix
	prevSuffix  descriptorSuffix
}

// parseSymbol parses s; a string that is not a SCIP symbol is malformed
// provider output.
func parseSymbol(s string) (symbol, error) {
	if s == "" || len(s) > model.MaxNativeKeyBytes {
		return symbol{}, malformed("symbol is empty or over " + strconv.Itoa(model.MaxNativeKeyBytes) + " bytes")
	}
	if rest, ok := strings.CutPrefix(s, "local "); ok {
		if rest == "" || strings.ContainsRune(rest, ' ') {
			return symbol{}, malformed("local symbol has no identifier")
		}
		return symbol{raw: s, local: true, lastName: rest, lastSuffix: suffixTerm}, nil
	}
	sym := symbol{raw: s}
	rest := s
	var ok bool
	for _, dst := range []*string{&sym.scheme, &sym.manager, &sym.name, &sym.version} {
		if *dst, rest, ok = splitComponent(rest); !ok {
			return symbol{}, malformed("symbol lacks its scheme, package or descriptors")
		}
	}
	if sym.scheme == "" || rest == "" {
		return symbol{}, malformed("symbol lacks its scheme or descriptors")
	}
	sym.descriptors = rest
	if err := sym.parseDescriptors(); err != nil {
		return symbol{}, err
	}
	return sym, nil
}

// splitComponent cuts one space-terminated component, unescaping doubled
// spaces. ok is false when no terminating single space exists.
func splitComponent(s string) (component, rest string, ok bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == ' ' {
			b.WriteByte(' ')
			i++
			continue
		}
		return b.String(), s[i+1:], true
	}
	return "", "", false
}

// parseDescriptors walks the descriptor string and records the last two
// suffixes and the last name. Names may be backtick-escaped with doubled
// backticks inside.
func (sym *symbol) parseDescriptors() error {
	d := sym.descriptors
	for i := 0; i < len(d); {
		var name string
		var suffix descriptorSuffix
		switch d[i] {
		case '[':
			end, err := nameEnd(d, i+1, ']')
			if err != nil {
				return err
			}
			name, suffix, i = unescapeName(d[i+1:end]), suffixTypeParameter, end+1
		case '(':
			end, err := nameEnd(d, i+1, ')')
			if err != nil {
				return err
			}
			name, suffix, i = unescapeName(d[i+1:end]), suffixParameter, end+1
		default:
			end, err := nameEnd(d, i, 0)
			if err != nil {
				return err
			}
			name = unescapeName(d[i:end])
			if end >= len(d) {
				return malformed("descriptor " + name + " has no suffix")
			}
			switch d[end] {
			case '/':
				suffix, i = suffixNamespace, end+1
			case '#':
				suffix, i = suffixType, end+1
			case '.':
				suffix, i = suffixTerm, end+1
			case ':':
				suffix, i = suffixMeta, end+1
			case '!':
				suffix, i = suffixMacro, end+1
			case '(':
				close := strings.IndexByte(d[end:], ')')
				if close < 0 || end+close+1 >= len(d) || d[end+close+1] != '.' {
					return malformed("method descriptor is not terminated by ().")
				}
				suffix, i = suffixMethod, end+close+2
			default:
				return malformed("descriptor has an unknown suffix " + strconv.QuoteRune(rune(d[end])))
			}
		}
		if name == "" {
			return malformed("descriptor has an empty name")
		}
		sym.prevSuffix, sym.lastSuffix, sym.lastName = sym.lastSuffix, suffix, name
	}
	if sym.lastSuffix == suffixNone {
		return malformed("symbol has no descriptors")
	}
	return nil
}

// nameEnd returns the index just past the identifier starting at i. A
// backtick-escaped identifier runs to its closing single backtick; a simple
// identifier runs to the first non-identifier character, or to close when
// one is given.
func nameEnd(d string, i int, close byte) (int, error) {
	if i < len(d) && d[i] == '`' {
		for j := i + 1; j < len(d); j++ {
			if d[j] != '`' {
				continue
			}
			if j+1 < len(d) && d[j+1] == '`' {
				j++
				continue
			}
			if close != 0 {
				if j+1 >= len(d) || d[j+1] != close {
					return 0, malformed("escaped descriptor is not closed")
				}
				return j + 1, nil
			}
			return j + 1, nil
		}
		return 0, malformed("escaped identifier is not terminated")
	}
	j := i
	for j < len(d) && isIdentifierByte(d[j]) {
		j++
	}
	if close != 0 {
		if j >= len(d) || d[j] != close {
			return 0, malformed("descriptor is not closed by " + string(close))
		}
	}
	return j, nil
}

func isIdentifierByte(b byte) bool {
	return b == '_' || b == '+' || b == '-' || b == '$' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// unescapeName strips the backticks of an escaped identifier.
func unescapeName(s string) string {
	if len(s) >= 2 && s[0] == '`' && s[len(s)-1] == '`' {
		return strings.ReplaceAll(s[1:len(s)-1], "``", "`")
	}
	return s
}

// scopeKey is ruling R9-1: a local symbol is scoped to its document, a global
// one to its SCIP package descriptor (manager, name, version).
func (sym symbol) scopeKey(path string) string {
	if sym.local {
		return "file:" + path
	}
	return "pkg:" + sym.manager + " " + sym.name + " " + sym.version
}

// nodeKind maps a SymbolInformation.Kind to the Section 9.2 vocabulary,
// falling back to the descriptor suffix when the indexer did not say. Named
// types without a finer kind (Type, TypeAlias, AssociatedType, TypeFamily,
// TypeParameter) map to class; the vocabulary has no bare type kind and a
// class is the nearest declaration kind. Documented in docs/providers-scip.md.
func (sym symbol) nodeKind(kind int32) model.NodeKind {
	switch kind {
	case 7, 33, 75, 85, 86, 54, 55, 3, 57, 58, 10: // Class Object SingletonClass Mixin Concept Type TypeAlias AssociatedType TypeFamily TypeParameter DataFamily
		return model.NodeClass
	case 21, 42, 53, 56: // Interface Protocol Trait TypeClass
		return model.NodeInterface
	case 49, 59, 28: // Struct Union Message
		return model.NodeStruct
	case 11: // Enum
		return model.NodeEnum
	case 12, 8: // EnumMember Constant
		return model.NodeConstant
	case 15, 41, 79, 81, 4, 77, 22: // Field Property StaticField StaticProperty Attribute StaticDataMember Key
		return model.NodeField
	case 17, 25, 9, 24, 51: // Function Macro Constructor Lemma Theorem
		return model.NodeFunction
	case 26, 66, 80, 18, 45, 72, 74, 27, 67, 68, 69, 76, 70, 71, 34, 73: // method-like kinds
		return model.NodeMethod
	case 61, 37, 44, 52, 82, 60, 20: // Variable Parameter SelfParameter ThisParameter StaticVariable Value Instance
		return model.NodeVariable
	case 35, 36, 64: // Package PackageObject Library
		return model.NodePackage
	case 29: // Module
		return model.NodeModule
	case 30: // Namespace
		return model.NodeNamespace
	case 16: // File
		return model.NodeFile
	}
	switch sym.lastSuffix {
	case suffixNamespace:
		return model.NodeNamespace
	case suffixType:
		return model.NodeClass
	case suffixMethod:
		if sym.prevSuffix == suffixType {
			return model.NodeMethod
		}
		return model.NodeFunction
	case suffixMeta:
		return model.NodeField
	case suffixMacro:
		return model.NodeFunction
	default:
		return model.NodeVariable
	}
}
