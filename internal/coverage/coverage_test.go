package coverage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/source"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// This file is the whole test budget for the coverage package: one fixture, one
// oracle and one scenario table whose rows every fill-in lane adds under its own
// marker. A test here exists only to protect an invariant whose silent breakage
// serves wrong source bytes, grants false full-read credit or breaks
// determinism; getters, enum spellings, forwarding and wiring get no row.

// --- identities -------------------------------------------------------------

// hexID builds one of the 64-character lowercase hex identifiers every model
// validator requires, so a fixture id is recognisable by its repeated byte.
func hexID(b byte) string { return strings.Repeat(fmt.Sprintf("%02x", b), 32) }

var (
	repoID     = model.RepositoryID(hexID(0x01))
	snapshotID = model.SnapshotID(hexID(0x02))
	manifestID = model.ManifestID(hexID(0x03))

	// Two sessions and two actors: coverage belongs to one actor, one session
	// and one source hash, so a receipt issued to one of these must never grant
	// credit inside the other.
	sessionA = model.SessionID(hexID(0x10))
	sessionB = model.SessionID(hexID(0x11))
	actorA   = "actor-a"
	actorB   = "actor-b"

	fixtureBinding = model.Binding{RepositoryID: repoID, SnapshotID: snapshotID, GenerationID: 1}
)

// --- fixture files ----------------------------------------------------------

// fixtureFile is one snapshot file the fake serves. checkpoints are the sparse
// line checkpoints a real snapshot.Index carries; spacing them unevenly is what
// lets a row read at an offset far from the checkpoint before it, which is the
// case that breaks a chunk cap that forgets the checkpoint prefix.
type fixtureFile struct {
	id          model.FileID
	path        string
	data        []byte
	checkpoints []source.Checkpoint
}

func (f *fixtureFile) hash() string { return string(f.id) }

func (f *fixtureFile) version() model.FileVersion {
	return model.FileVersion{
		ID: f.id, Path: f.path, Status: model.FileTracked,
		Size: int64(len(f.data)), ContentHash: f.hash(),
	}
}

// checkpointFor is snapshot.Index.CheckpointFor over the fixture's sparse list:
// the last checkpoint at or before offset.
func (f *fixtureFile) checkpointFor(offset uint64) source.Checkpoint {
	cp := source.Checkpoint{Byte: 0, Line: 1}
	for _, c := range f.checkpoints {
		if c.Byte <= offset {
			cp = c
		}
	}
	return cp
}

// longLine is one line longer than any raw cap a fixture row configures, so a
// read of it must split with PartialLine and still make progress.
func longLine() []byte {
	return append([]byte(strings.Repeat("x", 4096)), '\n')
}

// multiByte is UTF-8 whose code points straddle every round offset, so a naive
// slicer lands inside a sequence rather than on a rune boundary.
func multiByte() []byte {
	return []byte(strings.Repeat("é中\U0001f600\n", 512))
}

// newFixtureFiles builds the five files digest section 9 names. Each one exists
// because it breaks a different part of the read path.
func newFixtureFiles() []*fixtureFile {
	return []*fixtureFile{
		// Empty: its full_served can only come from an explicitly confirmed
		// zero-length EOF receipt, never from the vacuously complete union.
		{id: model.FileID(hexID(0x20)), path: "empty.txt", data: nil},
		// CRLF with a final line carrying no trailing newline: both must survive
		// byte for byte, with CRLF counted as two bytes and one line break.
		{id: model.FileID(hexID(0x21)), path: "crlf.txt", data: []byte("alpha\r\nbeta\r\ngamma")},
		// A line longer than the raw cap: the over-budget split.
		{id: model.FileID(hexID(0x22)), path: "long.txt", data: longLine()},
		// Invalid UTF-8: must come back as lossless base64, never coerced into
		// replacement characters. Byte 0 is ASCII so offset 0 stays a boundary.
		{id: model.FileID(hexID(0x23)), path: "binary.bin", data: []byte{'#', 0xff, 0xfe, 0x00, 0x80, 0xff, '\n'}},
		// Multi-byte UTF-8 with a checkpoint far behind the interesting offsets.
		{id: model.FileID(hexID(0x24)), path: "utf8.txt", data: multiByte(),
			checkpoints: []source.Checkpoint{{Byte: 0, Line: 1}, {Byte: 1210, Line: 101}}},
	}
}

// --- the byte-set oracle ----------------------------------------------------
//
// byteSet is the independent answer to "what has this actor confirmed for this
// file". It is a bitset over the file's bytes plus a SEPARATE eofConfirmed bit,
// because for a zero-length file the union [0,0) is vacuously complete and a
// union-only rule would report a false full_served. Storage models the same bit
// separately (a served_ranges row must satisfy end_byte > start_byte, so a
// zero-length confirmation is recorded on the issued chunk instead).
type byteSet struct {
	served       []bool
	eofConfirmed bool
}

// confirm credits one confirmed interval. A zero-length interval is the EOF
// receipt: on an empty file it is the only thing that can grant full coverage,
// and on a nonempty file it adds no bytes and covers nothing earlier.
func (b *byteSet) confirm(start, end uint64) {
	if start == end {
		b.eofConfirmed = true
		return
	}
	for i := start; i < end && int(i) < len(b.served); i++ {
		b.served[i] = true
	}
}

func (b *byteSet) confirmedBytes() int64 {
	var n int64
	for _, s := range b.served {
		if s {
			n++
		}
	}
	return n
}

// state reproduces the two branches of the storage coverage switch: the
// zero-length confirmed-EOF branch for an empty file, and the union == size
// branch for every other file.
func (b *byteSet) state() model.CoverageState {
	if len(b.served) == 0 {
		if b.eofConfirmed {
			return model.CoverageFullServed
		}
		return model.CoverageUnserved
	}
	switch n := b.confirmedBytes(); {
	case n == int64(len(b.served)):
		return model.CoverageFullServed
	case n > 0:
		return model.CoveragePartialServed
	default:
		return model.CoverageUnserved
	}
}

// --- the fake store ---------------------------------------------------------
//
// fakeStore is a TEST DOUBLE reproducing the documented semantics of
// internal/storage/sqlite/state.go: the actor check, the issued/confirmed
// split, and the coverage state switch. It is not a production interval
// merger, and no production code in this package may mirror it -- Go-side
// interval merging is the duplicate implementation the completion gate forbids.
// It keeps ONE representation of served bytes, the byteSet above, so the fake
// and the oracle can never drift apart.

type fakeChunk struct {
	id         string
	session    model.SessionID
	actor      string
	file       model.FileID
	hash       string
	start, end uint64
	confirmed  bool
	expiresAt  time.Time
}

type fakeSession struct {
	rec      sqlite.SessionRecord
	coverage map[model.FileID]*byteSet
	waived   map[model.FileID]bool
}

type fakeStore struct {
	now      func() time.Time
	files    map[model.FileID]*fixtureFile
	order    []model.FileID
	required map[model.FileID]model.Requirement
	sessions map[model.SessionID]*fakeSession
	chunks   map[string]*fakeChunk
	nextID   int

	// confirmCalls counts ConfirmChunks calls. Storage refuses a foreign
	// chunk too, so a row that only checks the returned code cannot tell the
	// service's own session/actor/snapshot cross-check from the store's: the
	// counter is what distinguishes "refused before storage was asked" from
	// "the store happened to catch it".
	confirmCalls int

	// scopeComplete is what the fixture manifest reports. Section 15.3's Q7
	// compiles an ambiguous or empty scope into a manifest with
	// ScopeComplete=false, which is the only thing that separates
	// "everything required has been read" from "nothing was required".
	scopeComplete bool
}

