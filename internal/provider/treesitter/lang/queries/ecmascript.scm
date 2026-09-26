; ECMAScript structural query pack shared by the javascript, typescript and
; tsx grammars; javascript.scm and typescript.scm add grammar-specific nodes.

; One match per import statement: the clause is captured whole and the worker
; walks its bindings in source order. A pattern that captured each specifier
; and then the source after it would keep one partial match per specifier open
; until the source, which is quadratic in the specifier count.
(import_statement (import_clause)? @import.clause source: (string) @import.path) @import
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
