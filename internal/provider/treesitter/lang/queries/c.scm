; C structural query pack. The C++ pack is this file followed by cpp.scm.
; Names of function, variable, field and typedef declarations are found by
; the worker's declarator hook, because the identifier sits below a chain of
; pointer/array/function declarators.

(preproc_include path: (_) @import.path) @import

(function_definition declarator: (_) @declarator body: (_) @body) @def.function
(translation_unit (declaration declarator: (_) @declarator) @def.variable)
; A file-scope declaration may sit under any number of conditional arms; the
; same patterns match a local inside a function body, which the declarator
; hook drops (only a declaration at file scope is extracted).
(preproc_if (declaration declarator: (_) @declarator) @def.variable)
(preproc_ifdef (declaration declarator: (_) @declarator) @def.variable)
(preproc_else (declaration declarator: (_) @declarator) @def.variable)
(preproc_elif (declaration declarator: (_) @declarator) @def.variable)
(preproc_elifdef (declaration declarator: (_) @declarator) @def.variable)
; A declaration under a linkage specification, braceless (`extern "C" int
; f(void);`) or in its braced body; in C++ the braced body of a namespace is
; the same declaration list.
(linkage_specification body: (declaration declarator: (_) @declarator) @def.variable)
(declaration_list (declaration declarator: (_) @declarator) @def.variable)
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
; A call through a function pointer written `(*fp)(x)`, `(*s->cb)(x)` or
; `(*o.cb)(x)`, or through a table entry `tbl[i](x)`: the callee token is the
; pointer's, the member's or the table's name.
(call_expression function: (parenthesized_expression (pointer_expression operator: "*" argument: (identifier) @call.name))) @call
(call_expression function: (parenthesized_expression (pointer_expression operator: "*" argument: (field_expression argument: (_) @call.qualifier field: (field_identifier) @call.name)))) @call
(call_expression function: (subscript_expression argument: (identifier) @call.name)) @call

(type_identifier) @ref.type
