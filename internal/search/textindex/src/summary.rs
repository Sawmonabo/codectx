//! Block summaries (docs/adr/ADR-0013-search-index.md decision 5): one file
//! per segment, `<segment uuid>.cxsum` in the index directory, holding for
//! every token of any text column its document frequency over the whole
//! segment and, for every 128-document block that holds it, the block's
//! largest weighted term frequency and smallest document length under both
//! length rules. Deleted documents are included; they only loosen a bound.
//!
//! The file is read through a memory map: a term is found by binary search
//! over a sorted table, and nothing per segment is copied onto the heap.
//!
//! Layout (little-endian):
//!   0   magic            8 bytes
//!   8   segment uuid     32 lowercase hex bytes
//!   40  max_doc          u32
//!   44  block_bits       u32
//!   48  n_terms          u64
//!   56  n_blocks         u64   (block entries, 16 bytes each, from offset 96)
//!   64  strings_len      u64   (term bytes, after the block entries)
//!   72  table_off        u64   (n_terms x 24-byte entries, after the strings)
//!   80  file_len         u64
//!   88  checksum         u64   (FNV-1a 64 over every byte from offset 96)
//!   96  block entries    { u32 block, u32 wtf_max, u32 min_len_total, u32 min_len_weighted }
//!       term bytes
//!       term table       { u64 string offset, u64 first entry, u32 df, u32 entries }
//!
//! A wtf_max of u32::MAX means "unbounded" (the true value did not fit); a
//! length that does not fit is stored as u32::MAX, which is below the true
//! length and so only loosens the bound.

use std::fs;
use std::io::{BufWriter, Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};

use tantivy::columnar::Column;
use tantivy::index::SegmentId;
use tantivy::postings::Postings;
use tantivy::schema::IndexRecordOption;
use tantivy::{DocId, DocSet, SegmentReader, TERMINATED};

use crate::error::{Error, Result};
use crate::schema::Fields;
use crate::score::WEIGHTS;

pub const BLOCK_BITS: u32 = 7;
pub const BLOCK: u32 = 1 << BLOCK_BITS;
pub const UNBOUNDED_WTF: u32 = u32::MAX;

const MAGIC: &[u8; 8] = b"CXBLKSUM";
const HEADER: usize = 96;
const ENTRY: usize = 16;
const TERM: usize = 24;
pub const SUFFIX: &str = ".cxsum";

pub fn file_name(seg: SegmentId) -> String {
    format!("{}{SUFFIX}", seg.uuid_string())
}

/// One block's bound inputs.
#[derive(Clone, Copy, Debug)]
pub struct BlockEntry {
    pub block: u32,
    pub wtf_max: u32,
    pub min_len_total: u32,
    pub min_len_weighted: u32,
}

/// One term's entry.
#[derive(Clone, Copy, Debug)]
pub struct TermEntry {
    pub df: u32,
    first: u64,
    count: u32,
}

/// A segment's summary, mapped.
pub struct Summary {
    map: memmap2::Mmap,
    n_terms: u64,
    strings_len: u64,
    table_off: u64,
}

fn fnv1a(mut h: u64, bytes: &[u8]) -> u64 {
    for &b in bytes {
        h ^= u64::from(b);
        h = h.wrapping_mul(0x100_0000_01b3);
    }
    h
}
const FNV_OFFSET: u64 = 0xcbf2_9ce4_8422_2325;

fn u32_at(b: &[u8], at: usize) -> u32 {
    u32::from_le_bytes(b[at..at + 4].try_into().expect("4 bytes"))
}
fn u64_at(b: &[u8], at: usize) -> u64 {
    u64::from_le_bytes(b[at..at + 8].try_into().expect("8 bytes"))
}

/// The two document lengths of every document, from the length columns.
pub struct Lengths {
    cols: Vec<Column<u64>>,
}

impl Lengths {
    pub fn open(seg: &SegmentReader) -> Result<Lengths> {
        let ff = seg.fast_fields();
        let cols = crate::schema::LENS.iter().map(|n| ff.u64(n)).collect::<tantivy::Result<Vec<_>>>()?;
        Ok(Lengths { cols })
    }
    /// (total, weighted) of document d.
    pub fn get(&self, d: DocId) -> (u64, u64) {
        let mut total = 0u64;
        let mut weighted = 0u64;
        for (c, w) in self.cols.iter().zip(WEIGHTS) {
            let n = c.first(d).unwrap_or(0);
            total = total.saturating_add(n);
            weighted = weighted.saturating_add(n.saturating_mul(w));
        }
        (total, weighted)
    }
}

