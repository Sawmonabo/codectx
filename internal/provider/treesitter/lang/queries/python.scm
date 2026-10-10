; Python structural query pack.

; `import a, b as c` imports each module on its own: each clause is one import.
(import_statement name: (dotted_name) @import.path @import)
(import_statement name: (aliased_import name: (dotted_name) @import.path alias: (identifier) @import.name) @import)
(import_from_statement module_name: (_) @import.path name: (dotted_name) @import.name) @import
(import_from_statement module_name: (_) @import.path name: (aliased_import name: (dotted_name) @import.original alias: (identifier) @import.name)) @import
(import_from_statement module_name: (_) @import.path (wildcard_import)) @import

(function_definition name: (identifier) @name body: (block) @body) @def.function
(class_definition name: (identifier) @name body: (block) @body) @def.class
(module (expression_statement (assignment left: (identifier) @name) @def.variable))
(class_definition body: (block (expression_statement (assignment left: (identifier) @name) @def.field)))

; Every statement that binds or changes `__all__`, which the export rule
; reads: the static forms are an assignment, `+=`, `.extend`, `.append` and
; `.remove`, each a statement of the module; anything else makes it computed.
((assignment left: (identifier) @all.target) @all (#eq? @all.target "__all__"))
((augmented_assignment left: (identifier) @all.target) @all (#eq? @all.target "__all__"))
((call function: (attribute object: (identifier) @all.target attribute: (identifier) @all.method)) @all (#eq? @all.target "__all__"))

(call function: (identifier) @call.name) @call
(call function: (attribute object: (_) @call.qualifier attribute: (identifier) @call.name)) @call
