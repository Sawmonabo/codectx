//! A generation's view (docs/adr/ADR-0013-search-index.md decision 3): the
//! latest commit's searcher, and per segment the alive set — not deleted and
//! visible in the generation — with the statistics over it.

use std::collections::HashMap;
use std::sync::Arc;

use tantivy::index::SegmentId;
use tantivy::{DocId, Searcher, SegmentReader};

use crate::error::Result;
use crate::index::{lock, Index};
use crate::summary::{Lengths, Summary};

/// One segment's alive set and the statistics over it.
pub struct AliveSet {
    words: Vec<u64>,
    /// Alive documents.
    pub docs: u64,
    /// Σ total length and Σ weighted length over the alive documents.
    pub total: u64,
    pub weighted: u64,
    /// No document of the segment is deleted or hidden: the summary's df is exact.
    pub clean: bool,
}

impl AliveSet {
    #[inline]
    pub fn alive(&self, d: DocId) -> bool {
        self.words.get((d >> 6) as usize).is_some_and(|w| (w >> (d & 63)) & 1 == 1)
    }

    fn bytes(&self) -> u64 {
        (self.words.len() * 8 + std::mem::size_of::<AliveSet>()) as u64
    }

    fn build(seg: &SegmentReader, fields: &crate::schema::Fields, visible: &[u8]) -> Result<AliveSet> {
        let max_doc = seg.max_doc();
        let ids = seg.fast_fields().u64(seg.schema().get_field_name(fields.doc_id))?;
        let lens = Lengths::open(seg)?;
        let deleted = seg.alive_bitset();
        let mut set = AliveSet {
            words: vec![0u64; (max_doc as usize).div_ceil(64)],
            docs: 0,
            total: 0,
            weighted: 0,
            clean: true,
        };
        for d in 0..max_doc {
            if deleted.is_some_and(|a| !a.is_alive(d)) {
                set.clean = false;
                continue;
            }
            let id = ids.first(d).unwrap_or(u64::MAX);
            let shown = usize::try_from(id / 8)
                .ok()
                .and_then(|i| visible.get(i))
                .is_some_and(|byte| (byte >> (id % 8)) & 1 == 1);
            if !shown {
                set.clean = false;
                continue;
            }
            set.words[(d >> 6) as usize] |= 1u64 << (d & 63);
            let (t, w) = lens.get(d);
            set.docs += 1;
            set.total = set.total.saturating_add(t);
            set.weighted = set.weighted.saturating_add(w);
        }
        Ok(set)
    }
}

type Key = (u64, SegmentId, Option<u64>);

/// The alive sets of every view an index opened, bounded in bytes, least
/// recently used evicted first. A zero bound caches nothing.
pub struct AliveCache {
    cap: u64,
    bytes: u64,
    tick: u64,
    entries: HashMap<Key, (Arc<AliveSet>, u64)>,
}

impl AliveCache {
    pub fn new(cap: u64) -> AliveCache {
        AliveCache { cap, bytes: 0, tick: 0, entries: HashMap::new() }
    }

    fn get(&mut self, k: &Key) -> Option<Arc<AliveSet>> {
        self.tick += 1;
        let tick = self.tick;
        self.entries.get_mut(k).map(|e| {
            e.1 = tick;
            e.0.clone()
        })
    }

    fn put(&mut self, k: Key, v: Arc<AliveSet>) {
        let size = v.bytes();
        if size > self.cap {
            return;
        }
        while self.bytes + size > self.cap {
            let Some(oldest) = self.entries.iter().min_by_key(|e| e.1 .1).map(|e| *e.0) else { break };
            if let Some((old, _)) = self.entries.remove(&oldest) {
                self.bytes -= old.bytes();
            }
        }
        self.tick += 1;
        if let Some((old, _)) = self.entries.insert(k, (v, self.tick)) {
            self.bytes -= old.bytes();
        }
        self.bytes += size;
    }
}

/// One segment as a view sees it.
pub struct SegView {
    pub alive: Arc<AliveSet>,
    pub summary: Option<Arc<Summary>>,
}

pub struct View {
    pub index: Arc<Index>,
    pub searcher: Searcher,
    pub segs: Vec<SegView>,
    pub docs: u64,
    pub total: u64,
    pub weighted: u64,
}

impl View {
    pub fn open(index: Arc<Index>, generation: u64, visible: &[u8]) -> Result<View> {
        index.reader.reload()?;
        let searcher = index.reader.searcher();
        let summaries = index.summaries_of(&searcher);
        let mut segs = Vec::with_capacity(searcher.segment_readers().len());
        let (mut docs, mut total, mut weighted) = (0u64, 0u64, 0u64);
        for (seg, summary) in searcher.segment_readers().iter().zip(summaries) {
            let key = (generation, seg.segment_id(), seg.delete_opstamp());
            let cached = lock(&index.alive).get(&key);
            let alive = match cached {
                Some(a) => a,
                None => {
                    let a = Arc::new(AliveSet::build(seg, &index.fields, visible)?);
                    lock(&index.alive).put(key, a.clone());
                    a
                }
            };
            docs += alive.docs;
            total = total.saturating_add(alive.total);
            weighted = weighted.saturating_add(alive.weighted);
            segs.push(SegView { alive, summary });
        }
        Ok(View { index, searcher, segs, docs, total, weighted })
    }
}
