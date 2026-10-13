// Package gogrammar is the Go grammar the parser worker compiles in: the
// tree-sitter-go grammar at v0.25.0 (MIT, LICENSE in this directory) with two
// rules extended so it accepts the forms Go 1.26 and Go 1.27 add.
//
// A generic method (Go 1.27) takes type parameters after the method name:
//
//	MethodDecl = "func" Receiver MethodName [ TypeParameters ] Signature [ FunctionBody ] .
//
// The release has no type parameters on method_declaration, so
// `func (l List[E]) Apply[F any](f func(E) F) List[F]` parses as an error that
// swallows every declaration after it in the file. method_declaration gains
// field('type_parameters', optional($.type_parameter_list)) after its name,
// and the conflict [type_parameter_declaration, _simple_type] is declared,
// because `func (r T) M[A, B …` cannot tell a type parameter list from a type
// with one token of lookahead.
//
// The operand of new (Go 1.26) is a type or an expression ("Allocation"). The
// release takes only a type, so `new(42)` is an error and `new(n + 1)` an
// error that swallows the statements after it. The first operand of
// special_argument_list, the argument list new and make share, becomes
// choice(prec.dynamic(PREC.primary, $._type), $._expression): an operand that
// reads as a type is still the type the release gives it, so make, whose
// operand is a type, and every `new(T)` parse to the release's trees.
//
// The parser forks only at a method's type parameter list and at the
// operand of new and make, so every file with no generic method and no new of
// an expression parses to the same tree as the release.
//
// parser.c, grammar.json, node-types.json and tree_sitter/ are generated from
// grammar.js and tree-sitter.json and are never edited by hand: running
// `tree-sitter generate -o .` in this directory with tree-sitter CLI 0.25.8,
// the version the release was generated with, rewrites them byte for byte,
// and the same CLI run on the unmodified release reproduces its generated
// src/ exactly. parser.c sits beside this file so cgo compiles it as part of
// the package and the build cache sees every change to it; the headers under
// tree_sitter/ change only with the CLI version, which parser.c's first line
// names, so a regenerated parser.c carries their change into the cache too.
// A change to grammar.js changes the go row's Module pin in the lang registry
// too, so every unit sealed under the old grammar is invalidated.
package gogrammar

// #cgo CFLAGS: -std=c11 -fPIC
// #include "tree_sitter/parser.h"
// const TSLanguage *tree_sitter_go(void);
import "C"

import "unsafe"

// Language returns the grammar's TSLanguage pointer.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_go())
}
