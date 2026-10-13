//! The index schema: the five text columns, the identity and comparator
//! columns, and the exact per-column lengths. Nothing is stored in the
//! library's document store: every field a page serves is a fast column.

use tantivy::schema::{Field, IndexRecordOption, NumericOptions, Schema, TextFieldIndexing, TextOptions};

use crate::error::{Error, Result};
use crate::tokenizer::CODE_TOKENIZER;

/// The text columns in the fixed column order the score sums them in.
pub const TEXT: [&str; 5] = ["name", "qualified_name", "signature", "path", "body"];
/// Each text column's exact code-token count.
pub const LENS: [&str; 5] = ["len_name", "len_qualified_name", "len_signature", "len_path", "len_body"];
/// The string fast columns, in the order a hit resolves them.
pub const STRS: [&str; 8] =
    ["path_s", "kind", "name_s", "qualified_name_s", "signature_s", "node_id", "search_key", "file_id"];
pub const S_PATH: usize = 0;
pub const S_KIND: usize = 1;
pub const S_NODE: usize = 5;

pub const DOC_ID: &str = "doc_id";
pub const UNIT: &str = "unit";
pub const START: &str = "start";
pub const END: &str = "end";

pub fn build() -> Schema {
    let mut b = Schema::builder();
    let text = TextOptions::default().set_indexing_options(
        TextFieldIndexing::default()
            .set_tokenizer(CODE_TOKENIZER)
            .set_index_option(IndexRecordOption::WithFreqsAndPositions)
            .set_fieldnorms(false),
    );
    for n in TEXT {
        b.add_text_field(n, text.clone());
    }
    b.add_u64_field(DOC_ID, NumericOptions::default().set_indexed().set_fast());
    b.add_text_field(
        UNIT,
        TextOptions::default()
            .set_indexing_options(
                TextFieldIndexing::default()
                    .set_tokenizer("raw")
                    .set_index_option(IndexRecordOption::Basic)
                    .set_fieldnorms(false),
            )
            .set_fast(None),
    );
    for n in STRS {
        b.add_text_field(n, TextOptions::default().set_fast(None));
    }
    for n in [START, END].into_iter().chain(LENS) {
        b.add_u64_field(n, NumericOptions::default().set_fast());
    }
    b.build()
}

/// Every field, resolved once from an index's schema.
#[derive(Clone, Copy)]
pub struct Fields {
    pub text: [Field; 5],
    pub lens: [Field; 5],
    pub strs: [Field; 8],
    pub doc_id: Field,
    pub unit: Field,
    pub start: Field,
    pub end: Field,
}

impl Fields {
    pub fn resolve(s: &Schema) -> Result<Fields> {
        let g = |n: &str| s.get_field(n).map_err(|_| Error::corrupt(format!("the index schema has no field {n}")));
        let arr5 = |names: [&str; 5]| -> Result<[Field; 5]> {
            Ok([g(names[0])?, g(names[1])?, g(names[2])?, g(names[3])?, g(names[4])?])
        };
        let mut strs = [g(STRS[0])?; 8];
        for (i, n) in STRS.iter().enumerate() {
            strs[i] = g(n)?;
        }
        Ok(Fields {
            text: arr5(TEXT)?,
            lens: arr5(LENS)?,
            strs,
            doc_id: g(DOC_ID)?,
            unit: g(UNIT)?,
            start: g(START)?,
            end: g(END)?,
        })
    }
}
