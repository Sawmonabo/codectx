use std::collections::{HashMap, HashSet as Set};
use std::fmt;

/// Non-ASCII: héllo → 日本
pub const GREETING: &str = "héllo → 日本";

/// A server.
pub struct Server {
    pub name: String,
    port: u16,
}

pub trait Handler {
    fn serve(&self, name: &str) -> fmt::Result;
}

impl Server {
    /// Start the server.
    pub fn start(&self) -> usize {
        fn inner(n: &str) -> usize { helper(n) }
        let m: HashMap<String, Set<u8>> = HashMap::new();
        inner(&self.name) + m.len()
    }
}

fn helper(name: &str) -> usize { name.len() }

pub mod nested {
    pub enum Mode { Fast, Slow }
}

#[test]
fn starts_the_server() { assert_eq!(helper("x"), 1); }