/// Builds the summary of seg and writes it atomically into dir.
pub fn build(dir: &Path, seg: &SegmentReader, fields: &Fields) -> Result<()> {
    let lens = Lengths::open(seg)?;
    let invs = fields.text.iter().map(|f| seg.inverted_index(*f)).collect::<tantivy::Result<Vec<_>>>()?;
    let mut streams = Vec::with_capacity(invs.len());
    for inv in &invs {
        streams.push(inv.terms().stream()?);
    }
    let mut cur: Vec<Option<(Vec<u8>, tantivy::postings::TermInfo)>> = streams
        .iter_mut()
        .map(|s| if s.advance() { Some((s.key().to_vec(), s.value().clone())) } else { None })
        .collect();

    let name = file_name(seg.segment_id());
    let tmp = dir.join(format!("{name}.tmp"));
    let file = fs::File::create(&tmp)?;
    let mut w = BufWriter::new(file);
    w.write_all(&[0u8; HEADER])?;
    let mut sum = FNV_OFFSET;
    let mut n_blocks = 0u64;
    let mut strings: Vec<u8> = Vec::new();
    let mut table: Vec<u8> = Vec::new();
    let mut n_terms = 0u64;
    let mut docs: Vec<(DocId, u64)> = Vec::new();
    while let Some(min) = cur.iter().flatten().map(|x| &x.0).min().cloned() {
        docs.clear();
        for i in 0..cur.len() {
            if cur[i].as_ref().is_some_and(|x| x.0 == min) {
                let ti = cur[i].take().expect("current term").1;
                let mut p = invs[i].read_postings_from_terminfo(&ti, IndexRecordOption::WithFreqs)?;
                let wt = WEIGHTS[i];
                let mut d = p.doc();
                while d != TERMINATED {
                    docs.push((d, wt * u64::from(p.term_freq())));
                    d = p.advance();
                }
                cur[i] = if streams[i].advance() {
                    Some((streams[i].key().to_vec(), streams[i].value().clone()))
                } else {
                    None
                };
            }
        }
        docs.sort_unstable_by_key(|x| x.0);
        let first = n_blocks;
        let mut df = 0u32;
        let mut entry: Option<BlockEntry> = None;
        let mut j = 0;
        while j < docs.len() {
            let d = docs[j].0;
            let mut wtf = 0u64;
            while j < docs.len() && docs[j].0 == d {
                wtf = wtf.saturating_add(docs[j].1);
                j += 1;
            }
            df += 1;
            let b = d >> BLOCK_BITS;
            let (lt, lw) = lens.get(d);
            let wtf = u32::try_from(wtf).ok().filter(|v| *v != UNBOUNDED_WTF).unwrap_or(UNBOUNDED_WTF);
            let lt = u32::try_from(lt).unwrap_or(u32::MAX);
            let lw = u32::try_from(lw).unwrap_or(u32::MAX);
            match entry.as_mut() {
                Some(e) if e.block == b => {
                    e.wtf_max = e.wtf_max.max(wtf);
                    e.min_len_total = e.min_len_total.min(lt);
                    e.min_len_weighted = e.min_len_weighted.min(lw);
                }
                _ => {
                    if let Some(e) = entry.take() {
                        write_entry(&mut w, &mut sum, &e)?;
                        n_blocks += 1;
                    }
                    entry = Some(BlockEntry { block: b, wtf_max: wtf, min_len_total: lt, min_len_weighted: lw });
                }
            }
        }
        if let Some(e) = entry.take() {
            write_entry(&mut w, &mut sum, &e)?;
            n_blocks += 1;
        }
        table.extend_from_slice(&(strings.len() as u64).to_le_bytes());
        table.extend_from_slice(&first.to_le_bytes());
        table.extend_from_slice(&df.to_le_bytes());
        table.extend_from_slice(&((n_blocks - first) as u32).to_le_bytes());
        strings.extend_from_slice(&min);
        n_terms += 1;
    }
    sum = fnv1a(sum, &strings);
    w.write_all(&strings)?;
    sum = fnv1a(sum, &table);
    w.write_all(&table)?;
    let table_off = (HEADER as u64) + n_blocks * ENTRY as u64 + strings.len() as u64;
    let file_len = table_off + table.len() as u64;
    let mut h = Vec::with_capacity(HEADER);
    h.extend_from_slice(MAGIC);
    h.extend_from_slice(seg.segment_id().uuid_string().as_bytes());
    h.extend_from_slice(&seg.max_doc().to_le_bytes());
    h.extend_from_slice(&BLOCK_BITS.to_le_bytes());
    h.extend_from_slice(&n_terms.to_le_bytes());
    h.extend_from_slice(&n_blocks.to_le_bytes());
    h.extend_from_slice(&(strings.len() as u64).to_le_bytes());
    h.extend_from_slice(&table_off.to_le_bytes());
    h.extend_from_slice(&file_len.to_le_bytes());
    h.extend_from_slice(&sum.to_le_bytes());
    debug_assert_eq!(h.len(), HEADER);
    w.seek(SeekFrom::Start(0))?;
    w.write_all(&h)?;
    let file = w.into_inner().map_err(|e| Error::from(e.into_error()))?;
    file.sync_all()?;
    drop(file);
    fs::rename(&tmp, dir.join(&name))?;
    sync_dir(dir);
    Ok(())
}

fn write_entry(w: &mut impl Write, sum: &mut u64, e: &BlockEntry) -> Result<()> {
    let mut b = [0u8; ENTRY];
    b[0..4].copy_from_slice(&e.block.to_le_bytes());
    b[4..8].copy_from_slice(&e.wtf_max.to_le_bytes());
    b[8..12].copy_from_slice(&e.min_len_total.to_le_bytes());
    b[12..16].copy_from_slice(&e.min_len_weighted.to_le_bytes());
    *sum = fnv1a(*sum, &b);
    w.write_all(&b)?;
    Ok(())
}

