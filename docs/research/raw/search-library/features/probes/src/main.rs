use tantivy::collector::{TopDocs, Collector, SegmentCollector};
use tantivy::query::{DisjunctionMaxQuery, Query, TermQuery, BooleanQuery, Occur};
use tantivy::schema::{IndexRecordOption, Schema, TEXT, STRING};
use tantivy::{doc, Index, IndexWriter, Term, DocId, Score, SegmentReader};

// Collector that keeps every (doc, score): no pruning path.
struct All;
struct AllSeg(Vec<(DocId, Score)>);
impl Collector for All {
    type Fruit = Vec<(DocId, Score)>;
    type Child = AllSeg;
    fn for_segment(&self, _: u32, _: &SegmentReader) -> tantivy::Result<AllSeg> { Ok(AllSeg(vec![])) }
    fn requires_scoring(&self) -> bool { true }
    fn merge_fruits(&self, f: Vec<Vec<(DocId, Score)>>) -> tantivy::Result<Self::Fruit> { Ok(f.into_iter().flatten().collect()) }
}
impl SegmentCollector for AllSeg {
    type Fruit = Vec<(DocId, Score)>;
    fn collect(&mut self, d: DocId, s: Score) { self.0.push((d, s)); }
    fn harvest(self) -> Self::Fruit { self.0 }
}

fn main() -> tantivy::Result<()> {
    let mut sb = Schema::builder();
    let title = sb.add_text_field("title", TEXT);
    let body = sb.add_text_field("body", TEXT);
    let key = sb.add_text_field("key", STRING);
    let schema = sb.build();
    let index = Index::create_in_ram(schema);
    let mut w: IndexWriter = index.writer_with_num_threads(1, 15_000_000)?;
    w.add_document(doc!(title => "alpha beta", body => "alpha gamma", key => "a"))?;
    w.add_document(doc!(title => "alpha", body => "delta", key => "b"))?;
    w.add_document(doc!(title => "zeta", body => "alpha", key => "c"))?;
    for i in 0..20 { w.add_document(doc!(title => "filler words", body => format!("filler {i}"), key => format!("f{i}")))?; }
    w.commit()?;
    let r = index.reader()?;
    let s = r.searcher();
    for opt in [IndexRecordOption::Basic, IndexRecordOption::WithFreqs] {
        let qs: Vec<Box<dyn Query>> = vec![
            Box::new(TermQuery::new(Term::from_field_text(title, "alpha"), opt)),
            Box::new(TermQuery::new(Term::from_field_text(body, "alpha"), opt)),
        ];
        let dm = DisjunctionMaxQuery::with_tie_breaker(qs, 0.0);
        let top = s.search(&dm, &TopDocs::with_limit(10).order_by_score())?;
        let all = s.search(&dm, &All)?;
        println!("dismax tie=0 opt={opt:?}\n  TopDocs: {:?}\n  all-docs collector: {:?}", top.iter().map(|(sc, a)| (a.doc_id, *sc)).collect::<Vec<_>>(), all);
    }
    // doc_freq and N before and after a delete
    let t = Term::from_field_text(title, "alpha");
    println!("before delete: doc_freq={} num_docs={}", s.doc_freq(&t)?, s.num_docs());
    w.delete_term(Term::from_field_text(key, "a"));
    w.commit()?;
    r.reload()?;
    let s2 = r.searcher();
    println!("after delete: doc_freq={} num_docs={} segments={}", s2.doc_freq(&t)?, s2.num_docs(), s2.segment_readers().len());
    let q = TermQuery::new(t.clone(), IndexRecordOption::WithFreqs);
    let e = q.explain(&s2, tantivy::DocAddress::new(0, 1))?;
    println!("explain doc1 after delete:\n{}", e.to_pretty_json());
    let _ = BooleanQuery::new(vec![(Occur::Should, Box::new(q) as Box<dyn Query>)]);
    Ok(())
}
