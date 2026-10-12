//! The code tokenizer (docs/adr/ADR-0013-search-index.md decision 6).
//!
//! The base rules are unicode61 with its defaults: letters, numbers and
//! private-use characters form tokens, everything else separates, and every
//! codepoint is case-folded and diacritic-stripped on its own (a fold to
//! nothing drops the codepoint). A token is never cut for length.
//!
//! On top of that, every token whose original spelling has two or more
//! camelCase parts also yields each folded part at the token's own position,
//! so `NewController` is indexed as `newcontroller`, `new` and `controller`.
//! snake_case needs nothing more: `_` already separates.

use tantivy::tokenizer::{Token, TokenStream, Tokenizer};

use crate::tables;

fn is_separator(cp: u32) -> bool {
    if cp < 128 {
        return !(cp as u8).is_ascii_alphanumeric();
    }
    tables::SEPARATORS
        .binary_search_by(|&(lo, hi)| {
            if hi < cp {
                std::cmp::Ordering::Less
            } else if lo > cp {
                std::cmp::Ordering::Greater
            } else {
                std::cmp::Ordering::Equal
            }
        })
        .is_ok()
}

/// The folded codepoint, or None when the rules drop it from the token.
fn fold(c: char) -> Option<char> {
    let cp = c as u32;
    if cp < 128 {
        return Some(c.to_ascii_lowercase());
    }
    match tables::FOLDS.binary_search_by_key(&cp, |&(k, _)| k) {
        Ok(i) => match tables::FOLDS[i].1 {
            0 => None,
            v => char::from_u32(v),
        },
        Err(_) => Some(c),
    }
}

/// Calls `f(position, byte_from, byte_to, token)` for every base token.
pub fn for_each_token(text: &str, mut f: impl FnMut(usize, usize, usize, &str)) {
    let mut buf = String::new();
    let mut start: Option<usize> = None;
    let mut pos = 0usize;
    let mut emit = |buf: &mut String, from: usize, to: usize, pos: &mut usize| {
        // A run whose every codepoint folds away is no token and takes no position.
        if buf.is_empty() {
            return;
        }
        f(*pos, from, to, buf);
        *pos += 1;
        buf.clear();
    };
    for (i, c) in text.char_indices() {
        if is_separator(c as u32) {
            if let Some(s) = start.take() {
                emit(&mut buf, s, i, &mut pos);
            }
            continue;
        }
        if start.is_none() {
            start = Some(i);
        }
        if let Some(fc) = fold(c) {
            buf.push(fc);
        }
    }
    if let Some(s) = start {
        emit(&mut buf, s, text.len(), &mut pos);
    }
}

fn fold_str(s: &str) -> String {
    s.chars().filter_map(fold).collect()
}

/// The camelCase parts of one separator-free run, in order. A part starts at
/// an upper-case letter that follows a lower-case letter or a digit, and at
/// the last upper-case letter of an acronym that a lower-case letter follows
/// ("HTTPClient" gives "HTTP", "Client"). Digits stay with the part they
/// follow ("v1beta1" is one part).
fn camel_parts(run: &str) -> Vec<&str> {
    let rc: Vec<(usize, char)> = run.char_indices().collect();
    let mut out = Vec::new();
    let mut start = 0;
    for i in 1..rc.len() {
        let (prev, cur) = (rc[i - 1].1, rc[i].1);
        let next_lower = rc.get(i + 1).is_some_and(|x| x.1.is_lowercase());
        if cur.is_uppercase() && (prev.is_lowercase() || prev.is_numeric() || (prev.is_uppercase() && next_lower)) {
            out.push(&run[start..rc[i].0]);
            start = rc[i].0;
        }
    }
    out.push(&run[start..]);
    out
}

/// Calls `f(position, byte_from, byte_to, token)` for every code token: each
/// base token, then its folded camelCase parts at the same position when it
/// has two or more and a part differs from the whole token.
pub fn for_each_code_token(text: &str, mut f: impl FnMut(usize, usize, usize, &str)) {
    for_each_token(text, |p, from, to, t| {
        f(p, from, to, t);
        let parts = camel_parts(&text[from..to]);
        if parts.len() >= 2 {
            for part in parts {
                let folded = fold_str(part);
                if !folded.is_empty() && folded != t {
                    f(p, from, to, &folded);
                }
            }
        }
    });
}

/// The number of code tokens of `text`: a document column's length.
pub fn code_token_count(text: &str) -> u64 {
    let mut n = 0u64;
    for_each_code_token(text, |_, _, _, _| n += 1);
    n
}

/// Every base token of `text`, in order.
pub fn tokenize(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    for_each_token(text, |_, _, _, t| out.push(t.to_string()));
    out
}

/// The name the code tokenizer is registered under in every index.
pub const CODE_TOKENIZER: &str = "code";

/// The index-side face of the code tokenizer.
#[derive(Clone, Default)]
pub struct Code;

pub struct CodeStream {
    tokens: Vec<Token>,
    at: usize,
}

impl Tokenizer for Code {
    type TokenStream<'a> = CodeStream;
    fn token_stream<'a>(&'a mut self, text: &'a str) -> CodeStream {
        let mut tokens = Vec::new();
        for_each_code_token(text, |p, from, to, t| {
            tokens.push(Token { offset_from: from, offset_to: to, position: p, text: t.to_string(), position_length: 1 })
        });
        CodeStream { tokens, at: 0 }
    }
}

impl TokenStream for CodeStream {
    fn advance(&mut self) -> bool {
        if self.at < self.tokens.len() {
            self.at += 1;
            true
        } else {
            false
        }
    }
    fn token(&self) -> &Token {
        &self.tokens[self.at - 1]
    }
    fn token_mut(&mut self) -> &mut Token {
        &mut self.tokens[self.at - 1]
    }
}

/// One scored query term: a single token, or a quoted phrase's tokens.
#[derive(Clone, Debug)]
pub struct QueryTerm {
    pub tokens: Vec<String>,
}

impl QueryTerm {
    pub fn phrase(&self) -> bool {
        self.tokens.len() > 1
    }
}

/// Parses a query: a quoted run is one phrase of whole base tokens (a run of
/// one token is a plain term); an unquoted run yields every code token as its
/// own term, so a whole identifier and each of its parts are OR'd. Duplicate
/// terms fold, the first kept.
pub fn parse_query(text: &str) -> Vec<QueryTerm> {
    let mut terms: Vec<QueryTerm> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    let mut add = |tokens: Vec<String>| {
        let key = tokens.join(" ");
        if key.is_empty() || !seen.insert(key) {
            return;
        }
        terms.push(QueryTerm { tokens });
    };
    let mut quoted = false;
    for (i, seg) in text.split('"').enumerate() {
        if i > 0 {
            quoted = !quoted;
        }
        if seg.is_empty() {
            continue;
        }
        if quoted {
            add(tokenize(seg));
        } else {
            for_each_code_token(seg, |_, _, _, t| add(vec![t.to_string()]));
        }
    }
    terms
}
