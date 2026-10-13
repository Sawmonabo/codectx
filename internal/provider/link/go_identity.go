package link

import (
	"context"
	"go/token"
	"math/big"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Go type identity (The Go Programming Language Specification, "Type
// identity"). Two method signatures are the same when their types are
// identical, and a named type is identical only to itself: the same type
// name declared in the same package. The worker spells each method's
// signature as an identity form (wire.Export.Identity) whose names are
// marked by how they are written; goIdentity rewrites a form into its
// canonical spelling, every named type written as its package's import path
// and its name, so that two signatures are identical exactly when their
// canonical spellings are equal:
//
//   - `.Name`, written unqualified, is the type the file's package declares
//     under that name; else, in a file with dot imports, the one a dot
//     imported package declares; else a predeclared type ("Predeclared
//     identifiers"), `byte` read as `uint8`, `rune` as `int32` and `any` as
//     `interface{}` (each an alias of the other, "Numeric types",
//     "Interface types"); else a type the package declares in a file the
//     unit does not hold;
//   - `:pkg.Name` is the type of that name in the package the file's import
//     of pkg names;
//   - a name that denotes an alias (`type A = T`) is T's canonical spelling,
//     read in the alias's own file ("Alias declarations");
//   - `$i`, a type parameter by position, and the literal tokens stay as
//     they are.
//
// A qualifier no import of the file binds by its name is one of the file's
// imports written without a name whose package clause is not read (their
// table names are only the convention, goPackageName): the file compiles
// only if exactly one of them declares the qualifier as its clause, since
// two imports of one name in a file block are a redeclaration. With one such
// import the qualifier is that import's package; with several the name is
// spelled unresolved, `?p1|p2:x.Name`, the candidates' paths sorted. A name
// written unqualified in a file with dot imports whose packages are not read
// is spelled `?.own|d1|d2.Name` the same way. Either spelling denotes one
// type in every file it is written in (the clauses and declarations of the
// packages it lists are the same everywhere in a build), so two methods
// compare equal on it only when their types are identical; a method whose
// identity holds one is counted (goUnresolvedIdentity).
//
// A form that denotes no type a compiling program can write makes the whole
// spelling goIdentityUnknown: a qualifier no import can bind, an alias chain
// deeper than goAliasDepth, a form the worker could not spell. The same
// rules read the standard library's and dependencies' declarations
// (goExternal), so a unit's method and an external interface's compare by
// one identity.
const goIdentityUnknown = "?"

// goAliasDepth bounds an alias chain: the specification forbids a cycle, so
// a longer chain is ill-formed source.
const goAliasDepth = 16

// goUniverse is the canonical spelling of each predeclared type name.
var goUniverse = map[string]string{
	"bool": "bool", "byte": "uint8", "comparable": "comparable", "complex64": "complex64",
	"complex128": "complex128", "error": "error", "float32": "float32", "float64": "float64", "int": "int",
	"int8": "int8", "int16": "int16", "int32": "int32", "int64": "int64", "rune": "int32", "string": "string",
	"uint": "uint", "uint8": "uint8", "uint16": "uint16", "uint32": "uint32", "uint64": "uint64",
	"uintptr": "uintptr", "any": "interface{}",
}

// goNames is the file a form is written in, as goIdentity reads it.
type goNames interface {
	// pkg is the import path of the file's package.
	pkg() string
	// imported is the import path the file's import of the package name
	// binds; ok is false when no import, or more than one, binds it.
	imported(name string) (string, bool, error)
	// dots are the import paths the file's dot imports name.
	dots() ([]string, error)
	// unresolved are the import paths the file imports without a name
	// whose package clause is not read, sorted, each once.
	unresolved() ([]string, error)
}

// goTypeAnswer is what a package declares under one type name: nothing
// (found false), a defined type, or an alias of the type whose form is
// form, written in the file names under the build condition cond; others
// are the alias's further declarations whose forms are written differently
// (one per platform or build tag), which denote the same type in every
// build exactly when every one spells it canonically the same.
type goTypeAnswer struct {
	found, alias bool
	form         string
	names        goNames
	cond         goCond
	others       []goDecl
}

// goDecl is one declaration's form, written in the file names, whose build
// condition is cond (0 outside a hierarchy build).
type goDecl struct {
	form  string
	names goNames
	cond  goCond
}

// goPackageTypes answers which type a package declares under a name.
type goPackageTypes interface {
	// lookup is what the package at import path p declares under name;
	// known is false when the source of p is not read, so whether it
	// declares one is not known.
	lookup(ctx context.Context, p, name string) (answer goTypeAnswer, known bool, err error)
	// constant is each distinctly written form of the integer value of the
	// constant the package at import path p declares under name
	// (wire.Export.Identity past its "#", one per platform or build tag that
	// writes it differently), with the file it is written in; none when the
	// package declares none or its source is not read.
	constant(ctx context.Context, p, name string) (decls []goDecl, err error)
}

// goIdentity is the canonical spelling of form, written in the file n,
// with every package's declarations read through types.
func goIdentity(ctx context.Context, types goPackageTypes, n goNames, form string) (string, error) {
	var b strings.Builder
	ok, err := goCanonical(ctx, types, n, form, nil, 0, &b)
	if err != nil || !ok {
		return goIdentityUnknown, err
	}
	return b.String(), nil
}

// goCanonical writes the canonical spelling of form, written in the file n;
// subst, for a generic alias's form, holds the canonical spelling of each of
// its type arguments, which its `$i` stand for.
func goCanonical(ctx context.Context, types goPackageTypes, n goNames, form string, subst []string, depth int, b *strings.Builder) (bool, error) {
	if form == "" || form == goIdentityUnknown || depth > goAliasDepth {
		return false, nil
	}
	toks := strings.Split(form, " ")
	rest, ok, err := goCanonicalTokens(ctx, types, n, toks, subst, depth, b)
	if err != nil || !ok {
		return false, err
	}
	return len(rest) == 0, nil
}

// goCanonicalTokens writes the canonical spelling of toks up to the `}`
// that closes the literal they are written in, or to their end, and returns
// the tokens past it. An interface literal's elements are written sorted
// (goInterfaceElements); a field or method name of a literal (`@name`) that
// is not exported is qualified by the file's package, since such a name
// from another package is always different ("Type identity").
func goCanonicalTokens(ctx context.Context, types goPackageTypes, n goNames, toks []string, subst []string, depth int, b *strings.Builder) ([]string, bool, error) {
	for len(toks) > 0 {
		tok := toks[0]
		toks = toks[1:]
		var name, pkg string
		switch {
		case tok == goIdentityUnknown:
			return nil, false, nil
		case tok == "}":
			goWrite(b, tok)
			return toks, true, nil
		case tok == "struct{":
			goWrite(b, tok)
			rest, ok, err := goCanonicalTokens(ctx, types, n, toks, subst, depth, b)
			if err != nil || !ok {
				return nil, false, err
			}
			toks = rest
			continue
		case tok == "[#":
			end := slices.Index(toks, "]")
			if end < 0 {
				return nil, false, nil
			}
			if ok, err := goArrayLength(ctx, types, n, toks[:end], depth, b); err != nil || !ok {
				return nil, false, err
			}
			toks = toks[end+1:]
			continue
		case tok == "interface{":
			rest, ok, err := goInterfaceElements(ctx, types, n, toks, subst, depth, b)
			if err != nil || !ok {
				return nil, false, err
			}
			toks = rest
			continue
		case subst != nil && strings.HasPrefix(tok, "$"):
			i, err := strconv.Atoi(tok[1:])
			if err != nil || i < 0 || i >= len(subst) {
				return nil, false, nil
			}
			goWrite(b, subst[i])
			continue
		case strings.HasPrefix(tok, "@"):
			if !token.IsExported(tok[1:]) {
				tok = "@" + n.pkg() + "." + tok[1:]
			}
			goWrite(b, tok)
			continue
		case strings.HasPrefix(tok, "."):
			name = tok[1:]
			p, ok, err := goUnqualified(ctx, types, n, name)
			if err != nil || !ok {
				return nil, false, err
			}
			if strings.HasPrefix(p, "?") {
				goWrite(b, p)
				continue
			}
			pkg = p
		case strings.HasPrefix(tok, ":"):
			q, nm, ok := strings.Cut(tok[1:], ".")
			if !ok {
				return nil, false, nil
			}
			p, ok, err := n.imported(q)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				spelled, ok, err := goUnboundQualifier(n, q, nm)
				if err != nil || !ok {
					return nil, false, err
				}
				if strings.HasPrefix(spelled, "?") {
					goWrite(b, spelled)
					continue
				}
				p = spelled
			}
			pkg, name = p, nm
		default:
			goWrite(b, tok)
			continue
		}
		if pkg == "" {
			goWrite(b, goUniverse[name])
			continue
		}
		a, known, err := types.lookup(ctx, pkg, name)
		if err != nil {
			return nil, false, err
		}
		if known && a.alias {
			var args []string
			arity, _, generic := goGenericAlias(a.form)
			if generic {
				// A generic alias is used instantiated ("Alias
				// declarations"): its form with each `$i` its i-th type
				// argument, each spelled where the use is written.
				var ok bool
				var err error
				args, toks, ok, err = goTypeArguments(ctx, types, n, toks, subst, depth, arity)
				if err != nil || !ok {
					return nil, false, err
				}
			}
			spelled, ok, err := goAliasSpelling(ctx, types, a, args, arity, depth)
			if err != nil || !ok {
				return nil, false, err
			}
			goWrite(b, spelled)
			continue
		}
		goWrite(b, pkg+"."+name)
	}
	return nil, true, nil
}

// goArrayLength writes an array length written as the constant expression
// expr (the worker's goLink.constForm) in the file n: `[ v ]` with v its
// value when every constant it names folds (goFold), else `[#` the
// expression with each constant named by its package and name `]`, which
// denotes one length wherever it is written ("Array types", "Constant
// expressions").
func goArrayLength(ctx context.Context, types goPackageTypes, n goNames, expr []string, depth int, b *strings.Builder) (bool, error) {
	v, err := goFold(ctx, types, n, expr, depth)
	if err != nil {
		return false, err
	}
	if v != nil {
		goWrite(b, "[")
		goWrite(b, v.String())
		goWrite(b, "]")
		return true, nil
	}
	spelled := []string{"[#"}
	for _, tok := range expr {
		if !strings.HasPrefix(tok, ".") && !strings.HasPrefix(tok, ":") {
			spelled = append(spelled, tok)
			continue
		}
		name, p, ok, err := goConstName(ctx, types, n, tok)
		if err != nil || !ok {
			return false, err
		}
		if strings.HasPrefix(p, "?") {
			spelled = append(spelled, p)
		} else {
			spelled = append(spelled, p+"."+name)
		}
	}
	for _, tok := range append(spelled, "]") {
		goWrite(b, tok)
	}
	return true, nil
}

// goConstName is the package a constant's name token tok names (`.N` or
// `:pkg.N`) and its name, the package "" for the universe's or an
// unresolved spelling (goUnqualified, goUnboundQualifier); ok is false for
// a token that names no constant.
func goConstName(ctx context.Context, types goPackageTypes, n goNames, tok string) (name, pkg string, ok bool, err error) {
	switch {
	case strings.HasPrefix(tok, "."):
		// The scopes an unqualified constant is read in, as goUnqualified
		// reads a type's: the package, then the dot imports, which declare
		// exported names only.
		name = tok[1:]
		own := n.pkg()
		if decls, err := types.constant(ctx, own, name); err != nil || len(decls) > 0 || !token.IsExported(name) {
			return name, own, err == nil, err
		}
		dots, err := n.dots()
		if err != nil {
			return "", "", false, err
		}
		var unread []string
		for _, d := range dots {
			decls, err := types.constant(ctx, d, name)
			if err != nil {
				return "", "", false, err
			}
			if len(decls) > 0 {
				return name, d, true, nil
			}
			if _, known, err := types.lookup(ctx, d, name); err != nil {
				return "", "", false, err
			} else if !known {
				unread = append(unread, d)
			}
		}
		if len(unread) > 0 {
			slices.Sort(unread)
			return name, "?." + own + "|" + strings.Join(slices.Compact(unread), "|") + "." + name, true, nil
		}
		return name, own, true, nil
	case strings.HasPrefix(tok, ":"):
		q, nm, found := strings.Cut(tok[1:], ".")
		if !found {
			return "", "", false, nil
		}
		p, ok, err := n.imported(q)
		if err != nil {
			return "", "", false, err
		}
		if !ok {
			spelled, ok, err := goUnboundQualifier(n, q, nm)
			if err != nil || !ok {
				return "", "", false, err
			}
			if strings.HasPrefix(spelled, "?") {
				return nm, spelled, true, nil
			}
			p = spelled
		}
		return nm, p, true, nil
	}
	return "", "", false, nil
}

// goFold is the value of the integer constant expression expr written in
// the file n, nil when a constant it names is not known or its value is
// not an integer the form spells. Constants are exact ("Constant
// expressions": arbitrary precision), so the value is a big.Int; division
// truncates and shifts take a non-negative count, as for integer constants.
func goFold(ctx context.Context, types goPackageTypes, n goNames, expr []string, depth int) (*big.Int, error) {
	if depth > goAliasDepth {
		return nil, nil
	}
	pos := 0
	var parse func() (*big.Int, error)
	parse = func() (*big.Int, error) {
		if pos >= len(expr) {
			return nil, nil
		}
		tok := expr[pos]
		pos++
		switch {
		case tok == "(":
			l, err := parse()
			if err != nil || l == nil || pos >= len(expr) {
				return nil, err
			}
			if expr[pos] == ")" {
				pos++
				return l, nil
			}
			op := expr[pos]
			pos++
			r, err := parse()
			if err != nil || r == nil || pos >= len(expr) || expr[pos] != ")" {
				return nil, err
			}
			pos++
			return goBinary(op, l, r), nil
		case len(tok) == 2 && tok[0] == 'u':
			v, err := parse()
			if err != nil || v == nil {
				return nil, err
			}
			switch tok[1] {
			case '+':
				return v, nil
			case '-':
				return new(big.Int).Neg(v), nil
			case '^':
				return new(big.Int).Not(v), nil
			}
			return nil, nil
		case strings.HasPrefix(tok, ".") || strings.HasPrefix(tok, ":"):
			name, p, ok, err := goConstName(ctx, types, n, tok)
			if err != nil || !ok || p == "" || strings.HasPrefix(p, "?") {
				return nil, err
			}
			decls, err := types.constant(ctx, p, name)
			if err != nil || len(decls) == 0 {
				return nil, err
			}
			// A constant declared once per platform or build tag has a
			// value exactly when every declaration folds to the same one.
			var v *big.Int
			for _, d := range decls {
				if d.form == goIdentityUnknown {
					return nil, nil
				}
				dv, err := goFold(ctx, types, d.names, strings.Split(d.form, " "), depth+1)
				if err != nil || dv == nil || v != nil && dv.Cmp(v) != 0 {
					return nil, err
				}
				v = dv
			}
			return v, nil
		}
		v, ok := new(big.Int).SetString(tok, 10)
		if !ok {
			return nil, nil
		}
		return v, nil
	}
	v, err := parse()
	if err != nil || pos != len(expr) {
		return nil, err
	}
	return v, nil
}

// goBinary is l op r for integer constants, nil when it is undefined.
func goBinary(op string, l, r *big.Int) *big.Int {
	z := new(big.Int)
	switch op {
	case "+":
		return z.Add(l, r)
	case "-":
		return z.Sub(l, r)
	case "*":
		return z.Mul(l, r)
	case "/", "%":
		if r.Sign() == 0 {
			return nil
		}
		if op == "/" {
			return z.Quo(l, r)
		}
		return z.Rem(l, r)
	case "<<", ">>":
		if r.Sign() < 0 || !r.IsUint64() || r.Uint64() > 1<<16 {
			return nil
		}
		if op == "<<" {
			return z.Lsh(l, uint(r.Uint64()))
		}
		return z.Rsh(l, uint(r.Uint64()))
	case "&":
		return z.And(l, r)
	case "|":
		return z.Or(l, r)
	case "^":
		return z.Xor(l, r)
	case "&^":
		return z.AndNot(l, r)
	}
	return nil
}

// goGenericAlias reads a generic alias's form, `$$n` (its number of type
// parameters) and the form of the type it denotes with its parameters as
// `$i`: the arity, that form, and true; false for any other form.
func goGenericAlias(form string) (int, string, bool) {
	head, rest, _ := strings.Cut(form, " ")
	if !strings.HasPrefix(head, "$$") {
		return 0, form, false
	}
	n, err := strconv.Atoi(head[2:])
	if err != nil || n <= 0 {
		return 0, form, false
	}
	return n, rest, true
}

// goAliasSpelling is the canonical spelling of the type the alias a
// denotes, instantiated with args for a generic alias of arity parameters:
// the one spelling every declaration of a gives, and ok false when two of
// them spell different types, since which one a use means depends on the
// build.
func goAliasSpelling(ctx context.Context, types goPackageTypes, a goTypeAnswer, args []string, arity, depth int) (string, bool, error) {
	spelled := ""
	for i, d := range append([]goDecl{{form: a.form, names: a.names, cond: a.cond}}, a.others...) {
		n, form, generic := goGenericAlias(d.form)
		if generic != (arity > 0) || n != arity {
			return "", false, nil
		}
		var b strings.Builder
		if ok, err := goCanonical(ctx, types, d.names, form, args, depth+1, &b); err != nil || !ok {
			return "", false, err
		}
		if i > 0 && b.String() != spelled {
			return "", false, nil
		}
		spelled = b.String()
	}
	return spelled, true, nil
}

// goTypeArguments reads, from toks, the type argument list `[ a , b ]` of
// an instantiation of arity arguments: each argument's canonical spelling,
// written in the file n, and the tokens past the `]`; ok is false for a
// list of another length or a use without one.
func goTypeArguments(ctx context.Context, types goPackageTypes, n goNames, toks []string, subst []string, depth, arity int) ([]string, []string, bool, error) {
	if len(toks) == 0 || toks[0] != "[" {
		return nil, nil, false, nil
	}
	var args []string
	start, nested := 1, 0
	for i := 1; i < len(toks); i++ {
		switch t := toks[i]; {
		case t == "[" || t == "(" || t == "struct{" || t == "interface{" || t == "[#":
			nested++
		case t == ")" || t == "}" || t == "]" && nested > 0:
			nested--
		case nested == 0 && (t == "," || t == "]"):
			var e strings.Builder
			rest, ok, err := goCanonicalTokens(ctx, types, n, toks[start:i], subst, depth, &e)
			if err != nil || !ok || len(rest) != 0 || e.Len() == 0 {
				return nil, nil, false, err
			}
			args = append(args, e.String())
			start = i + 1
			if t == "]" {
				if len(args) != arity {
					return nil, nil, false, nil
				}
				return args, toks[i+1:], true, nil
			}
		}
	}
	return nil, nil, false, nil
}

// goInterfaceElements writes an interface literal whose `interface{` has
// been read: its elements, each canonical, sorted and separated by `;`,
// then `}`; it returns the tokens past the `}`. An interface's identity is
// its method set ("Type identity"), whatever order its elements are
// written in.
func goInterfaceElements(ctx context.Context, types goPackageTypes, n goNames, toks []string, subst []string, depth int, b *strings.Builder) ([]string, bool, error) {
	var elems []string
	for {
		var e strings.Builder
		// One element: up to a `;` or the closing `}` at its own depth.
		end, nested := 0, 0
		for ; end < len(toks); end++ {
			t := toks[end]
			if nested == 0 && (t == ";" || t == "}") {
				break
			}
			switch t {
			case "struct{", "interface{":
				nested++
			case "}":
				nested--
			}
		}
		if end == len(toks) {
			return nil, false, nil
		}
		rest, ok, err := goCanonicalTokens(ctx, types, n, toks[:end], subst, depth, &e)
		if err != nil || !ok || len(rest) != 0 {
			return nil, false, err
		}
		elems = append(elems, e.String())
		sep := toks[end]
		toks = toks[end+1:]
		if sep == "}" {
			break
		}
	}
	slices.Sort(elems)
	goWrite(b, "interface{")
	for i, e := range elems {
		if i > 0 {
			goWrite(b, ";")
		}
		goWrite(b, e)
	}
	goWrite(b, "}")
	return toks, true, nil
}

// goUnqualified is the package of the type an unqualified name denotes in
// the file n, "" for a predeclared type, or the unresolved spelling of the
// name (`?.own|d1|d2.Name`) when only dot imports whose packages are not
// read can tell; ok is false when it cannot be decided.
func goUnqualified(ctx context.Context, types goPackageTypes, n goNames, name string) (pkg string, ok bool, err error) {
	own := n.pkg()
	a, known, err := types.lookup(ctx, own, name)
	if err != nil {
		return "", false, err
	}
	if known && a.found {
		return own, true, nil
	}
	_, universe := goUniverse[name]
	// A dot import declares only its package's exported names ("Import
	// declarations"), so a name that is not exported is the universe's or
	// the package's own.
	if !token.IsExported(name) {
		if universe {
			return "", true, nil
		}
		return own, true, nil
	}
	dots, err := n.dots()
	if err != nil {
		return "", false, err
	}
	// A dot import declares its package's exported names in the file block,
	// inside the universe and outside the package block ("Declarations and
	// scope"): it shadows a predeclared name, and the package's own name
	// (decided above) is never one it also declares, nor do two of them
	// declare one name, which would not compile. So a package read that
	// declares the name decides it whatever the unread ones hold.
	from := ""
	var unread []string
	for _, d := range dots {
		da, dk, err := types.lookup(ctx, d, name)
		if err != nil {
			return "", false, err
		}
		switch {
		case !dk:
			unread = append(unread, d)
		case da.found && from != "":
			return "", false, nil
		case da.found:
			from = d
		}
	}
	switch {
	case from != "":
		return from, true, nil
	case len(unread) > 0:
		slices.Sort(unread)
		return "?." + own + "|" + strings.Join(slices.Compact(unread), "|") + "." + name, true, nil
	case universe:
		return "", true, nil
	}
	return own, true, nil
}

// goUnboundQualifier is what the qualifier q of the type q.name denotes in
// the file n when no import binds q by its name: the one import of the file
// written without a name whose clause is not read (the only import that can
// declare q), or, with several, the unresolved spelling `?p1|p2:q.name`;
// ok is false when the file has none, and q denotes nothing.
func goUnboundQualifier(n goNames, q, name string) (string, bool, error) {
	cands, err := n.unresolved()
	if err != nil || len(cands) == 0 {
		return "", false, err
	}
	if len(cands) == 1 {
		return cands[0], true, nil
	}
	return "?" + strings.Join(cands, "|") + ":" + q + "." + name, true, nil
}

// goUnresolvedIdentity reports a canonical spelling that names a type by
// its unresolved spelling (goUnboundQualifier, goUnqualified).
func goUnresolvedIdentity(identity string) bool {
	return strings.HasPrefix(identity, "?") && identity != goIdentityUnknown || strings.Contains(identity, " ?") ||
		strings.Contains(identity, "[#")
}

func goWrite(b *strings.Builder, tok string) {
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	b.WriteString(tok)
}

// goIdentityKey is the method-set key (satisfy) of a method named name of
// canonical signature identity, declared at ref: a signature goIdentity
// could not spell gets a key of its own declaration, which no other
// method's key equals.
func goIdentityKey(name, identity string, ref Ref) string {
	if identity == goIdentityUnknown || identity == "" {
		return name + "\x00" + goIdentityUnknown + refKey(ref)
	}
	return name + "\x00" + identity
}

// goImportPath is the import path of the package declared in the
// repository-relative directory dir under the package name pkg, of a unit
// whose module is module at root: a vendored package's path below vendor/
// (the go command's vendor resolution), a module package's the module path
// and dir below root ("Packages, modules, and versions"), an external test
// package's (`package p_test`) that path and "_test". A directory of no
// module has no import path; its packages are named by their directory,
// which no import path can spell.
func goImportPath(module, root, dir, pkg string) string {
	var p string
	switch rel, inRoot := relDir(root, dir); {
	case !inRoot:
		p = "./" + dir
	case rel == "vendor" || strings.HasPrefix(rel, "vendor/"):
		p = strings.TrimPrefix(strings.TrimPrefix(rel, "vendor"), "/")
	case module == "":
		p = "./" + dir
	case rel == ".":
		p = module
	default:
		p = module + "/" + rel
	}
	if strings.HasSuffix(pkg, "_test") {
		p += "_test"
	}
	return p
}

// relDir is dir relative to root, both repository-relative and cleaned, and
// whether dir is root or lies under it.
func relDir(root, dir string) (string, bool) {
	root, dir = path.Clean(root), path.Clean(dir)
	if root == "." {
		return dir, true
	}
	if dir == root {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(dir, root+"/"); ok {
		return rest, true
	}
	return "", false
}
