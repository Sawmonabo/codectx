// Package gogrammar is the Go grammar the parser worker compiles in: the
// tree-sitter-go grammar at v0.25.0 (MIT, LICENSE in this directory) with one
// rule added so it accepts the Go 1.27 generic method, whose declaration
// takes type parameters after the method name:
//
//	MethodDecl = "func" Receiver MethodName [ TypeParameters ] Signature [ FunctionBody ] .
//
// The released grammar has no type parameters on method_declaration, so
// `func (l List[E]) Apply[F any](f func(E) F) List[F]` parses as an error that
// swallows every declaration after it in the file. grammar.js differs from the
// release in two lines: method_declaration gains
// field('type_parameters', optional($.type_parameter_list)) after its name,
// and the conflict [type_parameter_declaration, _simple_type] is declared,
// because `func (r T) M[A, B …` cannot tell a type parameter list from a type
// with one token of lookahead. The parser forks only there, so every file
// without a generic method parses to the same tree as the release.
//
// src/ is generated from grammar.js and tree-sitter.json and is never edited
// by hand: running `tree-sitter generate` in this directory with tree-sitter
// CLI 0.25.8, the version the release was generated with, rewrites it byte
// for byte. The same command on the unmodified release files reproduces the
// release's src/ exactly. A change to grammar.js changes the go row's Module
// pin in the lang registry too, so every unit sealed under the old grammar is
// invalidated.
package gogrammar

// #cgo CFLAGS: -std=c11 -fPIC
// #include "src/parser.c"
import "C"

import "unsafe"

// Language returns the grammar's TSLanguage pointer.
func Language() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_go())
}
