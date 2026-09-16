package neo4jcsv

import (
	"context"
	"database/sql"
	"math"
	"strings"
)

// Every frontend lowers every write form into an operator call whose first
// argument is the written operand (docs/research/10-round3-empirical.md §3,
// reproduced here for all six frontends). These are the operator names that
// carry a write.
const (
	opAssignPrefix = "<operator>.assignment"
	opPlainAssign  = "<operator>.assignment"
)

// incrementOps are the read-modify-write operators that carry no explicit
// right-hand side.
var incrementOps = map[string]bool{
	"<operator>.preIncrement":  true,
	"<operator>.postIncrement": true,
	"<operator>.preDecrement":  true,
	"<operator>.postDecrement": true,
}

// unwrapOps are the operators whose written operand is their own first
// argument: an index, a dereference or an address-of wrapping the real target.
var unwrapOps = map[string]bool{
	"<operator>.indexAccess":                  true,
	"<operator>.indirectIndexAccess":          true,
	"<operator>.computedMemberAccess":         true,
	"<operator>.indirectComputedMemberAccess": true,
	"<operator>.indirection":                  true,
	"<operator>.addressOf":                    true,
	"<operator>.cast":                         true,
}

// fieldOps are the operators whose written operand is a named field of their
// first argument's type.
var fieldOps = map[string]bool{
	"<operator>.fieldAccess":         true,
	"<operator>.indirectFieldAccess": true,
	"<operator>.memberAccess":        true,
	"<operator>.getElementPtr":       true,
}

// isWriteOperator reports whether an operator call assigns to its first
// argument, and whether that argument is also read (a compound assignment or
// an increment reads the operand it writes).
func isWriteOperator(op string) (write, alsoReads bool) {
	if incrementOps[op] {
		return true, true
	}
	if strings.HasPrefix(op, opAssignPrefix) {
		return true, op != opPlainAssign
	}
	return false, false
}

// argument is one operand of an operator call.
type argument struct {
	id                                                         int64
	label, name, canonical, code, typeFullName, methodFullName string
	index                                                      sql.NullInt64
}

// target is a declaration a written operand resolved to; ok is false when
// the operand's shape did not resolve.
type target struct {
	id int64
	ok bool
}

