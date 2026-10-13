package context

import "github.com/Sawmonabo/codectx/internal/model"

// ScopeRelations returns a copy of the relation kinds a plan's scope walk
// crosses, for measurements that must walk exactly what a plan walks.
func ScopeRelations() []model.RelationKind {
	return append([]model.RelationKind(nil), scopeRelations...)
}
