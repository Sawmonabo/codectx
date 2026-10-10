//! tblock <index dir> [--dl weighted]
//!
//! Writes product-topk's side structure (topk.rs): one file per searchable
//! segment, `tb-blockmax-<segment uuid>-<dl>.bin`, beside the index. The length
//! column is token_count, or token_count_code on a `--tokenizer code` index.
//! Segments are immutable, so a file stays valid until a merge replaces its
//! segment (`--dl weighted`: the weighted column-length sum); run tblock again after any commit or merge. Prints per segment the
//! time, the tokens and blocks stored, and the bytes; then the totals as JSON.
use std::time::Instant;
use tantivy::ReloadPolicy;
use tbench::{anyhow_lite::Result, open_index, product, topk};

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    let dir = std::path::Path::new(a.get(1).ok_or("usage: tblock <index dir>")?);
    let (index, f) = open_index(dir, None)?;
    let weighted = a.iter().any(|x| x == "--dl") && a.iter().any(|x| x == "weighted");
    let dl = if weighted {
        product::DL_WEIGHTED
    } else if f.token_count_code.is_some() {
        product::DL_CODE
    } else {
        product::DL
    };
    let s = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?.searcher();
    let t0 = Instant::now();
    let (mut bytes, mut terms, mut blocks) = (0u64, 0u64, 0u64);
    for seg in s.segment_readers() {
        let t = Instant::now();
        let sb = topk::build(seg, &f, dl)?;
        let b = sb.write(&dir.join(topk::file_name(seg.segment_id(), dl)))?;
        let nb: u64 = sb.0.values().map(|x| x.blocks.len() as u64).sum();
        eprintln!("tblock segment {}: {} docs, {} tokens, {nb} blocks, {b} B, {:.1} ms", seg.segment_id().short_uuid_string(), seg.max_doc(), sb.0.len(), t.elapsed().as_secs_f64() * 1e3);
        bytes += b;
        terms += sb.0.len() as u64;
        blocks += nb;
    }
    println!(
        "{}",
        serde_json::json!({"segments": s.segment_readers().len(), "dl": dl, "tokens": terms, "blocks": blocks, "bytes": bytes, "build_ms": t0.elapsed().as_secs_f64() * 1e3})
    );
    Ok(())
}