// deriveReadsWrites projects the reads and writes of Section 11.6 from the
// export's assignment and increment operators.
//
// The written operand is argument 1. Its shape decides the target: an
// identifier that the export bound with a REF edge, or a field whose name
// resolves to a MEMBER of the base expression's type, is a precise
// declaration and becomes `writes`; an index, dereference or address-of is
// unwrapped first; anything else — a package-qualified base, a tuple target,
// a computed member — is published as `may_refer_to` a provider-local
// unresolved entity carrying the syntactic name, never as a fabricated
// precise write.
//
// Every other bound identifier in the operator's operand tree is a `reads` of
// its declaration, and a compound assignment or increment reads the operand
// it writes as well.
func (e *emitter) deriveReadsWrites(ctx context.Context) error {
	after := int64(math.MinInt64)
	for {
		// Only the operators that assign are fetched: an export's operator
		// calls are mostly arithmetic and comparison, and paging all of them
		// into Go to discard them costs more than the writes cost to derive.
		rows, err := e.sc.db.QueryContext(ctx, `SELECT c.id, c.method_full_name, a.owner FROM nodes c
			JOIN attr a ON a.id = c.id JOIN ents o ON o.id = a.owner
			WHERE c.label = '`+labelCall+`' AND c.id > ?
			AND (c.method_full_name LIKE '`+opAssignPrefix+`%'
				OR c.method_full_name IN ('<operator>.preIncrement', '<operator>.postIncrement',
					'<operator>.preDecrement', '<operator>.postDecrement'))
			ORDER BY c.id LIMIT ?`, after, pageSize)
		if err != nil {
			return internalErr("import reads/writes: %v", err)
		}
		type site struct {
			id, owner int64
			op        string
		}
		var page []site
		for rows.Next() {
			var s site
			if err := rows.Scan(&s.id, &s.op, &s.owner); err != nil {
				rows.Close()
				return internalErr("import reads/writes: %v", err)
			}
			page = append(page, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return internalErr("import reads/writes: %v", err)
		}
		if len(page) == 0 {
			return nil
		}
		for _, s := range page {
			write, alsoReads := isWriteOperator(s.op)
			if !write {
				continue
			}
			if err := e.deriveOne(ctx, s.id, s.op, s.owner, alsoReads); err != nil {
				return err
			}
		}
		after = page[len(page)-1].id
	}
}

func (e *emitter) deriveOne(ctx context.Context, site int64, op string, owner int64, alsoReads bool) error {
	args, err := e.arguments(ctx, site)
	if err != nil {
		return err
	}
	var written *argument
	for i := range args {
		if args[i].index.Valid && args[i].index.Int64 == 1 {
			written = &args[i]
			break
		}
	}
	if written == nil {
		return nil
	}
	decl, syntactic, err := e.resolveTarget(ctx, *written, 0)
	if err != nil {
		return err
	}
	if decl.ok {
		if err := e.project(ctx, "writes", owner, decl.id, site, op, detailAssignment, syntactic); err != nil {
			return err
		}
	} else {
		// An unresolved shape is an honest may_refer_to against a
		// provider-local entity, never a guessed declaration.
		if err := e.sc.exec(ctx, `INSERT OR IGNORE INTO ents(id, kind, external) VALUES(?, '`+kindUnresolved+`', 0)`, written.id); err != nil {
			return err
		}
		e.unresolved++
		if err := e.project(ctx, "may_refer_to", owner, written.id, site, op, detailAssignment, syntactic); err != nil {
			return err
		}
	}
	// Reads: every bound identifier in the operand tree, minus the operand
	// this call only writes.
	reads, err := e.boundIdentifiers(ctx, site, 0, map[int64]bool{})
	if err != nil {
		return err
	}
	for id, name := range reads {
		if decl.ok && id == decl.id && !alsoReads {
			continue
		}
		if id == owner {
			continue
		}
		if err := e.project(ctx, "reads", owner, id, site, op, detailAssignment, name); err != nil {
			return err
		}
	}
	return nil
}

// project appends one derived occurrence. Duplicates are collapsed when the
// occurrences are sorted (scratch.occurrences).
func (e *emitter) project(ctx context.Context, kind string, from, to, site int64, op, detail, targetName string) error {
	if from == to {
		return nil
	}
	if err := e.sc.exec(ctx, `INSERT INTO proj(kind, from_e, to_e, site, op, detail, target_name)
		VALUES(?,?,?,?,?,?,?)`, kind, from, to, site, op, detail, truncate(targetName, maxLabelBytes)); err != nil {
		return err
	}
	return e.sc.staged(ctx)
}

// arguments reads the operands of one operator call in argument order.
func (e *emitter) arguments(ctx context.Context, site int64) ([]argument, error) {
	rows, err := e.sc.db.QueryContext(ctx, `SELECT n.id, n.label, n.name, n.canonical_name,
		COALESCE((SELECT c.code FROM code c WHERE c.id = n.id ORDER BY c.seq LIMIT 1), ''), n.type_full_name,
		n.method_full_name, n.arg_index FROM edges a JOIN nodes n ON n.id = a.dst
		WHERE a.label = '`+edgeArgument+`' AND a.src = ? ORDER BY COALESCE(n.arg_index, -1), n.id`, site)
	if err != nil {
		return nil, internalErr("import reads/writes: %v", err)
	}
	defer rows.Close()
	var out []argument
	for rows.Next() {
		var a argument
		if err := rows.Scan(&a.id, &a.label, &a.name, &a.canonical, &a.code, &a.typeFullName, &a.methodFullName, &a.index); err != nil {
			return nil, internalErr("import reads/writes: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, internalErr("import reads/writes: %v", err)
	}
	return out, nil
}

// anchorOf is the entity a node anchors to, if it anchors at all.
func (e *emitter) anchorOf(ctx context.Context, node int64) (target, error) {
	var t target
	err := e.sc.db.QueryRowContext(ctx, `SELECT target FROM anchors WHERE node = ?`, node).Scan(&t.id)
	if err == sql.ErrNoRows {
		return target{}, nil
	}
	if err != nil {
		return target{}, internalErr("import reads/writes: %v", err)
	}
	t.ok = true
	return t, nil
}

// resolveTarget walks one written operand to the declaration it writes.
// It returns the declaration's graph id, or the unresolved target with the
// syntactic name the unresolved entity is published under.
func (e *emitter) resolveTarget(ctx context.Context, a argument, depth int) (target, string, error) {
	name := a.name
	if name == "" {
		name = a.canonical
	}
	if name == "" {
		name = truncate(a.code, maxLabelBytes)
	}
	if depth > maxOperandDepth {
		return target{}, name, nil
	}
	switch {
	case a.label == labelLocal || a.label == labelParamIn || a.label == labelMember:
		return target{id: a.id, ok: true}, name, nil
	case a.label == labelIdent || a.label == labelFieldIdMe || a.label == labelMethodRef:
		t, err := e.anchorOf(ctx, a.id)
		return t, name, err
	case a.label != labelCall:
		return target{}, name, nil
	}
	args, err := e.arguments(ctx, a.id)
	if err != nil {
		return target{}, name, err
	}
	switch {
	case unwrapOps[a.methodFullName]:
		for _, inner := range args {
			if inner.index.Valid && inner.index.Int64 == 1 {
				return e.resolveTarget(ctx, inner, depth+1)
			}
		}
	case fieldOps[a.methodFullName]:
		var base, field *argument
		for i := range args {
			switch {
			case args[i].index.Valid && args[i].index.Int64 == 1:
				base = &args[i]
			case args[i].label == labelFieldIdMe:
				field = &args[i]
			}
		}
		if field == nil {
			break
		}
		name = field.canonical
		if name == "" {
			name = field.name
		}
		if base == nil {
			break
		}
		member, err := e.memberOf(ctx, base.typeFullName, name)
		if err != nil {
			return target{}, name, err
		}
		if member.ok {
			return member, name, nil
		}
	}
	return target{}, name, nil
}

// memberOf finds the declaration of a named field on a type. The base
// expression's TYPE_FULL_NAME carries pointer and reference decoration the
// TYPE_DECL does not, so it is stripped before the lookup.
//
// The lookup is a single primary-key read of the members map project()
// materialized, rather than a three-table join over an unindexed m.name run
// once per field-access write site.
func (e *emitter) memberOf(ctx context.Context, typeFullName, field string) (target, error) {
	t := strings.Trim(strings.TrimPrefix(typeFullName, "&mut "), "*& ")
	if t == "" || t == "ANY" || field == "" {
		return target{}, nil
	}
	var id int64
	err := e.sc.db.QueryRowContext(ctx,
		`SELECT id FROM members WHERE type_full_name = ? AND name = ?`, t, field).Scan(&id)
	if err == sql.ErrNoRows {
		return target{}, nil
	}
	if err != nil {
		return target{}, internalErr("import reads/writes: %v", err)
	}
	return target{id: id, ok: true}, nil
}

// boundIdentifiers collects every identifier in one operator call's operand
// tree that the export bound to a declaration, keyed by that declaration.
func (e *emitter) boundIdentifiers(ctx context.Context, site int64, depth int, seen map[int64]bool) (map[int64]string, error) {
	out := map[int64]string{}
	if depth > maxOperandDepth || seen[site] {
		return out, nil
	}
	seen[site] = true
	args, err := e.arguments(ctx, site)
	if err != nil {
		return nil, err
	}
	for _, a := range args {
		switch a.label {
		case labelIdent, labelFieldIdMe, labelMethodRef:
			t, err := e.anchorOf(ctx, a.id)
			if err != nil {
				return nil, err
			}
			if !t.ok {
				continue
			}
			name := a.name
			if name == "" {
				name = a.canonical
			}
			out[t.id] = name
		case labelLocal, labelParamIn, labelMember:
			out[a.id] = a.name
		case labelCall:
			inner, err := e.boundIdentifiers(ctx, a.id, depth+1, seen)
			if err != nil {
				return nil, err
			}
			for k, v := range inner {
				out[k] = v
			}
		}
	}
	return out, nil
}
