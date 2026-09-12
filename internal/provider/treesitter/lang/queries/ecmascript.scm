; ECMAScript structural query pack shared by the javascript, typescript and
; tsx grammars; javascript.scm and typescript.scm add grammar-specific nodes.

(import_statement source: (string) @import.path) @import
(import_statement (import_clause (identifier) @import.name) source: (string) @import.path) @import
(import_statement (import_clause (named_imports (import_specifier !alias name: (identifier) @import.name))) source: (string) @import.path) @import
(import_statement (import_clause (named_imports (import_specifier alias: (identifier) @import.name))) source: (string) @import.path) @import
(import_statement (import_clause (namespace_import (identifier) @import.name)) source: (string) @import.path) @import
((call_expression function: (identifier) @import.fn arguments: (arguments . (string) @import.path)) @import
  (#eq? @import.fn "require"))

(function_declaration name: (identifier) @name body: (_) @body) @def.function
(generator_function_declaration name: (identifier) @name body: (_) @body) @def.function
(class_declaration name: (_) @name body: (_) @body) @def.class
(method_definition name: (property_identifier) @name body: (_) @body) @def.method
(variable_declarator name: (identifier) @name value: [(arrow_function) (function_expression)] @body) @def.function
(lexical_declaration kind: "const" (variable_declarator name: (identifier) @name) @def.constant)
(variable_declarator name: (identifier) @name) @def.variable

(export_statement declaration: (_) @export)
(export_statement (export_clause (export_specifier name: (identifier) @export.name)))

(call_expression function: (identifier) @call.name) @call
(call_expression function: (member_expression object: (_) @call.qualifier property: (property_identifier) @call.name)) @call
(new_expression constructor: (identifier) @call.name) @call

((call_expression function: (identifier) @test.fn arguments: (arguments . (string) @test.name)) @def.test
  (#any-of? @test.fn "it" "test" "describe"))
