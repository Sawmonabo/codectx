; TypeScript additions, appended to ecmascript.scm for both the typescript and
; tsx grammars.

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
