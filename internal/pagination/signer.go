// Package pagination is the one shared cursor, lease and spool utility of
// Section 14.4. It signs bounded versioned payloads with an installation-local
// key, wraps retention leases in the configured TTL policy and keeps large
// traversal state in bounded private spool files.
//
// It imports only internal/model. Storage owns the lease rows and satisfies the
// LeaseStore interface declared here; storage does not import this package, so
// there is no cycle in either direction.
package pagination

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Purpose discriminates what a token authorizes. Section 16.3 requires that
// cursor tokens and source receipts are never accepted interchangeably.
type Purpose byte

const (
	PurposeCursor  Purpose = 1
	PurposeReceipt Purpose = 2
)

const (
	tokenVersion = 1
	keyFileName  = "token.key"
	keyBytes     = 32
	// header is version(1) + purpose(1) + expiry unix seconds(8).
	headerBytes = 10
	macBytes    = sha256.Size
	// maxPayloadBytes keeps the encoded token within model.MaxTokenBytes:
	// base64 expands 3 bytes to 4, so the raw token may hold at most 3/4 of
	// the ceiling.
	maxPayloadBytes = model.MaxTokenBytes*3/4 - headerBytes - macBytes
)

var encoding = base64.RawURLEncoding

// Signer signs and verifies tokens with the installation-local key.
type Signer struct {
	key []byte
}

// OpenSigner loads the key from dir, creating it with 0600 permissions on first
// use. The key is private to this installation; it is not a user
// authentication system (Section 14.4).
func OpenSigner(dir string) (*Signer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internalErr("token key directory: " + err.Error())
	}
	path := filepath.Join(dir, keyFileName)
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, keyBytes)
		if _, err := rand.Read(key); err != nil {
			return nil, internalErr("token key generation: " + err.Error())
		}
		// O_EXCL makes two concurrent first uses converge on one key: the
		// loser re-reads the winner's file instead of overwriting it.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return OpenSigner(dir)
		}
		if err != nil {
			return nil, internalErr("token key file: " + err.Error())
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, internalErr("token key write: " + err.Error())
		}
		if err := f.Close(); err != nil {
			return nil, internalErr("token key close: " + err.Error())
		}
	} else if err != nil {
		return nil, internalErr("token key read: " + err.Error())
	}
	if len(key) != keyBytes {
		return nil, &model.Error{Code: model.CodeStorageCorrupt,
			Message:     "token key file has an unexpected length",
			Remediation: "remove " + path + " to issue a new key; outstanding cursors and receipts become invalid"}
	}
	return &Signer{key: key}, nil
}

// Sign frames payload with the version, purpose and expiry, appends the
// HMAC-SHA256 tag and returns the base64url token.
func (s *Signer) Sign(purpose Purpose, payload []byte, expiresAt time.Time) (string, error) {
	if len(payload) > maxPayloadBytes {
		return "", &model.Error{Code: model.CodeResourceLimit,
			Message: "token payload exceeds the bounded token size"}
	}
	raw := make([]byte, 0, headerBytes+len(payload)+macBytes)
	raw = append(raw, tokenVersion, byte(purpose))
	raw = binary.BigEndian.AppendUint64(raw, uint64(expiresAt.Unix()))
	raw = append(raw, payload...)
	mac := hmac.New(sha256.New, s.key)
	mac.Write(raw)
	raw = mac.Sum(raw)
	return encoding.EncodeToString(raw), nil
}

// Verify checks the tag, version, purpose and expiry and returns the payload.
// Every rejection is CTX_CURSOR_INVALID; the message never echoes the token.
func (s *Signer) Verify(token string, purpose Purpose, now time.Time) ([]byte, error) {
	if len(token) > model.MaxTokenBytes {
		return nil, cursorInvalid("token exceeds the bounded token size")
	}
	raw, err := encoding.DecodeString(token)
	if err != nil || len(raw) < headerBytes+macBytes {
		return nil, cursorInvalid("token is malformed")
	}
	body, tag := raw[:len(raw)-macBytes], raw[len(raw)-macBytes:]
	mac := hmac.New(sha256.New, s.key)
	mac.Write(body)
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return nil, cursorInvalid("token signature does not verify")
	}
	if body[0] != tokenVersion {
		return nil, cursorInvalid("token version is not supported")
	}
	if Purpose(body[1]) != purpose {
		return nil, cursorInvalid("token was issued for a different purpose")
	}
	if expires := int64(binary.BigEndian.Uint64(body[2:headerBytes])); !now.Before(time.Unix(expires, 0)) {
		return nil, &model.Error{Code: model.CodeCursorInvalid, Message: "token has expired",
			Remediation: "restart the query or read; expired continuation state is not resurrected"}
	}
	return body[headerBytes:], nil
}

func cursorInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg}
}

func internalErr(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