var _ Sessions = (*fakeStore)(nil)

func typed(code, format string, args ...any) *model.Error {
	return &model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// session applies the actor and lifecycle gate every mutating storage method
// applies. An empty actor is refused outright: the real store skips its actor
// check in that case, which is exactly why callers Validate first.
func (s *fakeStore) session(id model.SessionID, actor string) (*fakeSession, error) {
	fs, ok := s.sessions[id]
	if !ok {
		return nil, typed(model.CodeCursorInvalid, "session is not known")
	}
	if actor == "" || fs.rec.ActorID != actor {
		return nil, typed(model.CodeActorMismatch, "session belongs to another actor")
	}
	if fs.rec.ClosedAt != nil {
		return fs, typed(model.CodeSessionExpired, "session is closed")
	}
	if !fs.rec.ExpiresAt.After(s.now()) {
		return fs, typed(model.CodeSessionExpired, "session has expired")
	}
	return fs, nil
}

func (s *fakeStore) Manifest(ctx context.Context, id model.ManifestID) (model.ContextManifest, error) {
	if id != manifestID {
		return model.ContextManifest{}, typed(model.CodeCursorInvalid, "manifest is not known")
	}
	return model.ContextManifest{
		ID: manifestID, Binding: fixtureBinding, Phase: model.PhaseSweep,
		RequestHash: hexID(0x30), PolicyVersion: "codectx.context.v1", CanonicalHash: hexID(0x31),
		EntryCount: len(s.order), ScopeComplete: s.scopeComplete,
		EstimateMethod: model.EstimateMethodUTF8Bytes, CreatedAt: s.now(),
	}, nil
}

// ManifestEntries returns entries in the frozen persistence order: ordinals
// 0..n-1 are the canonical reading order and the required_full entries occupy a
// prefix, so Next walks ascending and never sorts.
func (s *fakeStore) ManifestEntries(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ContextEntry, error) {
	if _, err := s.Manifest(ctx, id); err != nil {
		return nil, err
	}
	out := make([]model.ContextEntry, 0, limit)
	for i, fid := range s.order {
		if i <= afterOrdinal && afterOrdinal >= 0 {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, model.ContextEntry{
			Ordinal: i, FileID: fid, Requirement: s.required[fid],
			EstimatedBytes: int64(len(s.files[fid].data)), Reasons: []string{"fixture"},
		})
	}
	return out, nil
}

func (s *fakeStore) ManifestSlices(ctx context.Context, id model.ManifestID, afterIndex int, limit int) ([]model.ContextSlice, error) {
	if _, err := s.Manifest(ctx, id); err != nil {
		return nil, err
	}
	if afterIndex >= 0 {
		return nil, nil
	}
	ordinals := make([]int, len(s.order))
	for i := range s.order {
		ordinals[i] = i
	}
	return []model.ContextSlice{{Index: 0, EntryOrdinals: ordinals}}, nil
}

func (s *fakeStore) ManifestExcluded(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ExcludedContextEntry, error) {
	if _, err := s.Manifest(ctx, id); err != nil {
		return nil, err
	}
	return nil, nil
}

func (s *fakeStore) OpenSession(ctx context.Context, open model.SessionOpen) (model.SessionID, error) {
	if err := open.Validate(); err != nil {
		return "", err
	}
	if existing, ok := s.sessions[open.ID]; ok {
		return existing.rec.ID, nil
	}
	fs := &fakeSession{
		rec: sqlite.SessionRecord{
			ID: open.ID, ActorID: open.ActorID, Binding: fixtureBinding, ManifestID: open.ManifestID,
			Phase: model.PhaseSweep, State: model.StateSweepOpen, StateVersion: 1, ScopeVersion: 1,
			CreatedAt: s.now(), ExpiresAt: open.ExpiresAt,
		},
		coverage: map[model.FileID]*byteSet{},
		waived:   map[model.FileID]bool{},
	}
	// OpenSession populates session_files from the manifest; the fake does the
	// same so an unread required file is visible to Coverage from the start.
	for _, fid := range s.order {
		fs.coverage[fid] = &byteSet{served: make([]bool, len(s.files[fid].data))}
	}
	s.sessions[open.ID] = fs
	return open.ID, nil
}

func (s *fakeStore) Session(ctx context.Context, id model.SessionID, actor string) (sqlite.SessionRecord, error) {
	fs, err := s.session(id, actor)
	if fs == nil {
		return sqlite.SessionRecord{}, err
	}
	// Like the real store, an expired session still returns its record beside
	// the error so status can report honestly instead of reporting "no record".
	return fs.rec, err
}

func (s *fakeStore) IssueChunk(ctx context.Context, c model.IssuedChunk) error {
	if err := c.Validate(); err != nil {
		return err
	}
	fs, err := s.session(c.SessionID, c.ActorID)
	if err != nil {
		return err
	}
	f, ok := s.files[c.FileID]
	if !ok || f.hash() != c.ContentHash || fs.coverage[c.FileID] == nil {
		return typed(model.CodeScopeIncomplete, "file is not in the session's pinned scope")
	}
	if c.Bytes.End > uint64(len(f.data)) {
		return typed(model.CodeArgumentInvalid, "chunk ends past the stored file size")
	}
	if _, dup := s.chunks[c.ID]; dup {
		return typed(model.CodeVersionConflict, "chunk has already been issued")
	}
	s.chunks[c.ID] = &fakeChunk{
		id: c.ID, session: c.SessionID, actor: c.ActorID, file: c.FileID, hash: c.ContentHash,
		start: c.Bytes.Start, end: c.Bytes.End, expiresAt: c.ExpiresAt,
	}
	return nil
}

// UnconfirmedChunks deliberately takes no actor and loads no session, exactly
// like the real method, so a caller that forgets to gate on Session first leaks
// a cross-actor count.
func (s *fakeStore) UnconfirmedChunks(ctx context.Context, session model.SessionID) (int64, error) {
	var n int64
	for _, c := range s.chunks {
		if c.session == session && !c.confirmed {
			n++
		}
	}
	return n, nil
}

// ConfirmChunks takes raw chunk ids, not receipt tokens, and fails the whole
// batch on an unknown, expired or foreign chunk. Re-confirming an already
// confirmed chunk is idempotent and must not add credit twice.
func (s *fakeStore) ConfirmChunks(ctx context.Context, session model.SessionID, actor string, ids []string) error {
	s.confirmCalls++
	fs, err := s.session(session, actor)
	if err != nil {
		return err
	}
	if len(ids) == 0 || len(ids) > model.MaxReceiptsPerConfirmation {
		return typed(model.CodeArgumentInvalid, "a confirmation carries 1 to %d chunks, got %d",
			model.MaxReceiptsPerConfirmation, len(ids))
	}
	batch := make([]*fakeChunk, 0, len(ids))
	for _, id := range ids {
		c, ok := s.chunks[id]
		if !ok || c.session != session || c.actor != actor {
			return typed(model.CodeCursorInvalid, "a chunk in the batch is not this session's")
		}
		if !c.expiresAt.After(s.now()) {
			return typed(model.CodeCursorInvalid, "a chunk in the batch has expired")
		}
		batch = append(batch, c)
	}
	for _, c := range batch {
		if c.confirmed {
			continue
		}
		c.confirmed = true
		fs.coverage[c.file].confirm(c.start, c.end)
	}
	return nil
}

func (s *fakeStore) fileCoverage(fs *fakeSession, fid model.FileID) model.FileCoverage {
	set := fs.coverage[fid]
	return model.FileCoverage{
		FileID: fid, ContentHash: s.files[fid].hash(), Size: int64(len(s.files[fid].data)),
		ConfirmedBytes: set.confirmedBytes(), Requirement: s.required[fid],
		State: set.state(), Waived: fs.waived[fid],
	}
}

// Coverage pages by keyset on file_id, like the real method, and reports beside
// CTX_SESSION_EXPIRED rather than swallowing the records.
func (s *fakeStore) Coverage(ctx context.Context, session model.SessionID, actor string, after model.FileID, limit int) ([]model.FileCoverage, error) {
	fs, err := s.session(session, actor)
	if fs == nil {
		return nil, err
	}
	ids := make([]model.FileID, 0, len(fs.coverage))
	for fid := range fs.coverage {
		ids = append(ids, fid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]model.FileCoverage, 0, limit)
	for _, fid := range ids {
		if fid <= after {
			continue
		}
		if limit > 0 && len(out) == limit {
			break
		}
		out = append(out, s.fileCoverage(fs, fid))
	}
	return out, err
}

// CoverageSummary is the single-round-trip aggregate L6 adds to the real store.
func (s *fakeStore) CoverageSummary(ctx context.Context, session model.SessionID, actor string) (int64, int64, int64, error) {
	fs, err := s.session(session, actor)
	if fs == nil {
		return 0, 0, 0, err
	}
	var required, fullyServed, waived int64
	for fid := range fs.coverage {
		if s.required[fid] != model.RequirementFull {
			continue
		}
		required++
		if s.fileCoverage(fs, fid).State == model.CoverageFullServed {
			fullyServed++
		}
		if fs.waived[fid] {
			waived++
		}
	}
	return required, fullyServed, waived, err
}

// FilePath is the bounded path reader FX-D16-A's F4 adds to Sessions so Next
// can fill NextContextItem.Path. It is written here ahead of that lane because
// the widened interface is unsatisfiable without it and FX-D16-A does not own
// this file; until F4 lands nothing in the package calls it.
func (s *fakeStore) FilePath(ctx context.Context, snapshot model.SnapshotID, id model.FileID) (string, error) {
	if snapshot != snapshotID {
		return "", typed(model.CodeScopeIncomplete, "snapshot is not the fixture's")
	}
	f, ok := s.files[id]
	if !ok {
		return "", typed(model.CodeScopeIncomplete, "file is not in the snapshot")
	}
	return f.path, nil
}

// AcknowledgeFile refuses unless coverage is already full: a client assertion
// never creates coverage.
func (s *fakeStore) AcknowledgeFile(ctx context.Context, session model.SessionID, actor string, file model.FileID) error {
	fs, err := s.session(session, actor)
	if err != nil {
		return err
	}
	if fs.coverage[file] == nil {
		return typed(model.CodeScopeIncomplete, "file is not in the session's pinned scope")
	}
	if fs.coverage[file].state() != model.CoverageFullServed {
		return typed(model.CodeCoverageIncomplete, "file is not fully served for this actor")
	}
	return nil
}

// AdvanceSession reproduces the transition table, including the rule that
// complete has no outgoing edge, so Close on a completed session conflicts.
func (s *fakeStore) AdvanceSession(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WorkflowStatus{}, err
	}
	fs, err := s.session(req.SessionID, req.ActorID)
	if err != nil {
		return model.WorkflowStatus{}, err
	}
	allowed := map[model.WorkflowState][]model.WorkflowState{
		model.StateSweepOpen:       {model.StateVerifyOpen, model.StateClosed},
		model.StateVerifyOpen:      {model.StateConsolidateOpen, model.StateClosed},
		model.StateConsolidateOpen: {model.StateComplete, model.StateClosed},
	}
	if req.ExpectedVersion != fs.rec.StateVersion {
		return model.WorkflowStatus{}, typed(model.CodeVersionConflict,
			"session is at state version %d", fs.rec.StateVersion)
	}
	ok := false
	for _, t := range allowed[fs.rec.State] {
		if t == req.Target {
			ok = true
		}
	}
	if !ok {
		return model.WorkflowStatus{}, typed(model.CodeVersionConflict,
			"there is no transition from %s to %s", fs.rec.State, req.Target)
	}
	fs.rec.State = req.Target
	fs.rec.StateVersion++
	if req.Target == model.StateClosed {
		closed := s.now()
		fs.rec.ClosedAt = &closed
	}
	return model.WorkflowStatus{
		SessionID: fs.rec.ID, State: fs.rec.State, StateVersion: fs.rec.StateVersion,
		ScopeVersion: fs.rec.ScopeVersion, ManifestID: fs.rec.ManifestID, Phase: fs.rec.Phase,
	}, nil
}

