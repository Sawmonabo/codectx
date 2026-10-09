//! SQLite `unicode61` with its defaults (categories "L* N* Co", remove_diacritics=1),
//! driven by tables probed from the SQLite build the product links. A token is a
//! maximal run of non-separator codepoints; each codepoint is case-folded and
//! diacritic-stripped on its own (a fold of 0 drops it). FTS5 stores at most
//! FTS5_MAX_TOKEN_SIZE bytes of a token.

use tantivy::tokenizer::{Token, TokenStream, Tokenizer};

#[path = "unicode61_tables.rs"]
mod tables;

/// FTS5_MAX_TOKEN_SIZE: fts5 truncates a longer token to this many bytes.
pub const MAX_TOKEN_BYTES: usize = 32768;

#[inline]
fn is_separator(cp: u32) -> bool {
    if cp < 128 {
        let b = cp as u8;
        return !(b.is_ascii_alphanumeric());
    }
    match tables::SEPARATORS.binary_search_by(|&(lo, hi)| {
        if hi < cp {
            std::cmp::Ordering::Less
        } else if lo > cp {
            std::cmp::Ordering::Greater
        } else {
            std::cmp::Ordering::Equal
        }
    }) {
        Ok(_) => true,
        Err(_) => false,
    }
}

/// The folded codepoint, or None when unicode61 drops it from the token.
#[inline]
fn fold(c: char) -> Option<char> {
    let cp = c as u32;
    if cp < 128 {
        return Some(c.to_ascii_lowercase());
    }
    match tables::FOLDS.binary_search_by_key(&cp, |&(k, _)| k) {
        Ok(i) => {
            let v = tables::FOLDS[i].1;
            if v == 0 { None } else { char::from_u32(v) }
        }
        Err(_) => Some(c),
    }
}

/// Calls `f(position, byte_from, byte_to, term)` for every token of `text`.
pub fn for_each_token(text: &str, mut f: impl FnMut(usize, usize, usize, &str)) {
    let mut buf = String::new();
    let mut start: Option<usize> = None;
    let mut pos = 0usize;
    for (i, c) in text.char_indices() {
        if is_separator(c as u32) {
            if let Some(s) = start.take() {
                emit(&mut buf, &mut pos, s, i, &mut f);
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
        emit(&mut buf, &mut pos, s, text.len(), &mut f);
    }
}

#[inline]
fn emit(buf: &mut String, pos: &mut usize, from: usize, to: usize, f: &mut impl FnMut(usize, usize, usize, &str)) {
    // A run whose every codepoint folds away is not a token and takes no position.
    if buf.is_empty() {
        return;
    }
    if buf.len() > MAX_TOKEN_BYTES {
        let mut cut = MAX_TOKEN_BYTES;
        while !buf.is_char_boundary(cut) {
            cut -= 1;
        }
        buf.truncate(cut);
    }
    f(*pos, from, to, buf);
    *pos += 1;
    buf.clear();
}

/// `s` folded codepoint by codepoint as unicode61 folds a token's characters.
pub fn fold_str(s: &str) -> String {
    s.chars().filter_map(fold).collect()
}

/// The camelCase / snake_case parts of an identifier, in order. Separators are
/// unicode61's (which include "_"); inside a run, a part starts at an upper-case
/// letter that follows a lower-case letter or a digit, and at the last upper-case
/// letter of an acronym that a lower-case letter follows ("HTTPClient" ->
/// "HTTP", "Client"). Digits stay with the part they follow ("v1beta1" is one part).
pub fn split_parts(s: &str) -> Vec<&str> {
    let mut out = Vec::new();
    let mut run_start: Option<usize> = None;
    for (i, c) in s.char_indices() {
        if is_separator(c as u32) {
            if let Some(f) = run_start.take() {
                camel(&s[f..i], &mut out);
            }
        } else if run_start.is_none() {
            run_start = Some(i);
        }
    }
    if let Some(f) = run_start {
        camel(&s[f..], &mut out);
    }
    out
}

fn camel<'a>(run: &'a str, out: &mut Vec<&'a str>) {
    let rc: Vec<(usize, char)> = run.char_indices().collect();
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
}

/// Every token of `text`, in order.
pub fn tokenize(text: &str) -> Vec<String> {
    let mut out = Vec::new();
    for_each_token(text, |_, _, _, t| out.push(t.to_string()));
    out
}

/// The number of tokens of `text`.
pub fn count(text: &str) -> u64 {
    let mut n = 0u64;
    for_each_token(text, |_, _, _, _| n += 1);
    n
}

/// The tantivy face of the same tokenizer.
#[derive(Clone, Default)]
pub struct Unicode61;

pub struct Unicode61Stream {
    tokens: Vec<Token>,
    at: usize,
}

impl Tokenizer for Unicode61 {
    type TokenStream<'a> = Unicode61Stream;
    fn token_stream<'a>(&'a mut self, text: &'a str) -> Unicode61Stream {
        let mut tokens = Vec::new();
        for_each_token(text, |p, from, to, t| {
            tokens.push(Token { offset_from: from, offset_to: to, position: p, text: t.to_string(), position_length: 1 })
        });
        Unicode61Stream { tokens, at: 0 }
    }
}

impl Unicode61Stream {
    pub fn new(tokens: Vec<Token>) -> Unicode61Stream {
        Unicode61Stream { tokens, at: 0 }
    }
}

impl TokenStream for Unicode61Stream {
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

/// The code-aware variant: every unicode61 token, and, when the token's
/// original spelling has two or more camelCase parts, each part (folded) at the
/// SAME position as the whole token. snake_case needs nothing extra: "_" already
/// separates tokens in unicode61.
#[derive(Clone, Default)]
pub struct Code;

/// Calls `f(position, from, to, term, is_part)` for every code token.
pub fn for_each_code_token(text: &str, mut f: impl FnMut(usize, usize, usize, &str, bool)) {
    for_each_token(text, |p, from, to, t| {
        f(p, from, to, t, false);
        let parts = split_parts(&text[from..to]);
        if parts.len() >= 2 {
            for part in parts {
                let folded = fold_str(part);
                if !folded.is_empty() && folded != t {
                    f(p, from, to, &folded, true);
                }
            }
        }
    });
}

impl Tokenizer for Code {
    type TokenStream<'a> = Unicode61Stream;
    fn token_stream<'a>(&'a mut self, text: &'a str) -> Unicode61Stream {
        let mut tokens = Vec::new();
        for_each_code_token(text, |p, from, to, t, _| {
            tokens.push(Token { offset_from: from, offset_to: to, position: p, text: t.to_string(), position_length: 1 })
        });
        Unicode61Stream { tokens, at: 0 }
    }
}
