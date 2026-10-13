package link

import (
	"context"
	"path"
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
// A name it cannot decide makes the whole spelling goIdentityUnknown and
// never a guess: a qualifier no import (or more than one) of the file binds,
// a dot-imported file's name no package it reads declares, an alias chain
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
}

// goTypeAnswer is what a package declares under one type name: nothing
// (found false), a defined type, or an alias of the type whose form is
// form, written in the file names.
type goTypeAnswer struct {
	found, alias bool
	form         string
	names        goNames
}

// goPackageTypes answers which type a package declares under a name.
type goPackageTypes interface {
	// lookup is what the package at import path p declares under name;
	// known is false when the source of p is not read, so whether it
	// declares one is not known.
	lookup(ctx context.Context, p, name string) (answer goTypeAnswer, known bool, err error)
}

// goIdentity is the canonical spelling of form, written in the file n,
// with every package's declarations read through types.
func goIdentity(ctx context.Context, types goPackageTypes, n goNames, form string) (string, error) {
	var b strings.Builder
	ok, err := goCanonical(ctx, types, n, form, 0, &b)
	if err != nil || !ok {
		return goIdentityUnknown, err
	}
	return b.String(), nil
}

func goCanonical(ctx context.Context, types goPackageTypes, n goNames, form string, depth int, b *strings.Builder) (bool, error) {
	if form == "" || form == goIdentityUnknown || depth > goAliasDepth {
		return false, nil
	}
	for tok := range strings.SplitSeq(form, " ") {
		var name, pkg string
		switch {
		case tok == goIdentityUnknown:
			return false, nil
		case strings.HasPrefix(tok, "."):
			name = tok[1:]
			p, ok, err := goUnqualified(ctx, types, n, name)
			if err != nil || !ok {
				return false, err
			}
			pkg = p
		case strings.HasPrefix(tok, ":"):
			q, nm, ok := strings.Cut(tok[1:], ".")
			if !ok {
				return false, nil
			}
			p, ok, err := n.imported(q)
			if err != nil || !ok {
				return false, err
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
			return false, err
		}
		if known && a.alias {
			if ok, err := goCanonical(ctx, types, a.names, a.form, depth+1, b); err != nil || !ok {
				return false, err
			}
			continue
		}
		goWrite(b, pkg+"."+name)
	}
	return true, nil
}

// goUnqualified is the package of the type an unqualified name denotes in
// the file n, "" for a predeclared type; ok is false when it cannot be
// decided.
func goUnqualified(ctx context.Context, types goPackageTypes, n goNames, name string) (pkg string, ok bool, err error) {
	own := n.pkg()
	a, known, err := types.lookup(ctx, own, name)
	if err != nil {
		return "", false, err
	}
	if known && a.found {
		return own, true, nil
	}
	dots, err := n.dots()
	if err != nil {
		return "", false, err
	}
	// A dot import declares its package's exported names in the file block,
	// inside the universe and outside the package block ("Declarations and
	// scope"): it shadows a predeclared name, and the package's own name
	// (decided above) is never one it also declares, which would not
	// compile.
	from := ""
	for _, d := range dots {
		da, dk, err := types.lookup(ctx, d, name)
		if err != nil {
			return "", false, err
		}
		switch {
		case !dk:
			return "", false, nil
		case da.found && from != "":
			return "", false, nil
		case da.found:
			from = d
		}
	}
	if from != "" {
		return from, true, nil
	}
	if _, universe := goUniverse[name]; universe {
		return "", true, nil
	}
	if len(dots) > 0 {
		return "", false, nil
	}
	return own, true, nil
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
