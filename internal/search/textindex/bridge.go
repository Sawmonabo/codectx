package textindex

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo linux LDFLAGS: ${SRCDIR}/_build/release/libcodectx_textindex.a -lm -ldl -lpthread
#cgo darwin LDFLAGS: ${SRCDIR}/_build/release/libcodectx_textindex.a -lm -framework CoreFoundation -framework Security
#cgo windows LDFLAGS: ${SRCDIR}/_build/release/libcodectx_textindex.a -lws2_32 -luserenv -lbcrypt -lntdll
#include <stdlib.h>
#include "textindex.h"
*/
import "C"

import (
	"context"
	"strconv"
	"sync"
	"unsafe"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Index is one open index directory. Safe for concurrent use: the library
// serializes its writer, and searches run concurrently up to QuerySlots.
type Index struct {
	ptr      *C.cx_index
	writable bool
	opts     Options
	slots    chan struct{}

	// life guards the handle: every call holds it shared, Close holds it
	// exclusively, so no call can run on a freed handle.
	life   sync.RWMutex
	closed bool

	// mu guards the writer's admission: whether a writer is open, and the
	// release of the memory it was admitted with. It is taken inside life.
	mu      sync.Mutex
	writing bool
	release func()
}

// WriterMemoryFloor is the smallest writer budget the library accepts for
// threads threads.
func WriterMemoryFloor(threads int) int64 {
	return int64(C.cx_writer_memory_floor(C.uint32_t(max(threads, 1))))
}

// Open opens the index directory.
func Open(o Options) (*Index, error) {
	if o.Dir == "" {
		return nil, internalErr("open: no index directory")
	}
	if o.Writable && o.WriterMemoryBytes < WriterMemoryFloor(o.WriterThreads) {
		return nil, internalErr("open: the writer budget is below the library's floor for " +
			strconv.Itoa(max(o.WriterThreads, 1)) + " threads")
	}
	opts := C.cx_open_options{
		writable:            boolByte(o.Writable),
		writer_threads:      C.uint32_t(max(o.WriterThreads, 1)),
		writer_memory_bytes: C.uint64_t(max(o.WriterMemoryBytes, 0)),
		merge_threads:       C.uint32_t(max(o.MergeThreads, 1)),
		search_threads:      C.uint32_t(max(o.SearchThreads, 1)),
		view_cache_bytes:    C.uint64_t(max(o.ViewCacheBytes, 0)),
	}
	dir := []byte(o.Dir)
	var out *C.cx_index
	var errBuf C.cx_buf
	status := C.cx_index_open(bytesPtr(dir), C.size_t(len(dir)), &opts, &out, &errBuf)
	if err := statusErr("open", status, &errBuf); err != nil {
		return nil, err
	}
	return &Index{ptr: out, writable: o.Writable, opts: o, slots: make(chan struct{}, max(o.QuerySlots, 1))}, nil
}

// Close waits for running merges, closes the writer and the index.
func (x *Index) Close() error {
	x.life.Lock()
	defer x.life.Unlock()
	if x.closed {
		return nil
	}
	x.closed = true
	var errBuf C.cx_buf
	status := C.cx_index_close(x.ptr, &errBuf)
	x.ptr = nil
	x.mu.Lock()
	x.endWriting()
	x.mu.Unlock()
	return statusErr("close", status, &errBuf)
}

// use holds the handle open for one call; done releases it.
func (x *Index) use() (done func(), err error) {
	x.life.RLock()
	if x.closed {
		x.life.RUnlock()
		return nil, internalErr("the index is closed")
	}
	return x.life.RUnlock, nil
}

// beginWriting admits the writer's memory before the first mutating call opens
// it. The caller holds mu.
func (x *Index) beginWriting(ctx context.Context) error {
	if !x.writable {
		return internalErr("the index was opened read-only")
	}
	if x.writing {
		return nil
	}
	if x.opts.Reserve != nil {
		release, err := x.opts.Reserve(ctx, x.opts.WriterMemoryBytes)
		if err != nil {
			return err
		}
		x.release = release
	}
	x.writing = true
	return nil
}

// endWriting returns the writer's admission. The caller holds mu.
func (x *Index) endWriting() {
	if x.release != nil {
		x.release()
		x.release = nil
	}
	x.writing = false
}

// AddUnit adds one unit's documents. They become searchable at the next Commit.
func (x *Index) AddUnit(ctx context.Context, unitKey string, docs []Document) error {
	if len(docs) == 0 {
		return nil
	}
	buf := encodeUnit(unitKey, docs)
	return x.mutate(ctx, "add", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_add(x.ptr, bytesPtr(buf), C.size_t(len(buf)), errBuf)
	})
}

