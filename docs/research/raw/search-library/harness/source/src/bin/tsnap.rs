//! tsnap <index dir> <snapshot dir>
//!
//! A retained generation by hard links: every file of every searchable segment
//! (SegmentMeta::list_files) plus meta.json and the bench's tb-config.json is
//! hard-linked into a new directory, which then opens as an index of its own.
//! Prints the time, the file count, the bytes the links cover, and the snapshot's
//! document count against the source's.
use std::time::Instant;
use tantivy::ReloadPolicy;
use tbench::{anyhow_lite::Result, open_index, variants};

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() < 3 {
        return Err("usage: tsnap <index dir> <snapshot dir>".into());
    }
    let (src, dst) = (std::path::Path::new(&a[1]), std::path::Path::new(&a[2]));
    if dst.exists() {
        return Err(format!("{} exists", dst.display()).into());
    }
    let (index, _) = open_index(src, None)?;
    let docs = |i: &tantivy::Index| -> Result<u64> {
        Ok(i.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?.searcher().num_docs())
    };
    let before = docs(&index)?;
    let t = Instant::now();
    std::fs::create_dir_all(dst)?;
    let mut files: Vec<std::path::PathBuf> = vec!["meta.json".into()];
    for m in index.searchable_segment_metas()? {
        files.extend(m.list_files());
    }
    let (mut n, mut bytes) = (0u64, 0u64);
    for f in &files {
        let from = src.join(f);
        if !from.exists() {
            continue;
        }
        std::fs::hard_link(&from, dst.join(f))?;
        n += 1;
        bytes += std::fs::metadata(&from)?.len();
    }
    let ms = t.elapsed().as_secs_f64() * 1e3;
    if src.join(variants::CONFIG_FILE).exists() {
        std::fs::copy(src.join(variants::CONFIG_FILE), dst.join(variants::CONFIG_FILE))?;
    }
    let (snap, _) = open_index(dst, None)?;
    let after = docs(&snap)?;
    println!(
        "{}",
        serde_json::json!({"snapshot_ms": ms, "files": n, "bytes_linked": bytes, "docs_source": before, "docs_snapshot": after, "same_docs": before == after})
    );
    if before != after {
        return Err("the snapshot does not hold the source's documents".into());
    }
    Ok(())
}
