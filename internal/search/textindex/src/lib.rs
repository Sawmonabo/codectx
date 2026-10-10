//! The embedded search index of codectx's lexical tier
//! (docs/adr/ADR-0013-search-index.md), exported through the C boundary in
//! textindex.h. Every export runs inside one guard that catches a panic and
//! reports it as CX_INTERNAL, so nothing unwinds into the caller.

mod codec;
mod error;
mod index;
mod schema;
mod score;
mod search;
mod summary;
mod tables;
mod tokenizer;
mod view;

use std::panic::{catch_unwind, AssertUnwindSafe};
use std::path::Path;
use std::sync::Arc;

use error::{Error, Result, CX_INTERNAL, CX_OK};
use index::{Index, Options};
use view::View;

/// A byte buffer owned by the library (textindex.h `cx_buf`).
#[repr(C)]
pub struct CxBuf {
    ptr: *mut u8,
    len: usize,
    cap: usize,
}

impl CxBuf {
    fn set(out: *mut CxBuf, v: Vec<u8>) {
        if out.is_null() {
            return;
        }
        let mut v = std::mem::ManuallyDrop::new(v);
        // SAFETY: out is a caller-owned cx_buf; it takes ownership of v's allocation.
        unsafe {
            *out = CxBuf { ptr: v.as_mut_ptr(), len: v.len(), cap: v.capacity() };
        }
    }
}

/// textindex.h `cx_open_options`.
#[repr(C)]
pub struct CxOpenOptions {
    writable: u8,
    writer_threads: u32,
    writer_memory_bytes: u64,
    merge_threads: u32,
    search_threads: u32,
    view_cache_bytes: u64,
}

/// The guard around every export: a failure or a caught panic becomes a
/// status and a message in *err; success leaves *err untouched.
fn guard(err: *mut CxBuf, f: impl FnOnce() -> Result<()>) -> i32 {
    let e = match catch_unwind(AssertUnwindSafe(f)) {
        Ok(Ok(())) => return CX_OK,
        Ok(Err(e)) => e,
        Err(payload) => {
            let what = payload
                .downcast_ref::<&str>()
                .map(|s| s.to_string())
                .or_else(|| payload.downcast_ref::<String>().cloned())
                .unwrap_or_else(|| "a panic with no message".to_string());
            Error::new(CX_INTERNAL, format!("the search library panicked: {what}"))
        }
    };
    CxBuf::set(err, e.msg.into_bytes());
    e.status
}

/// A caller's (pointer, length) as a slice; (NULL, 0) is empty.
///
/// # Safety
/// A non-null ptr names len readable bytes for the call.
unsafe fn bytes<'a>(ptr: *const u8, len: usize) -> Result<&'a [u8]> {
    if len == 0 {
        return Ok(&[]);
    }
    if ptr.is_null() {
        return Err(Error::invalid("a null buffer with a nonzero length"));
    }
    Ok(std::slice::from_raw_parts(ptr, len))
}

/// # Safety
/// x is null or a handle cx_index_open returned and cx_index_close has not freed.
unsafe fn handle<'a>(x: *const Arc<Index>) -> Result<&'a Arc<Index>> {
    x.as_ref().ok_or_else(|| Error::invalid("a null index handle"))
}

fn utf8(b: &[u8]) -> Result<&str> {
    std::str::from_utf8(b).map_err(|_| Error::invalid("a string argument is not UTF-8"))
}

