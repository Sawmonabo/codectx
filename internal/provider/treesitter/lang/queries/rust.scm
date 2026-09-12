; Rust structural query pack.

(use_declaration argument: (_) @import.path) @import

(function_item name: (identifier) @name body: (_) @body) @def.function
(function_signature_item name: (identifier) @name) @def.function
(struct_item name: (type_identifier) @name body: (_)? @body) @def.struct
(union_item name: (type_identifier) @name body: (_) @body) @def.struct
(enum_item name: (type_identifier) @name body: (_) @body) @def.enum
(type_item name: (type_identifier) @name) @def.class
(trait_item name: (type_identifier) @name body: (_) @body) @def.interface
(mod_item name: (identifier) @name body: (_)? @body) @def.module
(const_item name: (identifier) @name) @def.constant
(static_item name: (identifier) @name) @def.variable
(macro_definition name: (identifier) @name) @def.function
(field_declaration name: (field_identifier) @name) @def.field
(enum_variant name: (identifier) @name) @def.constant

(impl_item type: (_) @scope.name) @scope

(call_expression function: (identifier) @call.name) @call
(call_expression function: (field_expression value: (_) @call.qualifier field: (field_identifier) @call.name)) @call
(call_expression function: (scoped_identifier path: (_) @call.qualifier name: (identifier) @call.name)) @call
(call_expression function: (generic_function function: (identifier) @call.name)) @call
(macro_invocation macro: (identifier) @call.name) @call

(type_identifier) @ref.type
