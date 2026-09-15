package sqlite

// Test hooks. They exist so a test can prove behaviour the production
// constants make unreachable on a fixture -- a list that straddles a part
// boundary needs either a repository-sized fixture or a smaller part.

// SetEdgePartBytes shrinks the edge-stream part size for one test and returns a
// function that restores it. It is not a user setting: part size bounds a
// working set, never what is stored or answered.
func SetEdgePartBytes(n int) func() {
	prev := edgePartBytes
	edgePartBytes = n
	return func() { edgePartBytes = prev }
}

// OutgoingEdgeQuery and IncomingEdgeQuery are the two ordered scans the build
// streams, exported so the query-plan test asserts the SQL that ships.
func OutgoingEdgeQuery() string { return outgoingEdgeQuery() }
func IncomingEdgeQuery() string { return incomingEdgeQuery() }

// UnitInstanceQuery is the bare instance scan the per-unit lexical fold
// streams, and MergePartQuery the one statement a merge pass issues per part,
// exported so the query-plan test asserts the SQL that ships.
func UnitInstanceQuery(vocab string) string { return unitInstanceQuery(vocab) }

func MergePartQuery(table, column string) string {
	return `SELECT bytes FROM ` + table + ` WHERE ` + column + ` = 1 AND stream = 'term.dir' AND part = 0`
}

// UnitLexicalPartsTable is the table a sealed unit's packed streams live in.
const UnitLexicalPartsTable = unitLexicalPartsTable

// EvidenceCountQuery is the build's third ordered scan.
func EvidenceCountQuery() string { return evidenceCountQuery() }
