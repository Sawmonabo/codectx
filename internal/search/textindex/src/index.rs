//! One open index directory: its writer (opened lazily, released after each
//! activation), its reader, the summaries of its segments and the cache of
//! per-generation alive sets.

use std::collections::HashMap;
use std::ops::Bound;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};

use tantivy::collector::Count;
use tantivy::index::SegmentId;
use tantivy::indexer::LogMergePolicy;
use tantivy::query::{RangeQuery, TermQuery};
use tantivy::schema::IndexRecordOption;
use tantivy::tokenizer::TextAnalyzer;
use tantivy::indexer::IndexWriterOptions;
use tantivy::{Executor, IndexReader, IndexWriter, ReloadPolicy, TantivyDocument, Term};

use crate::codec::Reader;
use crate::error::{Error, Result, CX_INVALID};
use crate::schema::{self, Fields};
use crate::summary::{self, Summary};
use crate::tokenizer::{self, Code, CODE_TOKENIZER};
use crate::view::AliveCache;

/// The library's smallest writer budget per thread (its writer refuses less:
/// 15 times its 1 MB arena margin).
pub const WRITER_MEMORY_FLOOR_PER_THREAD: u64 = 15_000_000;
/// The largest per-thread budget the library's writer accepts (it refuses
/// u32::MAX less its 1 MB margin and above).
const WRITER_MEMORY_CEILING_PER_THREAD: u64 = u32::MAX as u64 - 1_000_000 - 1;

/// The open options of textindex.h.
#[derive(Clone, Copy, Debug)]
pub struct Options {
    pub writable: bool,
    pub writer_threads: u32,
    pub writer_memory_bytes: u64,
    pub merge_threads: u32,
    pub search_threads: u32,
    pub view_cache_bytes: u64,
}

pub struct Index {
    pub dir: PathBuf,
    pub index: tantivy::Index,
    pub reader: IndexReader,
    pub fields: Fields,
    pub executor: Executor,
    opts: Options,
    writer: Mutex<Option<IndexWriter<TantivyDocument>>>,
    summaries: Mutex<HashMap<SegmentId, Arc<Summary>>>,
    pub alive: Mutex<AliveCache>,
}

/// Locks a mutex; a poisoned one (a panic while held, caught at the export)
/// still guards consistent state, because every holder either completes or
/// leaves the writer closed.
pub fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

impl Index {
    /// How many segments one search scores at once.
    pub fn search_threads(&self) -> u64 {
        u64::from(self.opts.search_threads.max(1))
    }

    pub fn open(dir: &Path, opts: Options) -> Result<Index> {
        let exists = dir.join("meta.json").exists();
        let index = if exists {
            // A directory whose schema lacks one of our fields is damaged:
            // Fields::resolve below refuses it as CX_CORRUPT.
            tantivy::Index::open_in_dir(dir)?
        } else if opts.writable {
            std::fs::create_dir_all(dir)?;
            tantivy::Index::create_in_dir(dir, schema::build())?
        } else {
            return Err(Error::new(CX_INVALID, format!("the search index {} does not exist", dir.display())));
        };
        index.tokenizers().register(CODE_TOKENIZER, TextAnalyzer::from(Code));
        let fields = Fields::resolve(&index.schema())?;
        let reader = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?;
        let executor = if opts.search_threads > 1 {
            Executor::multi_thread(opts.search_threads as usize, "cx-search-")?
        } else {
            Executor::single_thread()
        };
        Ok(Index {
            dir: dir.to_path_buf(),
            index,
            reader,
            fields,
            executor,
            opts,
            writer: Mutex::new(None),
            summaries: Mutex::new(HashMap::new()),
            alive: Mutex::new(AliveCache::new(opts.view_cache_bytes)),
        })
    }

    /// Runs f on the writer, opening it first when it is not open.
    fn with_writer<R>(&self, f: impl FnOnce(&mut IndexWriter<TantivyDocument>) -> Result<R>) -> Result<R> {
        if !self.opts.writable {
            return Err(Error::invalid("the index was opened read-only"));
        }
        let mut guard = lock(&self.writer);
        if guard.is_none() {
            *guard = Some(self.open_writer()?);
        }
        f(guard.as_mut().expect("writer opened"))
    }

    fn open_writer(&self) -> Result<IndexWriter<TantivyDocument>> {
        let threads = u64::from(self.opts.writer_threads.max(1));
        let per_thread = (self.opts.writer_memory_bytes / threads).min(WRITER_MEMORY_CEILING_PER_THREAD);
        if per_thread < WRITER_MEMORY_FLOOR_PER_THREAD {
            return Err(Error::invalid(format!(
                "the writer budget of {} bytes is below the library's floor of {} bytes for {threads} threads",
                self.opts.writer_memory_bytes,
                WRITER_MEMORY_FLOOR_PER_THREAD * threads
            )));
        }
        let options = IndexWriterOptions::builder()
            .num_worker_threads(threads as usize)
            .memory_budget_per_thread(per_thread as usize)
            .num_merge_threads(self.opts.merge_threads.max(1) as usize)
            .build();
        let writer = self.index.writer_with_options(options)?;
        writer.set_merge_policy(Box::new(LogMergePolicy::default()));
        Ok(writer)
    }

