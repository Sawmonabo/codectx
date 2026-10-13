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
; An anonymous class body (JLS §15.9.5) is a class named `new T`, T the
; created type's last simple name as the creation's call reference takes
; it; its methods are its members. An enum constant's body is the
; constant's own.
(object_creation_expression type: (type_identifier) @name (class_body) @body) @def.class @anonymous
(object_creation_expression type: (generic_type (type_identifier) @name) (class_body) @body) @def.class @anonymous
(object_creation_expression type: (scoped_type_identifier (type_identifier) @name .) (class_body) @body) @def.class @anonymous
(object_creation_expression type: (generic_type (scoped_type_identifier (type_identifier) @name .)) (class_body) @body) @def.class @anonymous

(method_invocation !object name: (identifier) @call.name) @call
(method_invocation object: (_) @call.qualifier name: (identifier) @call.name) @call
(object_creation_expression type: (type_identifier) @call.name) @call
; `new T<>()`, `new a.b.T()` and `new a.b.T<>()`: the callee token is the
; created type's simple name, the last one written.
(object_creation_expression type: (generic_type (type_identifier) @call.name)) @call
(object_creation_expression type: (scoped_type_identifier (type_identifier) @call.name .)) @call
(object_creation_expression type: (generic_type (scoped_type_identifier (type_identifier) @call.name .))) @call
; An explicit constructor invocation, `this(...)` or `super(...)`: the
; callee token is the keyword.
(explicit_constructor_invocation constructor: [(this) (super)] @call.name) @call

(type_identifier) @ref.type