#[cfg(unix)]
fn sync_dir(dir: &Path) {
    // The rename's durability; a failure leaves the summary rebuildable, never wrong.
    if let Ok(d) = fs::File::open(dir) {
        let _ = d.sync_all();
    }
}

#[cfg(not(unix))]
fn sync_dir(_: &Path) {}

impl Summary {
    /// Maps and validates seg's summary in dir. None when the file is absent,
    /// unreadable, damaged, or names another segment or document count: the
    /// segment is then scored as one without a summary.
    pub fn load(dir: &Path, seg: &SegmentReader) -> Option<Summary> {
        let file = fs::File::open(dir.join(file_name(seg.segment_id()))).ok()?;
        // SAFETY: the file is written once under a temporary name and renamed
        // into place; it is never modified in place afterwards.
        let map = unsafe { memmap2::Mmap::map(&file) }.ok()?;
        let b: &[u8] = &map;
        if b.len() < HEADER || &b[0..8] != MAGIC {
            return None;
        }
        if &b[8..40] != seg.segment_id().uuid_string().as_bytes() {
            return None;
        }
        if u32_at(b, 40) != seg.max_doc() || u32_at(b, 44) != BLOCK_BITS {
            return None;
        }
        let n_terms = u64_at(b, 48);
        let n_blocks = u64_at(b, 56);
        let strings_len = u64_at(b, 64);
        let table_off = u64_at(b, 72);
        let file_len = u64_at(b, 80);
        let blocks_end = (HEADER as u64).checked_add(n_blocks.checked_mul(ENTRY as u64)?)?;
        if blocks_end.checked_add(strings_len)? != table_off
            || table_off.checked_add(n_terms.checked_mul(TERM as u64)?)? != file_len
            || file_len != b.len() as u64
        {
            return None;
        }
        if fnv1a(FNV_OFFSET, &b[HEADER..]) != u64_at(b, 88) {
            return None;
        }
        let s = Summary { map, n_terms, strings_len, table_off };
        // Every table entry must point inside the file, strings ascending.
        let mut prev_off = 0u64;
        for i in 0..n_terms {
            let (off, first, _, count) = s.raw(i);
            if off < prev_off || off > strings_len || first.checked_add(u64::from(count))? > n_blocks {
                return None;
            }
            prev_off = off;
        }
        Some(s)
    }

    fn raw(&self, i: u64) -> (u64, u64, u32, u32) {
        let at = (self.table_off + i * TERM as u64) as usize;
        let b: &[u8] = &self.map;
        (u64_at(b, at), u64_at(b, at + 8), u32_at(b, at + 16), u32_at(b, at + 20))
    }

    fn term_bytes(&self, i: u64) -> &[u8] {
        let (off, _, _, _) = self.raw(i);
        let end = if i + 1 < self.n_terms { self.raw(i + 1).0 } else { self.strings_len };
        let base = self.table_off - self.strings_len;
        &self.map[(base + off) as usize..(base + end) as usize]
    }

    /// The entry of term, if any document of the segment holds it.
    pub fn term(&self, term: &[u8]) -> Option<TermEntry> {
        let (mut lo, mut hi) = (0u64, self.n_terms);
        while lo < hi {
            let mid = lo + (hi - lo) / 2;
            match self.term_bytes(mid).cmp(term) {
                std::cmp::Ordering::Less => lo = mid + 1,
                std::cmp::Ordering::Greater => hi = mid,
                std::cmp::Ordering::Equal => {
                    let (_, first, df, count) = self.raw(mid);
                    return Some(TermEntry { df, first, count });
                }
            }
        }
        None
    }

    /// The block entries of a term, blocks ascending.
    pub fn blocks(&self, t: &TermEntry) -> impl Iterator<Item = BlockEntry> + '_ {
        let b: &[u8] = &self.map;
        let first = t.first;
        (0..u64::from(t.count)).map(move |k| {
            let at = HEADER + ((first + k) as usize) * ENTRY;
            BlockEntry {
                block: u32_at(b, at),
                wtf_max: u32_at(b, at + 4),
                min_len_total: u32_at(b, at + 8),
                min_len_weighted: u32_at(b, at + 12),
            }
        })
    }
}

/// Removes every summary (and leftover temporary) whose segment is not in live.
pub fn remove_orphans(dir: &Path, live: &[SegmentId]) {
    let keep: std::collections::HashSet<String> = live.iter().map(|s| file_name(*s)).collect();
    let Ok(rd) = fs::read_dir(dir) else { return };
    for e in rd.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        let ours = name.ends_with(SUFFIX) || name.ends_with(&format!("{SUFFIX}.tmp"));
        if ours && !keep.contains(&name) {
            // A file still mapped by an open view may refuse removal on some
            // platforms; the next release of the writer removes it.
            let _ = fs::remove_file(PathBuf::from(dir).join(&name));
        }
    }
}
