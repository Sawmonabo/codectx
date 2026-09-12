; C structural query pack. The C++ pack is this file followed by cpp.scm.
; Names of function, variable, field and typedef declarations are found by
; the worker's declarator hook, because the identifier sits below a chain of
; pointer/array/function declarators.

(preproc_include path: (_) @import.path) @import

(function_definition declarator: (_) @declarator body: (_) @body) @def.function
(translation_unit (declaration declarator: (_) @declarator) @def.variable)
(struct_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(union_specifier name: (type_identifier) @name body: (_) @body) @def.struct
(enum_specifier name: (type_identifier) @name body: (_) @body) @def.enum
(enumerator name: (identifier) @name) @def.constant
(field_declaration declarator: (_) @declarator) @def.field
(type_definition declarator: (_) @declarator) @def.class
(preproc_def name: (identifier) @name) @def.constant
(preproc_function_def name: (identifier) @name) @def.function

(call_expression function: (identifier) @call.name) @call
(call_expression function: (field_expression argument: (_) @call.qualifier field: (field_identifier) @call.name)) @call

(type_identifier) @ref.type
