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

; A module-level `if __name__ == "__main__":` is a program entry point, an
; endpoint declaration named by the `__main__` string, whose block is its
; body, so the calls it makes are made from it. The query admits either
; operand order, and a parenthesized condition, whose comparison the worker
; finds inside the parentheses; the worker keeps only a two-operand `==`
; comparison of `__name__` and the string `__main__`.
((module (if_statement condition: (comparison_operator (identifier) @guard.name (string (string_content) @name)) consequence: (block) @body) @def.endpoint)
  (#eq? @guard.name "__name__") (#eq? @name "__main__"))
((module (if_statement condition: (comparison_operator (string (string_content) @name) (identifier) @guard.name) consequence: (block) @body) @def.endpoint)
  (#eq? @guard.name "__name__") (#eq? @name "__main__"))
(module (if_statement condition: (parenthesized_expression) consequence: (block) @body) @def.endpoint)

; Every statement that binds or changes `__all__`, which the export rule
; reads: the static forms are an assignment, `+=`, `.extend`, `.append` and
; `.remove`, each a statement of the module; anything else makes it computed.
((assignment left: (identifier) @all.target) @all (#eq? @all.target "__all__"))
((augmented_assignment left: (identifier) @all.target) @all (#eq? @all.target "__all__"))
((call function: (attribute object: (identifier) @all.target attribute: (identifier) @all.method)) @all (#eq? @all.target "__all__"))

(call function: (identifier) @call.name) @call
(call function: (attribute object: (_) @call.qualifier attribute: (identifier) @call.name)) @call
