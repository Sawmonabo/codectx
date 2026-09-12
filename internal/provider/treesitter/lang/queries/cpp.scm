; C++ additions, appended to c.scm.

(namespace_definition name: (_) @name body: (_) @body) @def.namespace
(class_specifier name: (_) @name body: (_) @body) @def.class
(declaration_list (declaration declarator: (_) @declarator) @def.variable)
(field_declaration_list (declaration declarator: (_) @declarator) @def.variable)
(alias_declaration name: (type_identifier) @name) @def.class
(using_declaration (_) @import.path) @import

(call_expression function: (qualified_identifier scope: (_) @call.qualifier name: (identifier) @call.name)) @call
(call_expression function: (template_function name: (identifier) @call.name)) @call
