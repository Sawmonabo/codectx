package ledger

import (
	"context"
	"testing"
)

// A process that never stops its ledger -- a server answering refresh after
// refresh -- opens a run each time. If a finished run were kept, the list would
// hold one entry for every run the process had ever done, and the two walks
// each flush makes over that list would grow with it for the life of the
// process. This is the bound: a run the collector has finished with is dropped.
//
// The failure it protects against is a server that grows for as long as it
// serves, which no test of one run can see.
func TestFinishedRunsAreNotRetained(t *testing.T) {
	l, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	defer func() { _ = l.Stop() }()

	const runs = 24
	for i := range runs {
		run, err := l.NewRun(KindIndex, repositoryIDHex)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		_, span := Start(run.Context(context.Background()), "capture", "")
		span.End(OutcomeOK, Measured{}, nil)
		run.Finish(OutcomeOK)
		if err := l.Flush(context.Background()); err != nil {
			t.Fatalf("flush after run %d: %v", i, err)
		}
	}

	l.runsMu.Lock()
	held := len(l.runs)
	l.runsMu.Unlock()
	if held != 0 {
		t.Errorf("the ledger still holds %d of %d finished runs: a process that keeps serving keeps growing", held, runs)
	}
}

// repositoryIDHex is the wire shape of a repository identity; the ledger never
// interprets it.
const repositoryIDHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
