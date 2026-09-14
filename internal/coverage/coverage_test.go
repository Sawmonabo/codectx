package coverage

import (
	"context"
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
		EntryCount: len(s.order), ScopeComplete: true,
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
		chunks: map[string]*fakeChunk{},
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
	// New is a stub until the fill-in lanes land; a row that needs the service
	// fails here honestly rather than dereferencing nil.
	if err != nil {
		h.svc = nil
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

// oracle is the independent expectation for one session and file: the byte-set
// plus the separate EOF bit, recomputed from confirmed chunks alone. A row
// compares the reported CoverageState against this, never against the service's
// own bookkeeping.
func (h *harness) oracle(session model.SessionID, id model.FileID) *byteSet {
	h.t.Helper()
	want := &byteSet{served: make([]bool, len(h.file(id).data))}
	for _, c := range h.store.chunks {
		if c.confirmed && c.session == session && c.file == id {
			want.confirm(c.start, c.end)
		}
	}
	return want
}

// checkOracle asserts the store's reported state for one file equals the
// oracle's. Failure means credit was granted for bytes no confirmed receipt
// covers, or withheld for bytes one does.
func (h *harness) checkOracle(session model.SessionID, actor string, id model.FileID) {
	h.t.Helper()
	got, err := h.store.Coverage(context.Background(), session, actor, "", 0)
	if err != nil {
		h.t.Fatalf("coverage: %v", err)
	}
	want := h.oracle(session, id)
	for _, fc := range got {
		if fc.FileID != id {
			continue
		}
		if fc.State != want.state() || fc.ConfirmedBytes != want.confirmedBytes() {
			h.t.Fatalf("file %s: store reports %s/%d bytes, oracle says %s/%d bytes",
				id, fc.State, fc.ConfirmedBytes, want.state(), want.confirmedBytes())
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

	// L2 rows

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
		for _, fid := range h.store.order {
			if h.store.required[fid] == model.RequirementFull {
				h.checkOracle(sessionA, actorA, fid)
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

	// L5 rows

	// L6 rows
}

func TestCoverage(t *testing.T) {
	// Built once here as well as per row: while the table is still filling up
	// this keeps the fixture, the signer and the oracle genuinely exercised, so
	// the first lane to add a row does not discover a broken harness.
	newHarness(t)
	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, newHarness(t)) })
	}
}
