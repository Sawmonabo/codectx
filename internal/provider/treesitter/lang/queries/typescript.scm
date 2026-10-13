; TypeScript additions, appended to ecmascript.scm for both the typescript and
; tsx grammars.

; `import x = require("m")` imports m as plainly as a `require` call does; the
; clause is the import, so its form reads as a require.
(import_require_clause source: (string) @import.path) @import

; A callee under a non-null assertion, or in one pair of parentheses under an
; `as`, `satisfies` or non-null assertion, is the name or member it asserts:
; `f!()`, `(f as T)()`, `(o.m satisfies T)()`, `(o.m!)()`.
(call_expression function: (non_null_expression (identifier) @call.name)) @call
(call_expression function: (non_null_expression (member_expression object: (_) @call.qualifier property: [(property_identifier) (private_property_identifier)] @call.name))) @call
(call_expression function: (parenthesized_expression [(as_expression . (identifier) @call.name) (satisfies_expression . (identifier) @call.name) (non_null_expression (identifier) @call.name)])) @call
(call_expression function: (parenthesized_expression [(as_expression . (member_expression object: (_) @call.qualifier property: [(property_identifier) (private_property_identifier)] @call.name)) (satisfies_expression . (member_expression object: (_) @call.qualifier property: [(property_identifier) (private_property_identifier)] @call.name)) (non_null_expression (member_expression object: (_) @call.qualifier property: [(property_identifier) (private_property_identifier)] @call.name))])) @call

(interface_declaration name: (type_identifier) @name body: (_) @body) @def.interface
(type_alias_declaration name: (type_identifier) @name) @def.class
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(abstract_class_declaration name: (type_identifier) @name body: (_) @body) @def.class
(internal_module name: (_) @name body: (_) @body) @def.namespace
(function_signature name: (identifier) @name) @def.function
(method_signature name: (property_identifier) @name) @def.method
(abstract_method_signature name: (property_identifier) @name) @def.method
(property_signature name: (property_identifier) @name) @def.field
(public_field_definition name: (property_identifier) @name) @def.field
(enum_assignment name: (property_identifier) @name) @def.constant

(type_identifier) @ref.type
