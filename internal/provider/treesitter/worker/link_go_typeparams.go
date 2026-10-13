package worker

import "sync"

// goTypeParams lets a Go type name that denotes a type parameter reduce to
// what the parameter's declaration says, never to a package-level type of the
// same name. A type parameter is declared by a function's or method's type
// parameter list, by a generic type's, and, for a method of a generic type,
// by the type arguments of its receiver ("Method declarations": the receiver
// declares the receiver type's parameters under names of its own). Its scope
// is the declaration that lists it, signature and body ("Declarations and
// scope"), so the declaration enclosing a use is found by walking the use's
// ancestors.
//
// A value whose type is a type parameter has the methods of the parameter's
// constraint ("Method sets"). A constraint of one named type, `[T I]`, so
// reduces to I; any other constraint (a union, an approximation `~T`, an
// interface literal) names no one type, and the parameter reduces to nothing,
// a type the tables do not know. A receiver type parameter takes the
// constraint the generic type's own declaration gives it, when this file
// declares that type, and reduces to nothing otherwise.
type goTypeParams struct {
	// specs are the file's generic type declarations by name, gathered on
	// the first receiver type parameter looked up.
	specs map[string]Node
	// raw is set while a constraint is reduced: Go forbids a type parameter
	// as a constraint, so a constraint's names are never type parameters,
	// and a malformed `[T T]` cannot recurse.
	raw bool
}

type goTypeParamSyntax struct {
	typeDeclaration, typeParameterList, typeParameterDeclaration, typeConstraint, typeArguments, typeElem uint16
	// Unregistered: read from the child's record only.
	fTypeParameters, fTypeArguments uint16
}

var (
	goTypeParamSyntaxOnce  sync.Once
	goTypeParamSyntaxTable *goTypeParamSyntax
)

func goTypeParamSyntaxOf() *goTypeParamSyntax {
	goTypeParamSyntaxOnce.Do(func() {
		const language = "go"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		raw := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the " + language + " grammar has no field " + name)
			}
			return v
		}
		goTypeParamSyntaxTable = &goTypeParamSyntax{
			typeDeclaration: kind("type_declaration"), typeParameterList: kind("type_parameter_list"),
			typeParameterDeclaration: kind("type_parameter_declaration"), typeConstraint: kind("type_constraint"),
			typeArguments: kind("type_arguments"), typeElem: kind("type_elem"),
			fTypeParameters: raw("type_parameters"), fTypeArguments: raw("type_arguments"),
		}
	})
	return goTypeParamSyntaxTable
}

// typeName is what the type name t reduces to: the reduction of the type
// parameter it denotes (see goTypeParams), or its own text.
func (x *goLink) typeName(t Node) string {
	name := x.text(t)
	if x.tparams.raw {
		return name
	}
	k, s := x.k, goTypeParamSyntaxOf()
	inArgs := false
	for p := t.Parent(); !p.IsNull(); p = p.Parent() {
		switch p.KindId() {
		case s.typeArguments:
			inArgs = true
		case k.parameterList:
			// The receiver's base type is named in the package block, not
			// in the scope of the parameters its type arguments declare.
			if m := p.Parent(); !inArgs && !m.IsNull() && m.KindId() == k.methodDeclaration && goChild(m, k.fReceiver) == p {
				return name
			}
		case k.functionDeclaration, k.typeSpec:
			if v, ok := x.listedParam(goChild(p, s.fTypeParameters), name); ok {
				return v
			}
		case k.methodDeclaration:
			if v, ok := x.listedParam(goChild(p, s.fTypeParameters), name); ok {
				return v
			}
			if v, ok := x.receiverParam(p, name); ok {
				return v
			}
		}
	}
	return name
}

// listedParam is the reduction of the type parameter name that list (a type
// parameter list, or null) declares, and whether it declares one.
func (x *goLink) listedParam(list Node, name string) (string, bool) {
	if list.IsNull() {
		return "", false
	}
	s := goTypeParamSyntaxOf()
	found, out := false, ""
	goNamed(list, func(d Node) {
		if found || d.KindId() != s.typeParameterDeclaration {
			return
		}
		x.eachName(d, func(id Node) {
			if !found && x.text(id) == name {
				found, out = true, x.constraintName(d.ChildByFieldId(x.k.fType))
			}
		})
	})
	return out, found
}

// constraintName is the one type a constraint names, reduced by baseName, or
// empty for a constraint that names no one type.
func (x *goLink) constraintName(c Node) string {
	if c.IsNull() || c.KindId() != goTypeParamSyntaxOf().typeConstraint {
		return ""
	}
	var terms []Node
	goNamed(c, func(t Node) { terms = append(terms, t) })
	if len(terms) != 1 {
		return ""
	}
	x.tparams.raw = true
	name := x.baseName(terms[0])
	x.tparams.raw = false
	return name
}

// receiverParam is the reduction of the type parameter name that method m's
// receiver declares, and whether it declares one: the parameter in the same
// position of the receiver type's declaration gives the constraint.
func (x *goLink) receiverParam(m Node, name string) (string, bool) {
	k, s := x.k, goTypeParamSyntaxOf()
	r := goChild(m, k.fReceiver)
	if r.IsNull() {
		return "", false
	}
	d := firstNamed(r)
	if d.IsNull() || d.KindId() != k.parameterDeclaration {
		return "", false
	}
	t := x.unparen(d.ChildByFieldId(k.fType))
	if !t.IsNull() && t.KindId() == k.pointerType {
		t = x.unparen(firstNamed(t))
	}
	if t.IsNull() || t.KindId() != k.genericType {
		return "", false
	}
	args := goChild(t, s.fTypeArguments)
	if args.IsNull() {
		return "", false
	}
	pos := -1
	i := 0
	goNamed(args, func(e Node) {
		if e.KindId() != s.typeElem {
			return
		}
		if id := firstNamed(e); pos < 0 && !id.IsNull() && x.text(id) == name && name != "_" {
			pos = i
		}
		i++
	})
	if pos < 0 {
		return "", false
	}
	x.tparams.raw = true
	base := x.baseName(goChild(t, k.fType))
	x.tparams.raw = false
	spec, ok := x.genericSpec(m, base)
	if !ok {
		return "", true
	}
	out, j := "", 0
	goNamed(goChild(spec, s.fTypeParameters), func(pd Node) {
		if pd.KindId() != s.typeParameterDeclaration {
			return
		}
		x.eachName(pd, func(Node) {
			if j == pos {
				out = x.constraintName(pd.ChildByFieldId(k.fType))
			}
			j++
		})
	})
	return out, true
}

// genericSpec is the file's generic type declaration of name, from any node
// n of the file.
func (x *goLink) genericSpec(n Node, name string) (Node, bool) {
	if x.tparams.specs == nil {
		k, s := x.k, goTypeParamSyntaxOf()
		x.tparams.specs = map[string]Node{}
		root := n
		for p := root.Parent(); !p.IsNull(); p = p.Parent() {
			root = p
		}
		goNamed(root, func(d Node) {
			if d.KindId() != s.typeDeclaration {
				return
			}
			goNamed(d, func(spec Node) {
				if spec.KindId() != k.typeSpec || goChild(spec, s.fTypeParameters).IsNull() {
					return
				}
				if id := spec.ChildByFieldId(k.fName); !id.IsNull() {
					x.tparams.specs[x.copied(id)] = spec
				}
			})
		})
	}
	spec, ok := x.tparams.specs[name]
	return spec, ok
}