// blockingSessions is a Sessions whose Session call never returns on its own,
// so only a deadline the service imposes can end the request. It EMBEDS the
// fake rather than reimplementing it, so it keeps satisfying Sessions however
// that interface is widened, and overrides the one method Read reaches first.
type blockingSessions struct{ *fakeStore }

// errStoreNeverReturned is what the blocker answers once its own escape hatch
// fires. A test that hangs is worse than a test that fails, so the row can name
// the missing deadline instead of blocking the package's test binary.
var errStoreNeverReturned = errors.New("the store blocked past the query timeout and the read was never cut off")

func (b blockingSessions) Session(ctx context.Context, id model.SessionID, actor string) (sqlite.SessionRecord, error) {
	select {
	case <-ctx.Done():
		return sqlite.SessionRecord{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return sqlite.SessionRecord{}, errStoreNeverReturned
	}
}

// --- the fake source --------------------------------------------------------

// fakeSource reproduces snapshot.View.Read: it rejects a range past the file
// and a boundary inside a UTF-8 sequence, and it derives positions from the
// nearest checkpoint through source.NewCursorAt rather than scanning from byte
// zero, so a row that reads far into a file exercises the real position path.
type fakeSource struct{ files map[model.FileID]*fixtureFile }

var _ Source = (*fakeSource)(nil)

// Checkpoint mirrors snapshot.View.Checkpoint over the fixture's sparse list,
// so a row can read at an offset far from the checkpoint before it and prove
// that the raw cap subtracted the real prefix rather than an assumed one.
func (s *fakeSource) Checkpoint(ctx context.Context, id model.FileID, offset uint64) (uint64, error) {
	f, ok := s.files[id]
	if !ok {
		return 0, typed(model.CodeScopeIncomplete, "file is not in the snapshot")
	}
	if offset > uint64(len(f.data)) {
		return 0, typed(model.CodeArgumentInvalid,
			"byte offset %d is past the %d-byte file", offset, len(f.data))
	}
	return f.checkpointFor(offset).Byte, nil
}

func (s *fakeSource) Read(ctx context.Context, id model.FileID, r model.ByteRange) (snapshot.Range, model.FileVersion, error) {
	f, ok := s.files[id]
	if !ok {
		return snapshot.Range{}, model.FileVersion{}, typed(model.CodeScopeIncomplete, "file is not in the snapshot")
	}
	fv := f.version()
	if err := r.Validate("byte_range"); err != nil {
		return snapshot.Range{}, fv, err
	}
	if r.End > uint64(len(f.data)) {
		return snapshot.Range{}, fv, typed(model.CodeArgumentInvalid,
			"byte range ends at %d, past the %d-byte file", r.End, len(f.data))
	}
	cp := f.checkpointFor(r.Start)
	cursor, err := source.NewCursorAt(f.data[cp.Byte:r.End], cp.Byte, cp.Line)
	if err != nil {
		return snapshot.Range{}, fv, err
	}
	start, err := cursor.PositionAt(r.Start)
	if err != nil {
		return snapshot.Range{}, fv, err
	}
	end, err := cursor.PositionAt(r.End)
	if err != nil {
		return snapshot.Range{}, fv, err
	}
	// A fresh copy on every call, deliberately. The production View returns a
	// slice aliasing its checkpoint-prefixed window, so a caller that retains it
	// must copy; handing out a shared buffer here would hide that bug from every
	// row below. Do not "optimize" this into a shared slice.
	out := make([]byte, r.End-r.Start)
	copy(out, f.data[r.Start:r.End])
	return snapshot.Range{Bytes: out, Start: start, End: end}, fv, nil
}

// --- harness ----------------------------------------------------------------

// harness is one fully wired fixture: the fake store, the fake source, a real
// pagination.Signer (receipts are really signed and really verified) and the
// service under test.
type harness struct {
	t     *testing.T
	store *fakeStore
	src   *fakeSource
	files map[model.FileID]*fixtureFile
	sign  *pagination.Signer
	svc   *Service
	now   time.Time
}

// fixtureLimits keeps every bound small enough that a row proves a boundary in
// microseconds instead of allocating megabytes, while staying inside the model
// ceilings the validators enforce.
func fixtureLimits() Limits {
	return Limits{
		ChunkBytes: 64, MaxChunkBytes: 4096,
		MaxSourceResponseBytes: 1 << 20, MaxMetadataResponseBytes: 1 << 18,
		MaxReceiptsPerConfirmation:     model.MaxReceiptsPerConfirmation,
		MaxUnconfirmedChunksPerSession: 4,
		MaxPageItems:                   3,
		SessionTTL:                     time.Hour, QueryTimeout: 5 * time.Second, ReceiptTTL: time.Hour,
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	files := map[model.FileID]*fixtureFile{}
	order := make([]model.FileID, 0, 5)
	required := map[model.FileID]model.Requirement{}
	for i, f := range newFixtureFiles() {
		files[f.id] = f
		order = append(order, f.id)
		// required_full occupies a prefix of the ordinals, which is the frozen
		// manifest contract Next walks.
		if i < 4 {
			required[f.id] = model.RequirementFull
		} else {
			required[f.id] = model.RequirementRecommended
		}
	}
	store := &fakeStore{
		now: func() time.Time { return now }, files: files, order: order,
		required: required, sessions: map[model.SessionID]*fakeSession{},
		chunks: map[string]*fakeChunk{}, scopeComplete: true,
	}
	src := &fakeSource{files: files}

	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	h := &harness{t: t, store: store, src: src, files: files, sign: signer, now: now}
	for _, id := range []model.SessionID{sessionA, sessionB} {
		actor := actorA
		if id == sessionB {
			actor = actorB
		}
		open := model.SessionOpen{
			ID: id, ActorID: actor, OpenRequestHash: hexID(0x40),
			ManifestID: manifestID, ExpiresAt: now.Add(time.Hour),
		}
		if _, err := store.OpenSession(context.Background(), open); err != nil {
			t.Fatalf("open session %s: %v", id, err)
		}
	}
	h.svc, err = New(Options{
		Sessions: store,
		OpenSource: func(ctx context.Context, snap model.SnapshotID) (Source, error) {
			if snap != snapshotID {
				return nil, typed(model.CodeScopeIncomplete, "snapshot is not the fixture's")
			}
			return src, nil
		},
		Signer: signer, Limits: fixtureLimits(),
		Now: func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatalf("build coverage service: %v", err)
	}
	return h
}

// file returns one fixture file by id, failing the row rather than panicking.
func (h *harness) file(id model.FileID) *fixtureFile {
	h.t.Helper()
	f, ok := h.files[id]
	if !ok {
		h.t.Fatalf("fixture has no file %s", id)
	}
	return f
}

// checkCoverage asserts the reported coverage of one file against the counts
// the CALLING ROW states, not against a replay of the fake's own records.
//
// The replayed oracle this replaces could not fail for a defect in the code
// under review: it fed the fake's confirmed chunks back through the same
// byteSet.confirm the fake had already used to build the state it was being
// compared with, so a wrong range issued by Read was recorded and replayed
// identically. Here the expectation comes from the fixture -- the row knows the
// file's size and which of its bytes a confirmed receipt covered -- so credit
// granted for bytes no receipt covers, or withheld for bytes one does, fails.
func (h *harness) checkCoverage(session model.SessionID, actor string, id model.FileID, wantBytes int64, wantState model.CoverageState) {
	h.t.Helper()
	got, err := h.store.Coverage(context.Background(), session, actor, "", 0)
	if err != nil {
		h.t.Fatalf("coverage: %v", err)
	}
	for _, fc := range got {
		if fc.FileID != id {
			continue
		}
		if fc.State != wantState || fc.ConfirmedBytes != wantBytes {
			h.t.Fatalf("file %s: coverage reports %s with %d confirmed bytes, the fixture says %s with %d",
				id, fc.State, fc.ConfirmedBytes, wantState, wantBytes)
		}
		return
	}
	h.t.Fatalf("file %s is not in the session's coverage", id)
}

// --- the scenario table -----------------------------------------------------
//
// One table, one marker per fill-in lane. A lane appends its rows under its own
// marker and nowhere else, so two lanes never touch the same lines. Every row
// names the failure mode it protects against in a comment and is mutation
// proved before it is committed.

type scenario struct {
	name string
	run  func(t *testing.T, h *harness)
}

var scenarios = []scenario{
	// L1 rows

	// A maximum-size chunk read at an offset far from the checkpoint before it.
	// snapshot.View.Read seeks back to that checkpoint and asks CAS.ReadRange
	// for [checkpoint, end), and CAS.ReadRange refuses a span over
	// model.MaxRawChunkBytes, so a read path that sizes the chunk from the
	// request alone serves a 1 MiB chunk only when the offset happens to sit on
	// a checkpoint and fails with CTX_RESOURCE_LIMIT everywhere else. Read must
	// subtract the real prefix, which caps this chunk exactly at the ceiling
	// measured from the checkpoint rather than from the offset.
	//
	// needs FX-D16-A: F1 DELETES the clamp this row asserts. Once View.Read
	// spans the prefix in successive reads, maxRawForWire keeps the full
	// 1 MiB and the chunk ends at the file size instead of at
	// checkpoint+MaxRawChunkBytes, so both the issued-range and the
	// NextOffset assertions below fail by design. The invariant is retired,
	// not broken: A's snapshot row proves the replacement. Left green at this
	// HEAD; the controller adapts or deletes it at merge.
	{"read/chunk far from a checkpoint is capped from the checkpoint", func(t *testing.T, h *harness) {
		const (
			lineBytes = 64
			lines     = 24576         // 1.5 MiB, so a 1 MiB chunk is not clamped by the file size
			cpByte    = uint64(65536) // the only checkpoint the offset can seek back to
			cpLine    = uint32(cpByte/lineBytes + 1)
			offset    = uint64(700032) // a line start 634_496 bytes past that checkpoint
		)
		file := &fixtureFile{
			id: model.FileID(hexID(0x25)), path: "far.txt",
			data:        []byte(strings.Repeat(strings.Repeat("x", lineBytes-1)+"\n", lines)),
			checkpoints: []source.Checkpoint{{Byte: 0, Line: 1}, {Byte: cpByte, Line: cpLine}},
		}
		// Recommended, not required_full: the required_full ordinals are a
		// prefix of the manifest and appending a required file would break it.
		h.files[file.id] = file
		h.store.order = append(h.store.order, file.id)
		h.store.required[file.id] = model.RequirementRecommended
		h.store.sessions[sessionA].coverage[file.id] = &byteSet{served: make([]bool, len(file.data))}

		// Production limits rather than the fixture's: the checkpoint prefix
		// only binds when the request is allowed to ask for a whole megabyte.
		limits := fixtureLimits()
		limits.ChunkBytes, limits.MaxChunkBytes = 65536, model.MaxRawChunkBytes
		limits.MaxSourceResponseBytes = 7 << 20
		svc := &Service{
			sessions: h.store,
			open:     func(ctx context.Context, snap model.SnapshotID) (Source, error) { return h.src, nil },
			signer:   h.sign,
			limits:   limits,
			now:      func() time.Time { return h.now },
		}

		resp, err := svc.Read(context.Background(), model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: file.id,
			Offset: offset, MaxBytes: model.MaxRawChunkBytes,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}

		// The whole point: the span the view is asked for, measured from the
		// checkpoint and not from the offset, is exactly the read ceiling.
		wantEnd := cpByte + model.MaxRawChunkBytes
		issued := make([]*fakeChunk, 0, 1)
		for _, c := range h.store.chunks {
			if c.file == file.id {
				issued = append(issued, c)
			}
		}
		if len(issued) != 1 {
			t.Fatalf("read issued %d chunks for %s, want exactly 1", len(issued), file.id)
		}
		if issued[0].start != offset || issued[0].end != wantEnd {
			t.Fatalf("issued chunk is [%d,%d), want [%d,%d): the chunk cap did not subtract the %d-byte checkpoint prefix",
				issued[0].start, issued[0].end, offset, wantEnd, offset-cpByte)
		}
		if err == nil {
			if resp.ByteRange.Start != offset || resp.ByteRange.End != wantEnd {
				t.Fatalf("response range is [%d,%d), want [%d,%d)", resp.ByteRange.Start, resp.ByteRange.End, offset, wantEnd)
			}
			if resp.Encoding != model.EncodingUTF8 || resp.PartialLine {
				t.Fatalf("response is %s partial=%v, want %s on a line-aligned chunk", resp.Encoding, resp.PartialLine, model.EncodingUTF8)
			}
			if resp.Content != string(file.data[offset:wantEnd]) {
				t.Fatalf("response content is %d bytes and does not match the file", len(resp.Content))
			}
			if resp.NextOffset == nil || *resp.NextOffset != wantEnd {
				t.Fatalf("next offset is %v, want %d", resp.NextOffset, wantEnd)
			}
			if resp.LineRange.Start.Line != uint32(offset/lineBytes+1) || resp.LineRange.End.Line != uint32(wantEnd/lineBytes+1) {
				t.Fatalf("line range is %d..%d, want %d..%d: positions were not derived from the checkpoint",
					resp.LineRange.Start.Line, resp.LineRange.End.Line, offset/lineBytes+1, wantEnd/lineBytes+1)
			}
		}
		// Issuing grants nothing: only a confirmed receipt does.
		h.checkCoverage(sessionA, actorA, file.id, 0, model.CoverageUnserved)
	}},

	// L2 rows
	{
		// Protects the receipt codec's two bindings: the purpose discriminator,
		// which is the only thing stopping a cursor token from being spent as a
		// source receipt, and the chunk id the payload carries into the one
		// ConfirmChunks call. Breaking either grants full-read credit for bytes
		// no issued chunk was ever echoed for. It deliberately asserts nothing
		// about interval merging or confirm idempotency -- those are Task 5's
		// and are already tested there.
		name: "receipt purpose and chunk binding",
		run: func(t *testing.T, h *harness) {
			ctx := context.Background()
			// Built directly rather than through New: the row exercises the
			// receipt path, not the composition root.
			svc := &Service{
				sessions: h.store, signer: h.sign, limits: fixtureLimits(),
				now: func() time.Time { return h.now },
			}
			f := h.file(model.FileID(hexID(0x21)))
			rec, err := h.store.Session(ctx, sessionA, actorA)
			if err != nil {
				t.Fatalf("session: %v", err)
			}
			chunk := model.IssuedChunk{
				ID: hexID(0x50), SessionID: sessionA, ActorID: actorA,
				FileID: f.id, ContentHash: f.hash(),
				Bytes:     model.ByteRange{Start: 0, End: uint64(len(f.data))},
				ExpiresAt: h.now.Add(time.Hour),
			}
			if err := h.store.IssueChunk(ctx, chunk); err != nil {
				t.Fatalf("issue chunk: %v", err)
			}
			payload := receiptPayload{
				SessionID: sessionA, ActorID: actorA, SnapshotID: snapshotID,
				FileID: f.id, ContentHash: f.hash(),
				Start: chunk.Bytes.Start, End: chunk.Bytes.End, ChunkID: chunk.ID,
			}
			token, err := svc.encodeReceipt(payload, h.now.Add(time.Hour))
			if err != nil {
				t.Fatalf("encode receipt: %v", err)
			}

			// The same payload bytes signed for the cursor purpose. Only the
			// purpose differs, so nothing but the discriminator can reject it.
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			cursorToken, err := h.sign.Sign(pagination.PurposeCursor, raw, h.now.Add(time.Hour))
			if err != nil {
				t.Fatalf("sign cursor token: %v", err)
			}
			var typedErr *model.Error
			if err := svc.confirmReceipts(ctx, rec, []string{cursorToken}); !errors.As(err, &typedErr) ||
				typedErr.Code != model.CodeCursorInvalid {
				t.Fatalf("cursor-purpose token confirmed as a receipt: %v", err)
			}
			h.checkCoverage(sessionA, actorA, f.id, 0, model.CoverageUnserved)

			// The real receipt, echoed twice: the first grants exactly the
			// chunk's bytes -- the fixture file entire -- and the replay
			// neither fails nor adds more.
			for i := 0; i < 2; i++ {
				if err := svc.confirmReceipts(ctx, rec, []string{token}); err != nil {
					t.Fatalf("confirm %d: %v", i+1, err)
				}
				h.checkCoverage(sessionA, actorA, f.id, int64(len(f.data)), model.CoverageFullServed)
			}
		},
	},

	// L3 rows

	// Strict readiness is a stronger claim than full coverage and Task 16 never
	// makes it. The failure mode this guards is a status that folds
	// "every required file is served" into ready_for_implementation: Section
	// 16.3 additionally requires the verify phase, complete resolved scope, a
	// current-scope actor review, no blocking unresolved dependency and current
	// source validation, none of which this task evaluates. The row first
	// proves the premise -- every required_full file really is full_served --
	// so it cannot pass vacuously.
	{name: "status/full coverage still grants no strict readiness", run: func(t *testing.T, h *harness) {
		ctx := context.Background()
		var ids []string
		for i, fid := range h.store.order {
			if h.store.required[fid] != model.RequirementFull {
				continue
			}
			f := h.file(fid)
			id := hexID(byte(0x50 + i))
			// The empty file's chunk is the zero-length EOF chunk, which is the
			// only thing that can ever grant it full coverage.
			chunk := model.IssuedChunk{
				ID: id, SessionID: sessionA, ActorID: actorA, FileID: fid, ContentHash: f.hash(),
				Bytes:     model.ByteRange{Start: 0, End: uint64(len(f.data))},
				ExpiresAt: h.now.Add(time.Hour),
			}
			if err := h.store.IssueChunk(ctx, chunk); err != nil {
				t.Fatalf("issue chunk for %s: %v", f.path, err)
			}
			ids = append(ids, id)
		}
		if err := h.store.ConfirmChunks(ctx, sessionA, actorA, ids); err != nil {
			t.Fatalf("confirm %d chunks: %v", len(ids), err)
		}
		// Every required file was issued and confirmed over its whole extent,
		// so each carries exactly its fixture size in confirmed bytes -- the
		// empty file zero, credited by its confirmed zero-length EOF chunk.
		for _, fid := range h.store.order {
			if h.store.required[fid] == model.RequirementFull {
				h.checkCoverage(sessionA, actorA, fid, int64(len(h.file(fid).data)), model.CoverageFullServed)
			}
		}

		_, status, err := h.svc.Status(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA}, model.PageRequest{})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status.RequiredFiles != int64(len(ids)) || status.FullyServedFiles != status.RequiredFiles {
			t.Fatalf("premise failed: %d of %d required files are fully served, want all %d",
				status.FullyServedFiles, status.RequiredFiles, len(ids))
		}
		if !status.ReadCompleteForSnapshot {
			t.Fatalf("premise failed: read_complete_for_snapshot is false with every required file served")
		}
		if status.ReadyForImplementation || status.StrictGateSatisfied {
			t.Fatalf("full coverage granted strict readiness: ready_for_implementation=%v strict_gate_satisfied=%v",
				status.ReadyForImplementation, status.StrictGateSatisfied)
		}
	}},

	// L4 rows

	// Next must resume where the confirmed prefix ends and must stay metadata
	// only. A Next that answers zero sends the actor back over bytes it already
	// holds and, on a partially served file, can never terminate; a Next that
	// opens the Source has become a second read endpoint that serves bytes
	// without issuing a receipt, so coverage would be granted or bypassed
	// outside ConfirmChunks. The fixture's ordinal 0 is the empty file, whose
	// natural offset is zero, so the row first serves it and then confirms a
	// prefix of ordinal 1: the assertion is only meaningful once the answer is
	// a nonzero offset on the next required file.
	{name: "Next resumes at the confirmed prefix without opening the source", run: func(t *testing.T, h *harness) {
		ctx := context.Background()
		empty, crlf := model.FileID(hexID(0x20)), model.FileID(hexID(0x21))
		// Built directly rather than through newHarness so the opener can fail
		// the row if Next ever reaches for source, which is what gives "no
		// bytes" real teeth -- NextContextItem has no bytes or receipt field to
		// assert against.
		svc := &Service{
			sessions: h.store,
			open: func(context.Context, model.SnapshotID) (Source, error) {
				t.Fatalf("Next opened the source; it is metadata only and must never read bytes")
				return nil, nil
			},
			limits: fixtureLimits(),
			now:    func() time.Time { return h.now },
		}
		// Coverage is only ever created the way production creates it: an
		// issued chunk whose receipt is confirmed. Kept local to this row so no
		// other lane's rows depend on it.
		serve := func(file model.FileID, start, end uint64) {
			t.Helper()
			id, err := model.NewRandomID()
			if err != nil {
				t.Fatalf("chunk id: %v", err)
			}
			chunk := model.IssuedChunk{
				ID: id, SessionID: sessionA, ActorID: actorA, FileID: file,
				ContentHash: h.file(file).hash(), Bytes: model.ByteRange{Start: start, End: end},
				ExpiresAt: h.now.Add(time.Hour),
			}
			if err := h.store.IssueChunk(ctx, chunk); err != nil {
				t.Fatalf("issue chunk: %v", err)
			}
			if err := h.store.ConfirmChunks(ctx, sessionA, actorA, []string{id}); err != nil {
				t.Fatalf("confirm chunk: %v", err)
			}
		}
		// The empty file is fully served only by a confirmed zero-length EOF
		// chunk; the next required file gets a confirmed 7-byte prefix.
		serve(empty, 0, 0)
		serve(crlf, 0, 7)

		item, err := svc.Next(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if item.FileID != crlf {
			t.Fatalf("next names file %s; the first required file that is not full_served is %s", item.FileID, crlf)
		}
		if item.Offset != 7 {
			t.Fatalf("next resumes at %d; the confirmed prefix ends at 7", item.Offset)
		}
		want := h.file(crlf)
		if item.Action != actionReadSource || item.ContentHash != want.hash() || item.Size != int64(len(want.data)) {
			t.Fatalf("next answers action %q hash %s size %d; want %q/%s/%d",
				item.Action, item.ContentHash, item.Size, actionReadSource, want.hash(), len(want.data))
		}
		// Four required files, one of them now full_served.
		if item.Remaining != 3 {
			t.Fatalf("next reports %d required files remaining; three are not full_served", item.Remaining)
		}
	}},

	// L5 rows

	// L6 rows

	// INT rows

	// The whole session lifecycle through the composed service. Every other row
	// builds a Service literal for one endpoint, so nothing else proves that the
	// five endpoints agree once New has wired them together: that Acknowledge
	// really returns the status L3 builds rather than a zero value (the tail
	// L2's lane could not reach), that Status reports the same session metadata
	// Acknowledge just did, that Next advances to the file the confirmation did
	// not cover, and that Close is a compare-and-swap against the version Status
	// handed out. The readiness assertion is not L3's: L3 pins both flags false
	// at full coverage on a hand-built Service, while this pins them false
	// across the whole lifecycle, including the closed session -- the state a
	// caller is most likely to mistake for "done".
	{"int/a session reads, confirms, reports, advances and closes", func(t *testing.T, h *harness) {
		ctx := context.Background()
		empty := model.FileID(hexID(0x20))
		crlf := model.FileID(hexID(0x21))

		// 1. Read the empty required file. Its only possible coverage is the
		// zero-length EOF chunk, so the receipt is the whole transaction.
		resp, err := h.svc.Read(ctx, model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: empty,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if resp.Receipt == "" || resp.Coverage == model.CoverageFullServed {
			t.Fatalf("read of the empty file reports coverage %q with receipt %q; "+
				"an issued chunk is not coverage until it is confirmed", resp.Coverage, resp.Receipt)
		}

		// 2. Acknowledge it. The returned status is L3's, built from the record
		// the confirmation just changed.
		acked, err := h.svc.Acknowledge(ctx, model.AcknowledgeRequest{
			SessionID: sessionA, ActorID: actorA,
			Kind: model.AcknowledgeReceipt, Receipts: []string{resp.Receipt},
		})
		if err != nil {
			t.Fatalf("acknowledge: %v", err)
		}
		if acked.SessionID != sessionA || acked.RequiredFiles != 4 || acked.FullyServedFiles != 1 {
			t.Fatalf("acknowledge reports session %q with %d of %d required files served; "+
				"want %q with 1 of 4 -- the confirmed EOF receipt is the empty file's coverage",
				acked.SessionID, acked.FullyServedFiles, acked.RequiredFiles, sessionA)
		}
		// The empty file's only possible coverage: zero bytes, full_served on
		// the strength of the confirmed zero-length EOF receipt alone.
		h.checkCoverage(sessionA, actorA, empty, 0, model.CoverageFullServed)

		// 3. Status answers the same session, from the same record.
		page, status, err := h.svc.Status(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA},
			model.PageRequest{})
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if status.SessionID != acked.SessionID || status.ActorID != acked.ActorID ||
			status.Binding != acked.Binding || status.ManifestID != acked.ManifestID ||
			status.Phase != acked.Phase || status.State != acked.State ||
			status.StateVersion != acked.StateVersion ||
			status.RequiredFiles != acked.RequiredFiles ||
			status.FullyServedFiles != acked.FullyServedFiles {
			t.Fatalf("status describes %+v; acknowledge described %+v -- two endpoints "+
				"reporting one session must not disagree", status, acked)
		}
		if len(page.Items) == 0 {
			t.Fatalf("status returned no coverage records for a session with four required files")
		}

		// 4. Next advances past the file the confirmation covered.
		item, err := h.svc.Next(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if item.FileID != crlf || item.Action != actionReadSource || item.Offset != 0 || item.Remaining != 3 {
			t.Fatalf("next names %s (%s) at offset %d with %d remaining; want %s/%s/0/3 -- "+
				"the empty file is served and the next required entry is unread",
				item.FileID, item.Action, item.Offset, item.Remaining, crlf, actionReadSource)
		}

		// 5. Close is the compare-and-swap of ruling Q5 against the version
		// Status just reported, not a CloseSession of its own: a stale version
		// must lose rather than close a session that moved under the caller.
		var typedErr *model.Error
		if _, err := h.svc.Close(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA},
			status.StateVersion+1); !errors.As(err, &typedErr) || typedErr.Code != model.CodeVersionConflict {
			t.Fatalf("close at the wrong state version reports %v; want %s",
				err, model.CodeVersionConflict)
		}
		closed, err := h.svc.Close(ctx, model.SessionRequest{SessionID: sessionA, ActorID: actorA},
			status.StateVersion)
		if err != nil {
			t.Fatalf("close: %v", err)
		}
		if closed.State != model.StateClosed || closed.StateVersion != status.StateVersion+1 {
			t.Fatalf("close leaves the session %s at version %d; want %s at %d",
				closed.State, closed.StateVersion, model.StateClosed, status.StateVersion+1)
		}
		// Partial coverage never earns readiness, and neither does closing.
		for _, st := range []model.SessionStatus{acked, status, closed} {
			if st.ReadCompleteForSnapshot || st.ReadyForImplementation || st.StrictGateSatisfied {
				t.Fatalf("a session with 1 of 4 required files served reports read_complete=%t "+
					"ready_for_implementation=%t strict_gate_satisfied=%t; Task 16 grants none of them",
					st.ReadCompleteForSnapshot, st.ReadyForImplementation, st.StrictGateSatisfied)
			}
		}
	}},

	// An incomplete manifest scope is not a completed read. Ruling Q7 compiles
	// an ambiguous or empty scope into a manifest with ScopeComplete=false and
	// no required_full entries, so the required_full walk exhausts at once and
	// the count test "0 of 0 served" would otherwise answer actionComplete --
	// telling the actor the reading is done when the scope never resolved.
	// L4 could not exercise this branch: the fixture manifest it inherited
	// reported ScopeComplete unconditionally.
	{"int/an unresolved scope asks for review, not completion", func(t *testing.T, h *harness) {
		h.store.scopeComplete = false
		for id := range h.store.required {
			h.store.required[id] = model.RequirementRecommended
		}

		item, err := h.svc.Next(context.Background(),
			model.SessionRequest{SessionID: sessionA, ActorID: actorA})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if item.Action != actionReviewScope || item.FileID != "" {
			t.Fatalf("next answers %q for file %q over a manifest that resolved nothing; want %q and no file",
				item.Action, item.FileID, actionReviewScope)
		}
	}},

	// FX-D16-B rows

	// Bytes that are not text reach the client through base64 and through
	// nothing else, so the encoder has to carry exactly the chunk's bytes.
	// Truncating, padding or re-slicing the body serves wrong source under a
	// response that still validates and still reports a byte range the client
	// will credit: silently wrong source, which is the failure class this
	// package exists to prevent.
	{"read/invalid UTF-8 comes back as lossless base64", func(t *testing.T, h *harness) {
		f := h.file(model.FileID(hexID(0x23)))
		resp, err := h.svc.Read(context.Background(), model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: f.id,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if resp.Encoding != model.EncodingBase64 {
			t.Fatalf("read of %s reports encoding %q; bytes that are not valid UTF-8 must travel as %q",
				f.path, resp.Encoding, model.EncodingBase64)
		}
		got, err := base64.StdEncoding.DecodeString(resp.Content)
		if err != nil {
			t.Fatalf("the response body is not decodable base64: %v", err)
		}
		if !bytes.Equal(got, f.data) {
			t.Fatalf("the decoded body is %x over range [%d,%d); %s holds %x",
				got, resp.ByteRange.Start, resp.ByteRange.End, f.path, f.data)
		}
	}},

	// A line longer than the chunk budget must be cut and must SAY it was cut.
	// A response that drops partial_line tells the client it holds a complete
	// line, so a client assembling lines corrupts the one it is reading; a
	// response with no next offset stalls the read loop on a file it can then
	// never finish.
	{"read/a line longer than the budget splits and says so", func(t *testing.T, h *harness) {
		f := h.file(model.FileID(hexID(0x22)))
		resp, err := h.svc.Read(context.Background(), model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: f.id,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !resp.PartialLine {
			t.Fatalf("read of the %d-byte single line %s covers [%d,%d) and reports partial_line=false; "+
				"the line was split and the client is not told",
				len(f.data), f.path, resp.ByteRange.Start, resp.ByteRange.End)
		}
		if resp.NextOffset == nil || *resp.NextOffset == 0 || *resp.NextOffset != resp.ByteRange.End {
			t.Fatalf("read of %s reports next offset %v over [%d,%d); a split must resume at the byte it stopped on",
				f.path, resp.NextOffset, resp.ByteRange.Start, resp.ByteRange.End)
		}
	}},

	// The chunk the client is handed is a slice of a window that starts at the
	// checkpoint, not at the requested offset, so every chunk after the first
	// is wrong by the prefix unless that offset is subtracted. Reading a whole
	// file back in budget-sized steps is the only assertion that sees it: each
	// individual response validates, reports a plausible range and carries the
	// right number of bytes. CRLF pairs and a final line with no trailing
	// newline are the two shapes a boundary rule is most likely to normalise
	// away, so they are what the fixture holds.
	{"read/a file reassembles byte for byte across chunk boundaries", func(t *testing.T, h *harness) {
		ctx := context.Background()
		f := h.file(model.FileID(hexID(0x21)))
		// Seven bytes: three chunks over the 18-byte fixture, with the second
		// boundary falling inside a line that ends in CRLF.
		limits := fixtureLimits()
		limits.ChunkBytes = 7
		svc := &Service{
			sessions: h.store,
			open:     func(context.Context, model.SnapshotID) (Source, error) { return h.src, nil },
			signer:   h.sign, limits: limits,
			now: func() time.Time { return h.now },
		}
		var got []byte
		for offset, reads := uint64(0), 0; ; reads++ {
			if reads > len(f.data) {
				t.Fatalf("reading %s in %d-byte chunks made no progress", f.path, limits.ChunkBytes)
			}
			resp, err := svc.Read(ctx, model.ReadChunkRequest{
				SessionID: sessionA, ActorID: actorA, FileID: f.id, Offset: offset,
			})
			if err != nil {
				t.Fatalf("read at %d: %v", offset, err)
			}
			if resp.Encoding != model.EncodingUTF8 {
				t.Fatalf("read of the ASCII file %s at %d reports encoding %q", f.path, offset, resp.Encoding)
			}
			got = append(got, resp.Content...)
			if resp.NextOffset == nil {
				break
			}
			offset = *resp.NextOffset
		}
		if !bytes.Equal(got, f.data) {
			t.Fatalf("%s reassembles to %q; the pinned file holds %q", f.path, got, f.data)
		}
	}},

	// A read offset inside a UTF-8 sequence has no honest answer: the bytes
	// from there are not a decodable prefix of anything, and serving them as
	// base64 instead would hand the client a chunk it cannot place in the
	// text. The request is refused so the client re-reads from a boundary.
	//
	// Where the teeth are: the rejection itself is Task 4's, raised twice in
	// internal/source (PlanChunk's window[0] guard and Cursor.PositionAt's),
	// and no mutation of internal/coverage makes this row red -- deleting
	// either source guard alone leaves the other one catching it. What the row
	// pins here is that Read PROPAGATES the refusal rather than rounding the
	// offset down to a boundary or falling back to base64, which is the shape
	// a future "be lenient about offsets" change would take.
	{"read/an offset inside a UTF-8 sequence is refused", func(t *testing.T, h *harness) {
		f := h.file(model.FileID(hexID(0x24)))
		// The file opens with "é": byte 1 is its continuation byte, and the
		// checkpoint before it is byte 0, so the window still starts on a
		// boundary and the offset is the only thing wrong with the request.
		var typedErr *model.Error
		_, err := h.svc.Read(context.Background(), model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: f.id, Offset: 1,
		})
		if !errors.As(err, &typedErr) || typedErr.Code != model.CodeArgumentInvalid {
			t.Fatalf("read of %s at the continuation byte 1 returned %v; want %s",
				f.path, err, model.CodeArgumentInvalid)
		}
	}},

	// A receipt binds one session, one actor and one snapshot, and Section
	// 16.3 forbids sharing it across any of them. The service cross-checks the
	// payload against the live session record BEFORE storage is asked, which
	// is the part a returned error code alone cannot prove: ConfirmChunks
	// refuses a foreign chunk too, so dropping the cross-check would still
	// look like a rejection while every receipt in the process became a
	// storage probe for other actors' chunk ids.
	{"acknowledge/another session's receipt is refused before storage is asked", func(t *testing.T, h *harness) {
		ctx := context.Background()
		f := h.file(model.FileID(hexID(0x21)))
		resp, err := h.svc.Read(ctx, model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: f.id,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		before := h.store.confirmCalls
		var typedErr *model.Error
		if _, err := h.svc.Acknowledge(ctx, model.AcknowledgeRequest{
			SessionID: sessionB, ActorID: actorB,
			Kind: model.AcknowledgeReceipt, Receipts: []string{resp.Receipt},
		}); !errors.As(err, &typedErr) || typedErr.Code != model.CodeCursorInvalid {
			t.Fatalf("session %s / actor %s spent a receipt issued to %s / %s: %v",
				sessionB, actorB, sessionA, actorA, err)
		}
		if h.store.confirmCalls != before {
			t.Fatalf("the foreign receipt reached ConfirmChunks (%d calls, was %d); "+
				"the session, actor and snapshot it names are checked against the live record first",
				h.store.confirmCalls, before)
		}
		h.checkCoverage(sessionB, actorB, f.id, 0, model.CoverageUnserved)
	}},

	// A file acknowledgment asserts a human review of a file that is ALREADY
	// fully served; it creates no coverage of its own. Skipping the store's
	// refusal -- or never reaching the store at all -- turns `context
	// acknowledge --file-review` into a silent no-op that reports success, so
	// an actor marks a file reviewed having read seven of its bytes.
	{"acknowledge/a file review is refused while the file is only partly served", func(t *testing.T, h *harness) {
		ctx := context.Background()
		f := h.file(model.FileID(hexID(0x21)))
		limits := fixtureLimits()
		limits.ChunkBytes = 7
		svc := &Service{
			sessions: h.store,
			open:     func(context.Context, model.SnapshotID) (Source, error) { return h.src, nil },
			signer:   h.sign, limits: limits,
			now: func() time.Time { return h.now },
		}
		resp, err := svc.Read(ctx, model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: f.id,
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if _, err := svc.Acknowledge(ctx, model.AcknowledgeRequest{
			SessionID: sessionA, ActorID: actorA,
			Kind: model.AcknowledgeReceipt, Receipts: []string{resp.Receipt},
		}); err != nil {
			t.Fatalf("acknowledge the receipt: %v", err)
		}
		h.checkCoverage(sessionA, actorA, f.id, 7, model.CoveragePartialServed)

		var typedErr *model.Error
		if _, err := svc.Acknowledge(ctx, model.AcknowledgeRequest{
			SessionID: sessionA, ActorID: actorA,
			Kind: model.AcknowledgeFile, FileID: f.id,
		}); !errors.As(err, &typedErr) || typedErr.Code != model.CodeCoverageIncomplete {
			t.Fatalf("a %q acknowledgment over 7 of the %d bytes of %s returned %v; want %s",
				model.AcknowledgeFile, len(f.data), f.path, err, model.CodeCoverageIncomplete)
		}
	}},

	// needs FX-D16-A: F2 puts context.WithTimeout(ctx, QueryTimeout) atop
	// Read, as OpenSession and Status already do. Read is the one endpoint
	// that touches the CAS and the filesystem, and `--timeout` defaults to
	// zero precisely so resources.query_timeout applies, so an unbounded Read
	// is a request with no finite bound at all.
	{"read/a store that blocks is cut off by the query timeout", func(t *testing.T, h *harness) {
		t.Skip("FX-D16-A pending")
		limits := fixtureLimits()
		limits.QueryTimeout = 50 * time.Millisecond
		svc := &Service{
			sessions: blockingSessions{h.store},
			open:     func(context.Context, model.SnapshotID) (Source, error) { return h.src, nil },
			signer:   h.sign, limits: limits,
			now: func() time.Time { return h.now },
		}
		started := time.Now()
		_, err := svc.Read(context.Background(), model.ReadChunkRequest{
			SessionID: sessionA, ActorID: actorA, FileID: model.FileID(hexID(0x21)),
		})
		if err == nil || errors.Is(err, errStoreNeverReturned) {
			t.Fatalf("read of a blocked store returned %v after %s; the %s query timeout must cut it off",
				err, time.Since(started), limits.QueryTimeout)
		}
		// The invariant is "bounded, not hung", and the escape hatch above is
		// what enforces it exactly. This only keeps the row from passing on an
		// unrelated failure, so it admits every spelling the deadline may
		// reach a caller in: FX-D16-A may return ctx.Err() raw, as the landed
		// timeout sites do, or wrap it in a typed error.
		var typedErr *model.Error
		bounded := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
			(errors.As(err, &typedErr) && (typedErr.Code == model.CodeQueryDeadline ||
				typedErr.Code == model.CodeCanceled || typedErr.Code == model.CodeResourceLimit))
		if !bounded {
			t.Fatalf("read of a blocked store failed with %v after %s; want a deadline, not an unrelated error",
				err, time.Since(started))
		}
	}},

	// needs FX-D16-A: F4 assigns NextContextItem.Path from the widened
	// Sessions. `context next` names the file the actor must read, and a
	// response carrying only a 64-hex file id names a file the operator
	// cannot open.
	{"next/names the path of the file it selects", func(t *testing.T, h *harness) {
		t.Skip("FX-D16-A pending")
		item, err := h.svc.Next(context.Background(),
			model.SessionRequest{SessionID: sessionA, ActorID: actorA})
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if want := h.file(item.FileID); item.Path != want.path {
			t.Fatalf("next names file %s with path %q; the fixture declares it at %q",
				item.FileID, item.Path, want.path)
		}
	}},
}

func TestCoverage(t *testing.T) {
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newHarness(t)) })
	}
}
