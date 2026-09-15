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
