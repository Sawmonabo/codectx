package pagination

import (
	"encoding/json"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Cursor is the keyset continuation payload of Section 14.4. It pins the
// endpoint, the generation and its analysis key, the normalized query/filter/
// ordering hash, the last sort tuple or the spool that holds traversal state,
// and the lease that retains the generation. The lease TTL is the token TTL.
type Cursor struct {
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LastKey      string             `json:"last_key,omitempty"`
	SpoolID      string             `json:"spool_id,omitempty"`
	LeaseID      string             `json:"lease_id"`
	ExpiresAt    time.Time          `json:"expires_at"`
}

// maxLastKeyBytes bounds the serialized last sort tuple. Section 14.3 forbids
// serializing traversal state into a token; anything larger goes to a spool.
const maxLastKeyBytes = 1024

// Validate enforces the cursor's shape.
func (c Cursor) Validate() error {
	if c.Endpoint == "" || len(c.Endpoint) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor endpoint is missing or too long")
	}
	if c.GenerationID <= 0 {
		return cursorInvalid("cursor does not pin a generation")
	}
	if !model.ValidHexID(string(c.AnalysisKey)) || !model.ValidHexID(c.QueryHash) {
		return cursorInvalid("cursor analysis key and query hash must be well-formed identifiers")
	}
	// A cursor that names a SPOOL must name the lease that keeps that spool
	// readable. A pure keyset cursor retains nothing on disk: it carries its
	// whole position in the token, and the generation it pins is held by the
	// read snapshot of whichever call presents it, so it may carry no lease at
	// all. That is what lets a process with no writer -- one answering while
	// another indexes -- hand back a continuation instead of stopping.
	if c.SpoolID != "" && !model.ValidHexID(c.LeaseID) {
		return cursorInvalid("a cursor naming a spool must name the lease that retains it")
	}
	if c.LeaseID != "" && !model.ValidHexID(c.LeaseID) {
		return cursorInvalid("cursor lease id is malformed")
	}
	if len(c.LastKey) > maxLastKeyBytes {
		return cursorInvalid("cursor sort key exceeds its bound; use a spool")
	}
	if c.SpoolID != "" && !model.ValidHexID(c.SpoolID) {
		return cursorInvalid("cursor spool id is malformed")
	}
	if c.LastKey != "" && c.SpoolID != "" {
		return cursorInvalid("cursor names both a sort key and a spool")
	}
	if c.ExpiresAt.IsZero() {
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// EncodeCursor signs a cursor for the client.
func (s *Signer) EncodeCursor(c Cursor) (string, error) {
	c.ExpiresAt = c.ExpiresAt.UTC().Truncate(time.Second)
	if err := c.Validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", internalErr("cursor encoding: " + err.Error())
	}
	return s.Sign(PurposeCursor, payload, c.ExpiresAt)
}

// DecodeCursor verifies a client cursor for endpoint. A cursor issued by
// another endpoint is rejected: its sort tuple and query hash mean nothing
// here, and honouring it would silently repin the request.
func (s *Signer) DecodeCursor(token, endpoint string, now time.Time) (Cursor, error) {
	payload, err := s.Verify(token, PurposeCursor, now)
	if err != nil {
		return Cursor{}, err
	}
	var c Cursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return Cursor{}, cursorInvalid("cursor payload is malformed")
	}
	if err := c.Validate(); err != nil {
		return Cursor{}, err
	}
	if c.Endpoint != endpoint {
		return Cursor{}, cursorInvalid("cursor was issued by a different endpoint")
	}
	return c, nil
}
