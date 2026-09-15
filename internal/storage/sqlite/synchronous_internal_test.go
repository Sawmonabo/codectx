package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The WAL writer's synchronous mode is a durability promise, so it is read back
// from the live writer connection rather than trusted from the DSN: a reader
// that ignored Options.Synchronous would still open cleanly and would still
// fsync every commit. `PRAGMA synchronous` reports 1 for NORMAL and 2 for FULL.
func TestWriterSynchronousFollowsOption(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
		// reported is what SynchronousMode renders the pragma as: the
		// configuration's own spelling, which is what the doctor's storage row
		// prints as the mode it read back.
		reported string
	}{
		{"", "1", "normal"},
		{"normal", "1", "normal"},
		{"full", "2", "full"},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(t.TempDir(), "codectx.db"), Options{Synchronous: tc.mode})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer s.Close()
			var got string
			if err := s.writer.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&got); err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got != tc.want {
				t.Fatalf("writer PRAGMA synchronous = %q, want %q for %q", got, tc.want, tc.mode)
			}
			// The diagnostic accessor reads the SAME connection and renders
			// it in the configuration's spelling. Reading a reader instead
			// would report FULL for every workspace, so this is asserted
			// against the live store rather than trusted from the mapping.
			reported, err := s.SynchronousMode(ctx)
			if err != nil {
				t.Fatalf("SynchronousMode: %v", err)
			}
			if reported != tc.reported {
				t.Fatalf("SynchronousMode = %q, want %q for %q", reported, tc.reported, tc.mode)
			}
			// Readers never commit; they stay pinned to FULL so every pooled
			// connection's pragma set remains verified against a known value.
			if err := s.readers.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&got); err != nil {
				t.Fatalf("read back reader: %v", err)
			}
			if got != "2" {
				t.Fatalf("reader PRAGMA synchronous = %q, want 2 (FULL)", got)
			}
		})
	}
}

// An unrecognized mode fails Open rather than silently choosing one.
func TestOpenRejectsUnknownSynchronous(t *testing.T) {
	_, err := Open(context.Background(), filepath.Join(t.TempDir(), "codectx.db"), Options{Synchronous: "off"})
	var typed *model.Error
	if err == nil || !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Fatalf("open with synchronous=off: got %v, want CTX_ARGUMENT_INVALID", err)
	}
	if want := `storage.synchronous is "off"; use "normal" or "full"`; typed.Message != want {
		t.Fatalf("message = %q, want %q", typed.Message, want)
	}
}