    /// Adds one unit's documents (the cx_index_add layout).
    pub fn add(&self, buf: &[u8]) -> Result<()> {
        let mut r = Reader::new(buf);
        let unit = r.str()?;
        let count = r.uvarint()?;
        let f = self.fields;
        let mut docs = Vec::new();
        for _ in 0..count {
            let mut d = TantivyDocument::default();
            d.add_u64(f.doc_id, r.uvarint()?);
            let search_key = r.str()?;
            let node_id = r.str()?;
            let file_id = r.str()?;
            let path = r.str()?;
            let kind = r.str()?;
            let name = r.str()?;
            let qualified_name = r.str()?;
            let signature = r.str()?;
            let start = r.uvarint()?;
            let end = r.uvarint()?;
            let body = r.str()?;
            let columns = [name, qualified_name, signature, path, body];
            for (i, c) in columns.iter().enumerate() {
                d.add_text(f.text[i], c);
                d.add_u64(f.lens[i], tokenizer::code_token_count(c));
            }
            for (i, v) in [path, kind, name, qualified_name, signature, node_id, search_key, file_id].iter().enumerate() {
                d.add_text(f.strs[i], v);
            }
            d.add_text(f.unit, unit);
            d.add_u64(f.start, start);
            d.add_u64(f.end, end);
            docs.push(d);
        }
        r.finish()?;
        self.with_writer(|w| {
            for d in docs {
                w.add_document(d)?;
            }
            Ok(())
        })
    }

    pub fn delete_docs(&self, ids: &[u64]) -> Result<()> {
        let f = self.fields.doc_id;
        self.with_writer(|w| {
            for &id in ids {
                w.delete_term(Term::from_field_u64(f, id));
            }
            Ok(())
        })
    }

    pub fn delete_from(&self, first: u64) -> Result<()> {
        let f = self.fields.doc_id;
        self.with_writer(|w| {
            let q = RangeQuery::new(Bound::Included(Term::from_field_u64(f, first)), Bound::Unbounded);
            w.delete_query(Box::new(q))?;
            Ok(())
        })
    }

    pub fn delete_unit(&self, unit: &str) -> Result<()> {
        let f = self.fields.unit;
        self.with_writer(|w| {
            w.delete_term(Term::from_field_text(f, unit));
            Ok(())
        })
    }

    pub fn commit(&self, payload: &[u8]) -> Result<()> {
        let payload = std::str::from_utf8(payload).map_err(|_| Error::invalid("a commit payload is not UTF-8"))?;
        self.with_writer(|w| {
            let mut prepared = w.prepare_commit()?;
            prepared.set_payload(payload);
            prepared.commit()?;
            Ok(())
        })?;
        self.reader.reload()?;
        self.summarize_missing()
    }

    /// Waits for running merges, summarizes what they wrote, removes orphaned
    /// summaries and closes the writer. A writer that is not open is not an error.
    pub fn release_writer(&self) -> Result<()> {
        let mut guard = lock(&self.writer);
        let Some(w) = guard.take() else { return Ok(()) };
        w.wait_merging_threads()?;
        drop(guard);
        self.reader.reload()?;
        self.summarize_missing()?;
        self.remove_orphans()
    }

    /// Merges every searchable segment into one and summarizes it. Running
    /// merges are waited for first by closing the writer, which discards
    /// changes not yet committed: callers commit before merging.
    pub fn merge_all(&self) -> Result<()> {
        if !self.opts.writable {
            return Err(Error::invalid("the index was opened read-only"));
        }
        let mut guard = lock(&self.writer);
        if let Some(w) = guard.take() {
            w.wait_merging_threads()?;
        }
        let mut w = self.open_writer()?;
        let ids = self.index.searchable_segment_ids()?;
        let deletes = self.index.searchable_segment_metas()?.iter().any(|m| m.has_deletes());
        if ids.len() > 1 || (ids.len() == 1 && deletes) {
            w.merge(&ids).wait()?;
        }
        *guard = Some(w);
        drop(guard);
        self.reader.reload()?;
        self.summarize_missing()?;
        self.remove_orphans()
    }

    /// Closes the writer after its merges and summarizes what they wrote.
    pub fn close(&self) -> Result<()> {
        if lock(&self.writer).is_none() {
            return Ok(());
        }
        self.release_writer()
    }

    /// The payload of the last commit on disk, empty when there is none.
    pub fn committed(&self) -> Result<String> {
        let metas = self.index.load_metas()?;
        Ok(metas.payload.unwrap_or_default())
    }

    /// The committed, not deleted documents a unit added.
    pub fn unit_docs(&self, unit: &str) -> Result<u64> {
        self.reader.reload()?;
        let searcher = self.reader.searcher();
        let q = TermQuery::new(Term::from_field_text(self.fields.unit, unit), IndexRecordOption::Basic);
        Ok(searcher.search(&q, &Count)? as u64)
    }

    /// The summary of a segment: cached, else loaded from its file. None when
    /// the segment has no current summary.
    pub fn summary(&self, seg: &tantivy::SegmentReader) -> Option<Arc<Summary>> {
        if let Some(s) = lock(&self.summaries).get(&seg.segment_id()) {
            return Some(s.clone());
        }
        let s = Arc::new(Summary::load(&self.dir, seg)?);
        lock(&self.summaries).insert(seg.segment_id(), s.clone());
        Some(s)
    }

    /// Builds the summary of every searchable segment that has none.
    fn summarize_missing(&self) -> Result<()> {
        let searcher = self.reader.searcher();
        for seg in searcher.segment_readers() {
            if self.summary(seg).is_some() {
                continue;
            }
            summary::build(&self.dir, seg, &self.fields)?;
            if self.summary(seg).is_none() {
                return Err(Error::internal(format!(
                    "the block summary just written for segment {} does not read back",
                    seg.segment_id().uuid_string()
                )));
            }
        }
        Ok(())
    }

    fn remove_orphans(&self) -> Result<()> {
        let live = self.index.searchable_segment_ids()?;
        lock(&self.summaries).retain(|id, _| live.contains(id));
        summary::remove_orphans(&self.dir, &live);
        Ok(())
    }
}
