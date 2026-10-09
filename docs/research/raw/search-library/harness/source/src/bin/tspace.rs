//! tspace <index dir>
//!
//! Prints Searcher::space_usage() (core/searcher.rs:240) as one JSON object:
//! totals per component (termdict, postings, positions, fast fields, fieldnorms,
//! store, deletes) summed over segments, the same per field, and the raw
//! per-segment breakdown. Untimed; the disk report for every build variant.
use std::collections::BTreeMap;
use tantivy::ReloadPolicy;
use tbench::{anyhow_lite::Result, open_index};

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    let dir = a.get(1).ok_or("usage: tspace <index dir>")?;
    let (index, _) = open_index(std::path::Path::new(dir), None)?;
    let s = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?.searcher();
    let u = s.space_usage()?;
    let mut comp: BTreeMap<&str, u64> = BTreeMap::new();
    let mut per_field: BTreeMap<String, u64> = BTreeMap::new();
    for seg in u.segments() {
        for (name, pf) in [
            ("termdict", seg.termdict()),
            ("postings", seg.postings()),
            ("positions", seg.positions()),
            ("fast_fields", seg.fast_fields()),
            ("fieldnorms", seg.fieldnorms()),
        ] {
            *comp.entry(name).or_default() += pf.total().get_bytes();
            for fu in pf.fields() {
                *per_field.entry(format!("{name}/{}", fu.field_name())).or_default() += fu.total().get_bytes();
            }
        }
        *comp.entry("store").or_default() += seg.store().total().get_bytes();
        *comp.entry("deletes").or_default() += seg.deletes().get_bytes();
    }
    let out = serde_json::json!({
        "index": std::path::Path::new(dir).file_name().map(|n| n.to_string_lossy().to_string()),
        "segments": u.segments().len(),
        "total": u.total().get_bytes(),
        "components": comp,
        "fields": per_field,
        "raw": serde_json::to_value(&u)?,
    });
    println!("{out}");
    Ok(())
}
