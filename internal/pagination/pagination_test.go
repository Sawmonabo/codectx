package pagination_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestCursorTokensRejectForgeryExpiryAndPurpose protects the one signing
// primitive shared by search/graph/context cursors and source receipts.
//
// Failure mode: a token that verifies after a payload flip lets a client repin
// a query onto another generation or claim a receipt for bytes it never got; an
// expired token that verifies resurrects a released lease; a cursor accepted as
// a receipt (or the reverse) grants coverage credit from a search page.
func TestCursorTokensRejectForgeryExpiryAndPurpose(t *testing.T) {
	dir := t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cursor := pagination.Cursor{
		Endpoint:     "search",
		GenerationID: 7,
		AnalysisKey:  model.AnalysisKey(model.H("analysis", "a")),
		QueryHash:    model.H("query", "q"),
		LastKey:      "0000:abc",
		LeaseID:      model.H("lease", "l"),
		ExpiresAt:    now.Add(15 * time.Minute),
	}
	token, err := signer.EncodeCursor(cursor)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if len(token) > model.MaxTokenBytes {
		t.Fatalf("token is %d bytes, exceeds MaxTokenBytes", len(token))
	}
	got, err := signer.DecodeCursor(token, "search", now)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got != cursor {
		t.Fatalf("round trip = %+v, want %+v", got, cursor)
	}

	// A second signer over the same directory must load the same key, or a
	// long-running MCP process and a CLI invocation would reject each other's
	// cursors.
	again, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.DecodeCursor(token, "search", now); err != nil {
		t.Fatalf("second signer over the same key dir rejected the token: %v", err)
	}

	tampered := []byte(token)
	tampered[len(tampered)/2] ^= 0x01
	rejects := map[string]func() error{
		"tampered byte": func() error { _, err := signer.DecodeCursor(string(tampered), "search", now); return err },
		"expired":       func() error { _, err := signer.DecodeCursor(token, "search", now.Add(16*time.Minute)); return err },
		"wrong endpoint": func() error {
			_, err := signer.DecodeCursor(token, "graph", now)
			return err
		},
		"cursor presented as receipt": func() error {
			_, err := signer.Verify(token, pagination.PurposeReceipt, now)
			return err
		},
		"unknown version": func() error {
			raw, _ := base64.RawURLEncoding.DecodeString(token)
			raw[0] = 0x7f
			_, err := signer.Verify(base64.RawURLEncoding.EncodeToString(raw), pagination.PurposeCursor, now)
			return err
		},
		"oversized": func() error {
			_, err := signer.Verify(strings.Repeat("A", model.MaxTokenBytes+1), pagination.PurposeCursor, now)
			return err
		},
	}
	for name, check := range rejects {
		err := check()
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
			t.Errorf("%s: got %v, want %s", name, err, model.CodeCursorInvalid)
		}
	}
	if _, err := signer.Sign(pagination.PurposeReceipt, []byte("receipt"), now.Add(time.Minute)); err != nil {
		t.Fatalf("Sign(receipt): %v", err)
	}
}