// DeleteDocs deletes documents by doc id, at the next Commit.
func (x *Index) DeleteDocs(ctx context.Context, ids []uint64) error {
	if len(ids) == 0 {
		return nil
	}
	return x.mutate(ctx, "delete", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_delete_docs(x.ptr, (*C.uint64_t)(unsafe.Pointer(&ids[0])), C.size_t(len(ids)), errBuf)
	})
}

// DeleteDocsFrom deletes every document whose doc id is at least first, at the
// next Commit.
func (x *Index) DeleteDocsFrom(ctx context.Context, first uint64) error {
	return x.mutate(ctx, "delete from", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_delete_from(x.ptr, C.uint64_t(first), errBuf)
	})
}

// DeleteUnit deletes every document a unit added, at the next Commit.
func (x *Index) DeleteUnit(ctx context.Context, unitKey string) error {
	key := []byte(unitKey)
	return x.mutate(ctx, "delete unit", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_delete_unit(x.ptr, bytesPtr(key), C.size_t(len(key)), errBuf)
	})
}

// Commit makes every pending change durable under payload and builds the
// block summaries of the segments it wrote.
func (x *Index) Commit(ctx context.Context, payload string) error {
	p := []byte(payload)
	return x.mutate(ctx, "commit", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_commit(x.ptr, bytesPtr(p), C.size_t(len(p)), errBuf)
	})
}

// ReleaseWriter waits for running merges, summarizes the segments they wrote
// and closes the writer, so another process may write. Changes not committed
// are discarded. A writer that is not open is not an error.
func (x *Index) ReleaseWriter() error {
	done, err := x.use()
	if err != nil {
		return err
	}
	defer done()
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.writing {
		return nil
	}
	var errBuf C.cx_buf
	status := C.cx_index_release_writer(x.ptr, &errBuf)
	x.endWriting()
	return statusErr("release writer", status, &errBuf)
}

// Writing reports whether this process holds an open writer, which may hold
// changes no commit has made durable yet.
func (x *Index) Writing() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.writing
}

// mergeAll merges every searchable segment into one and summarizes it.
func (x *Index) mergeAll(ctx context.Context) error {
	return x.mutate(ctx, "merge", func(errBuf *C.cx_buf) C.int32_t {
		return C.cx_index_merge_all(x.ptr, errBuf)
	})
}

func (x *Index) mutate(ctx context.Context, what string, call func(*C.cx_buf) C.int32_t) error {
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	done, err := x.use()
	if err != nil {
		return err
	}
	defer done()
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.beginWriting(ctx); err != nil {
		return err
	}
	var errBuf C.cx_buf
	return statusErr(what, call(&errBuf), &errBuf)
}

// Committed is the payload of the last commit; ok is false when nothing was
// ever committed.
func (x *Index) Committed() (payload string, ok bool, err error) {
	done, err := x.use()
	if err != nil {
		return "", false, err
	}
	defer done()
	var out, errBuf C.cx_buf
	if err := statusErr("committed", C.cx_index_committed(x.ptr, &out, &errBuf), &errBuf); err != nil {
		return "", false, err
	}
	b := takeBuf(&out)
	return string(b), len(b) > 0, nil
}

// UnitDocs counts the committed, not deleted documents a unit added.
func (x *Index) UnitDocs(unitKey string) (int64, error) {
	done, err := x.use()
	if err != nil {
		return 0, err
	}
	defer done()
	key := []byte(unitKey)
	var n C.uint64_t
	var errBuf C.cx_buf
	if err := statusErr("unit docs", C.cx_index_unit_docs(x.ptr, bytesPtr(key), C.size_t(len(key)), &n, &errBuf), &errBuf); err != nil {
		return 0, err
	}
	return int64(n), nil
}

// View is one generation's view of the latest commit. A view holds the
// library's searcher and stays valid after a later commit; it must be closed
// before its Index is.
type View struct {
	x   *Index
	ptr *C.cx_view
}

// OpenView opens generation's view. visible is a bitmap over doc ids: bit
// id%8 of byte id/8, least significant first.
func (x *Index) OpenView(generation model.GenerationID, visible []byte) (*View, error) {
	done, err := x.use()
	if err != nil {
		return nil, err
	}
	defer done()
	var out *C.cx_view
	var errBuf C.cx_buf
	status := C.cx_view_open(x.ptr, C.uint64_t(generation), bytesPtr(visible), C.size_t(len(visible)), &out, &errBuf)
	if err := statusErr("open view", status, &errBuf); err != nil {
		return nil, err
	}
	return &View{x: x, ptr: out}, nil
}

