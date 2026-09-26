; Go structural query pack. Capture vocabulary is documented in
; docs/providers-treesitter.md; kinds are refined by the worker's Go hook
; (type_spec bodies decide struct/interface/class).

(package_clause (package_identifier) @package)

(import_spec name: (_)? @import.name path: (_) @import.path) @import

(function_declaration name: (identifier) @name body: (block)? @body) @def.function
(method_declaration name: (field_identifier) @name body: (block)? @body) @def.method
; Both lists are captured whole and the worker pairs each name with the value
; in its position; capturing each name and then the list after it would keep
; one partial match per name open, quadratic in the name count. Only a
; statement whose values hold a function literal matches, so a plain `x := v`
; costs the worker nothing. The literal is not captured and is the pattern's
; last step, so the matcher does not split a match per literal: a statement
; binding several matches once.
(short_var_declaration left: (expression_list) @bind.names right: (expression_list (func_literal)) @bind.values) @def.function
(type_spec name: (type_identifier) @name type: (_) @body) @def.class
(type_alias name: (type_identifier) @name type: (_) @body) @def.class
(field_declaration name: (field_identifier) @name) @def.field
(method_elem name: (field_identifier) @name) @def.method
(var_spec name: (identifier) @name) @def.variable
(const_spec name: (identifier) @name) @def.constant

(call_expression function: (identifier) @call.name) @call
(call_expression function: (selector_expression operand: (_) @call.qualifier field: (field_identifier) @call.name)) @call

(type_identifier) @ref.type
