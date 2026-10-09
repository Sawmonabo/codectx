; C++ additions, appended to c.scm.

(namespace_definition name: (_) @name body: (_) @body) @def.namespace
(class_specifier name: (_) @name body: (_) @body) @def.class
(field_declaration_list (declaration declarator: (_) @declarator) @def.variable)
(template_declaration (declaration declarator: (_) @declarator) @def.variable)
; A member function defined without a body: pure virtual, defaulted, deleted.
(function_definition declarator: (_) @declarator [(pure_virtual_clause) (default_method_clause) (delete_method_clause)]) @def.function
(alias_declaration name: (type_identifier) @name) @def.class
(using_declaration (_) @import.path) @import

(call_expression function: (qualified_identifier scope: (_) @call.qualifier name: (identifier) @call.name)) @call
(call_expression function: (template_function name: (identifier) @call.name)) @call
