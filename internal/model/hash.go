package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
)

// IDHexLen is the length of a public content-derived ID: SHA-256 rendered as
// lowercase hex. SQLite stores the decoded 32 bytes (Section 9.1).
const IDHexLen = 2 * sha256.Size

// Hasher computes the canonical digest H of Section 9.1. Each component is
// prefixed with its unsigned 64-bit big-endian byte length before it is
// absorbed, so no arrangement of bytes can forge a component boundary.
//
// Components are appended one at a time, which is what lets a repository-sized
// input be folded incrementally: a caller streams a sorted manifest entry by
// entry and never concatenates it into one string. The resulting aggregate
// digest is then an ordinary component of a fixed-arity H call, which is why
// Snapshot.ManifestHash and UnitSpec.InputHash exist as their own fields.
type Hasher struct {
	h      hash.Hash
	length [8]byte
}

// NewHasher starts a digest in the given domain. The domain is absorbed as the
// first length-framed component, so two domains can never alias.
func NewHasher(domain string) *Hasher {
	h := &Hasher{h: sha256.New()}
	h.AddString(domain)
	return h
}

// AddString appends one length-framed component.
func (h *Hasher) AddString(component string) {
	binary.BigEndian.PutUint64(h.length[:], uint64(len(component)))
	// hash.Hash never reports an error.
	h.h.Write(h.length[:])
	h.h.Write([]byte(component))
}

// AddBytes appends one length-framed component without converting to string.
func (h *Hasher) AddBytes(component []byte) {
	binary.BigEndian.PutUint64(h.length[:], uint64(len(component)))
	h.h.Write(h.length[:])
	h.h.Write(component)
}

// Sum returns the lowercase hex digest. The hasher may continue to be used
// afterwards; Sum does not reset or consume it.
func (h *Hasher) Sum() string {
	return hex.EncodeToString(h.h.Sum(nil))
}

// H is the fixed-arity canonical hash of Section 9.1. Every identity definition
// in that section maps to exactly one H call with exactly its listed arity; an
// absent optional component is passed as the empty string so the arity stays
// fixed and an omission cannot alias a present value.
func H(domain string, components ...string) string {
	h := NewHasher(domain)
	for _, c := range components {
		h.AddString(c)
	}
	return h.Sum()
}

// ValidHexID reports whether s is a well-formed public ID: exactly IDHexLen
// lowercase hex characters. Uppercase is rejected so one byte string has one
// wire spelling.
func ValidHexID(s string) bool {
	if len(s) != IDHexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// DecodeID converts a public ID to the 32 raw bytes SQLite stores.
func DecodeID(id string) ([]byte, error) {
	if !ValidHexID(id) {
		return nil, invalid("identifier %q is not %d lowercase hex characters", truncateForMessage(id), IDHexLen)
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		return nil, invalid("identifier is not valid hex: %v", err)
	}
	return raw, nil
}

// EncodeID converts stored bytes back to the public lowercase hex ID.
func EncodeID(raw []byte) (string, error) {
	if len(raw) != sha256.Size {
		return "", invalid("stored identifier is %d bytes, want %d", len(raw), sha256.Size)
	}
	return hex.EncodeToString(raw), nil
}