// Close releases the view. Safe to call more than once.
func (v *View) Close() {
	if v.ptr != nil {
		C.cx_view_close(v.ptr)
		v.ptr = nil
	}
}

// Search answers one page. The context's end raises the library's cancel
// flag: a search already running returns the best results found so far with
// Page.Canceled set, and one not started returns the context's error.
func (v *View) Search(ctx context.Context, r Request) (Page, error) {
	if r.Depth < 1 {
		return Page{}, &model.Error{Code: model.CodeArgumentInvalid, Message: "textindex: a search depth must be at least 1"}
	}
	select {
	case v.x.slots <- struct{}{}:
	case <-ctx.Done():
		return Page{}, model.Canceled(ctx.Err())
	}
	defer func() { <-v.x.slots }()
	release, err := v.x.use()
	if err != nil {
		return Page{}, err
	}
	defer release()

	flag := (*C.uint8_t)(C.malloc(1))
	defer C.free(unsafe.Pointer(flag))
	*flag = 0
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			*(*uint8)(unsafe.Pointer(flag)) = 1
		case <-done:
		}
	}()
	req := encodeRequest(r)
	var out, errBuf C.cx_buf
	status := C.cx_view_search(v.ptr, bytesPtr(req), C.size_t(len(req)), flag, &out, &errBuf)
	close(done)
	<-stopped
	if status == C.CX_CANCELED {
		freeBuf(&errBuf)
		return Page{}, model.Canceled(ctx.Err())
	}
	if status == C.CX_TOO_MANY_TERMS {
		n, _ := strconv.ParseInt(string(takeBuf(&errBuf)), 10, 64)
		return Page{}, &TooManyTerms{Terms: n}
	}
	if err := statusErr("search", status, &errBuf); err != nil {
		return Page{}, err
	}
	return decodePage(takeBuf(&out))
}

// scoreVector runs the library's scorer arithmetic over in.
func scoreVector(in []scoreInput) ([]scoreOutput, error) {
	buf := encodeScoreInputs(in)
	var out, errBuf C.cx_buf
	if err := statusErr("score vector", C.cx_score_vector(bytesPtr(buf), C.size_t(len(buf)), &out, &errBuf), &errBuf); err != nil {
		return nil, err
	}
	return decodeScoreOutputs(takeBuf(&out), len(in))
}

// panicSelfTest panics inside the library's export guard and returns what the
// guard reported.
func panicSelfTest() error {
	var errBuf C.cx_buf
	return statusErr("panic self-test", C.cx_panic_selftest(&errBuf), &errBuf)
}

// statusErr maps a library status to a typed error, consuming errBuf.
func statusErr(what string, status C.int32_t, errBuf *C.cx_buf) error {
	msg := string(takeBuf(errBuf))
	if status == C.CX_OK {
		return nil
	}
	text := "textindex: " + what + ": " + msg
	switch status {
	case C.CX_BUSY:
		return &model.Error{Code: model.CodeWorkspaceBusy, Message: text,
			Remediation: "another codectx process is writing this workspace's search index; retry when it finishes"}
	case C.CX_CORRUPT:
		return &model.Error{Code: model.CodeStorageCorrupt, Message: text,
			Remediation: "run `codectx reset --reindex` to build the workspace again from the repository"}
	case C.CX_DISK_FULL:
		return &model.Error{Code: model.CodeDiskFull, Message: text}
	case C.CX_CANCELED:
		return &model.Error{Code: model.CodeCanceled, Message: text}
	case C.CX_NO_POSITION:
		return ErrUnknownPosition
	default:
		return &model.Error{Code: model.CodeInternal, Message: text}
	}
}

func internalErr(msg string) error {
	return &model.Error{Code: model.CodeInternal, Message: "textindex: " + msg}
}

// takeBuf copies a library buffer into Go memory and frees it.
func takeBuf(b *C.cx_buf) []byte {
	if b.ptr == nil {
		return nil
	}
	out := C.GoBytes(unsafe.Pointer(b.ptr), C.int(b.len))
	C.cx_buf_free(b)
	return out
}

func freeBuf(b *C.cx_buf) {
	if b.ptr != nil {
		C.cx_buf_free(b)
	}
}

func bytesPtr(b []byte) *C.uint8_t {
	if len(b) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(&b[0]))
}

func boolByte(v bool) C.uint8_t {
	if v {
		return 1
	}
	return 0
}
