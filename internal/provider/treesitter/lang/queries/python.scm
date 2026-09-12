; Python structural query pack.

(import_statement name: (dotted_name) @import.path) @import
(import_statement name: (aliased_import name: (dotted_name) @import.path alias: (identifier) @import.name)) @import
(import_from_statement module_name: (_) @import.path name: (dotted_name) @import.name) @import
(import_from_statement module_name: (_) @import.path name: (aliased_import alias: (identifier) @import.name)) @import
(import_from_statement module_name: (_) @import.path (wildcard_import)) @import

(function_definition name: (identifier) @name body: (block) @body) @def.function
(class_definition name: (identifier) @name body: (block) @body) @def.class
(module (expression_statement (assignment left: (identifier) @name) @def.variable))
(class_definition body: (block (expression_statement (assignment left: (identifier) @name) @def.field)))

(call function: (identifier) @call.name) @call
(call function: (attribute object: (_) @call.qualifier attribute: (identifier) @call.name)) @call
