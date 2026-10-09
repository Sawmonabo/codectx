use tantivy::collector::TopDocs;
use tantivy::collector::sort_key::{SortBySimilarityScore, SortByString};
use tantivy::query::TermQuery;
use tantivy::schema::{IndexRecordOption, Schema, TEXT, STRING, FAST};
use tantivy::{doc, Index, IndexWriter, Order, Term};
fn main() -> tantivy::Result<()> {
    let mut sb = Schema::builder();
    let body = sb.add_text_field("body", TEXT);
    let id = sb.add_text_field("id", STRING | FAST);
    let index = Index::create_in_ram(sb.build());
    let mut w: IndexWriter = index.writer_with_num_threads(1, 15_000_000)?;
    for k in ["c", "a", "b"] { w.add_document(doc!(body => "same text", id => k))?; }
    w.commit()?;
    let s = index.reader()?.searcher();
    let q = TermQuery::new(Term::from_field_text(body, "same"), IndexRecordOption::WithFreqs);
    let by_score = s.search(&q, &TopDocs::with_limit(3).order_by_score())?;
    let c = TopDocs::with_limit(3).order_by(((SortBySimilarityScore, Order::Desc), (SortByString::for_field("id"), Order::Asc)));
    let by_tuple = s.search(&q, &c)?;
    println!("order_by_score: {:?}", by_score.iter().map(|(_, a)| a.doc_id).collect::<Vec<_>>());
    println!("tuple (score desc, id asc): {:?}", by_tuple.iter().map(|(k, a)| (a.doc_id, k.1.clone())).collect::<Vec<_>>());
    Ok(())
}