#[no_mangle]
pub extern "C" fn cx_writer_memory_floor(threads: u32) -> u64 {
    index::WRITER_MEMORY_FLOOR_PER_THREAD.saturating_mul(u64::from(threads.max(1)))
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_open(
    dir: *const u8,
    dir_len: usize,
    opts: *const CxOpenOptions,
    out: *mut *mut Arc<Index>,
    err: *mut CxBuf,
) -> i32 {
    guard(err, || {
        let dir = utf8(bytes(dir, dir_len)?)?;
        if dir.is_empty() {
            return Err(Error::invalid("no index directory"));
        }
        let o = opts.as_ref().ok_or_else(|| Error::invalid("null open options"))?;
        if out.is_null() {
            return Err(Error::invalid("a null output handle"));
        }
        let opts = Options {
            writable: o.writable != 0,
            writer_threads: o.writer_threads,
            writer_memory_bytes: o.writer_memory_bytes,
            merge_threads: o.merge_threads,
            search_threads: o.search_threads,
            view_cache_bytes: o.view_cache_bytes,
        };
        let x = Index::open(Path::new(dir), opts)?;
        *out = Box::into_raw(Box::new(Arc::new(x)));
        Ok(())
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_close(x: *mut Arc<Index>, err: *mut CxBuf) -> i32 {
    guard(err, || {
        if x.is_null() {
            return Ok(());
        }
        let x = Box::from_raw(x);
        x.close()
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_add(x: *mut Arc<Index>, buf: *const u8, len: usize, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.add(bytes(buf, len)?))
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_delete_docs(x: *mut Arc<Index>, ids: *const u64, n: usize, err: *mut CxBuf) -> i32 {
    guard(err, || {
        let ids: &[u64] = if n == 0 {
            &[]
        } else if ids.is_null() {
            return Err(Error::invalid("a null id list with a nonzero length"));
        } else {
            std::slice::from_raw_parts(ids, n)
        };
        handle(x)?.delete_docs(ids)
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_delete_from(x: *mut Arc<Index>, first: u64, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.delete_from(first))
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_delete_unit(x: *mut Arc<Index>, unit: *const u8, len: usize, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.delete_unit(utf8(bytes(unit, len)?)?))
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_commit(x: *mut Arc<Index>, payload: *const u8, len: usize, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.commit(bytes(payload, len)?))
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_release_writer(x: *mut Arc<Index>, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.release_writer())
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_committed(x: *mut Arc<Index>, payload: *mut CxBuf, err: *mut CxBuf) -> i32 {
    guard(err, || {
        let p = handle(x)?.committed()?;
        if !p.is_empty() {
            CxBuf::set(payload, p.into_bytes());
        }
        Ok(())
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_unit_docs(
    x: *mut Arc<Index>,
    unit: *const u8,
    len: usize,
    count: *mut u64,
    err: *mut CxBuf,
) -> i32 {
    guard(err, || {
        let n = handle(x)?.unit_docs(utf8(bytes(unit, len)?)?)?;
        if count.is_null() {
            return Err(Error::invalid("a null count"));
        }
        *count = n;
        Ok(())
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_index_merge_all(x: *mut Arc<Index>, err: *mut CxBuf) -> i32 {
    guard(err, || handle(x)?.merge_all())
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_view_open(
    x: *mut Arc<Index>,
    generation: u64,
    visible: *const u8,
    len: usize,
    out: *mut *mut View,
    err: *mut CxBuf,
) -> i32 {
    guard(err, || {
        let x = handle(x)?.clone();
        let visible = bytes(visible, len)?;
        if out.is_null() {
            return Err(Error::invalid("a null output view"));
        }
        let v = View::open(x, generation, visible)?;
        *out = Box::into_raw(Box::new(v));
        Ok(())
    })
}

/// # Safety
/// v is null or a view cx_view_open returned that was not closed.
#[no_mangle]
pub unsafe extern "C" fn cx_view_close(v: *mut View) {
    if v.is_null() {
        return;
    }
    let _ = catch_unwind(AssertUnwindSafe(|| drop(Box::from_raw(v))));
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_view_search(
    v: *mut View,
    req: *const u8,
    len: usize,
    cancel: *const u8,
    page: *mut CxBuf,
    err: *mut CxBuf,
) -> i32 {
    guard(err, || {
        let v = v.as_ref().ok_or_else(|| Error::invalid("a null view"))?;
        let out = search::search(v, bytes(req, len)?, search::Cancel(cancel))?;
        CxBuf::set(page, out);
        Ok(())
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_score_vector(input: *const u8, len: usize, out: *mut CxBuf, err: *mut CxBuf) -> i32 {
    guard(err, || {
        let b = bytes(input, len)?;
        if b.len() % 40 != 0 {
            return Err(Error::invalid("a score vector is not a whole number of 40-byte rows"));
        }
        let mut r = codec::Reader::new(b);
        let mut o = Vec::with_capacity(b.len() / 40 * 24);
        for _ in 0..b.len() / 40 {
            let docs = r.fixed64()?;
            let df = r.fixed64()?;
            let wtf = f64::from_bits(r.fixed64()?);
            let dl = r.fixed64()?;
            let avgdl = f64::from_bits(r.fixed64()?);
            let i = score::idf(docs, df);
            let s = i * score::saturation(wtf, dl, avgdl);
            o.extend_from_slice(&i.to_bits().to_le_bytes());
            o.extend_from_slice(&s.to_bits().to_le_bytes());
            o.extend_from_slice(&score::quantize(s).to_le_bytes());
        }
        CxBuf::set(out, o);
        Ok(())
    })
}

/// # Safety
/// See textindex.h.
#[no_mangle]
pub unsafe extern "C" fn cx_panic_selftest(err: *mut CxBuf) -> i32 {
    guard(err, || {
        let armed = std::hint::black_box(true);
        if armed {
            panic!("the boundary self-test panicked on purpose");
        }
        Ok(())
    })
}

/// # Safety
/// b is null or a cx_buf the library filled.
#[no_mangle]
pub unsafe extern "C" fn cx_buf_free(b: *mut CxBuf) {
    let Some(buf) = b.as_mut() else { return };
    if !buf.ptr.is_null() {
        drop(Vec::from_raw_parts(buf.ptr, buf.len, buf.cap));
    }
    *buf = CxBuf { ptr: std::ptr::null_mut(), len: 0, cap: 0 };
}
