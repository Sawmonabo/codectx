package model

// ContextRoute is one evidence route of one manifest entry: the relations a
// walk crossed from a root to the entry, in walk order. A manifest stores every
// route of every entry in full; the routes view pages them in (Entry, Index)
// order, and an entry names how many it has (ContextEntry.RouteCount), so an
// entry's routes are read by reference rather than carried with it.
type ContextRoute struct {
	// Entry is the ordinal of the entry the route explains.
	Entry int `json:"entry"`
	// Index is the route's position among the entry's routes, from zero.
	Index     int          `json:"index"`
	Relations []RelationID `json:"relations"`
}

// Validate enforces the context_entry_routes constraints: a route names at
// least one relation and at most MaxRelationsPerPath, each a canonical id.
func (r ContextRoute) Validate() error {
	if err := requireNonNegative("context_route.entry", int64(r.Entry)); err != nil {
		return err
	}
	if err := requireNonNegative("context_route.index", int64(r.Index)); err != nil {
		return err
	}
	if len(r.Relations) == 0 {
		return invalid("context_route %d of entry %d names no relation", r.Index, r.Entry)
	}
	if err := boundCount("context_route.relations", len(r.Relations), MaxRelationsPerPath); err != nil {
		return err
	}
	for i, id := range r.Relations {
		if err := requireID(indexed("context_route.relations", i), string(id)); err != nil {
			return err
		}
	}
	return nil
}
