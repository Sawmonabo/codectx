; Java structural query pack.

(package_declaration [(identifier) (scoped_identifier)] @package)
(import_declaration [(identifier) (scoped_identifier)] @import.path) @import

(class_declaration name: (identifier) @name body: (_) @body) @def.class
(interface_declaration name: (identifier) @name body: (_) @body) @def.interface
(enum_declaration name: (identifier) @name body: (_) @body) @def.enum
(record_declaration name: (identifier) @name body: (_) @body) @def.class
(annotation_type_declaration name: (identifier) @name body: (_) @body) @def.interface
(method_declaration name: (identifier) @name body: (_)? @body) @def.method
(constructor_declaration name: (identifier) @name body: (_) @body) @def.method
(field_declaration declarator: (variable_declarator name: (identifier) @name)) @def.field
(constant_declaration declarator: (variable_declarator name: (identifier) @name)) @def.constant
(enum_constant name: (identifier) @name) @def.constant

(method_invocation !object name: (identifier) @call.name) @call
(method_invocation object: (_) @call.qualifier name: (identifier) @call.name) @call
(object_creation_expression type: (type_identifier) @call.name) @call

(type_identifier) @ref.type
