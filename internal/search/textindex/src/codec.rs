//! The buffer layouts of textindex.h: LEB128 uvarints, zigzag varints and
//! length-prefixed UTF-8 strings.

use crate::error::Error;

/// Reads one request buffer; every short or malformed read is CX_INVALID.
pub struct Reader<'a> {
    b: &'a [u8],
    at: usize,
}

impl<'a> Reader<'a> {
    pub fn new(b: &'a [u8]) -> Reader<'a> {
        Reader { b, at: 0 }
    }

    pub fn uvarint(&mut self) -> Result<u64, Error> {
        let mut v = 0u64;
        let mut shift = 0u32;
        loop {
            let byte = *self.b.get(self.at).ok_or_else(|| Error::invalid("a buffer ends inside a varint"))?;
            self.at += 1;
            if shift == 63 && byte > 1 {
                return Err(Error::invalid("a varint overflows 64 bits"));
            }
            v |= u64::from(byte & 0x7f) << shift;
            if byte & 0x80 == 0 {
                return Ok(v);
            }
            shift += 7;
            if shift > 63 {
                return Err(Error::invalid("a varint overflows 64 bits"));
            }
        }
    }

    pub fn varint(&mut self) -> Result<i64, Error> {
        let u = self.uvarint()?;
        Ok(((u >> 1) as i64) ^ -((u & 1) as i64))
    }

    pub fn byte(&mut self) -> Result<u8, Error> {
        let v = *self.b.get(self.at).ok_or_else(|| Error::invalid("a buffer ends early"))?;
        self.at += 1;
        Ok(v)
    }

    pub fn str(&mut self) -> Result<&'a str, Error> {
        let n = self.uvarint()?;
        let n = usize::try_from(n).map_err(|_| Error::invalid("a string length overflows"))?;
        if n > self.b.len() - self.at {
            return Err(Error::invalid("a buffer ends inside a string"));
        }
        let s = &self.b[self.at..self.at + n];
        self.at += n;
        std::str::from_utf8(s).map_err(|_| Error::invalid("a string is not UTF-8"))
    }

    pub fn fixed64(&mut self) -> Result<u64, Error> {
        if self.b.len() - self.at < 8 {
            return Err(Error::invalid("a buffer ends inside a fixed 64-bit value"));
        }
        let v = u64::from_le_bytes(self.b[self.at..self.at + 8].try_into().expect("8 bytes"));
        self.at += 8;
        Ok(v)
    }

    /// Fails unless every byte was read.
    pub fn finish(&self) -> Result<(), Error> {
        if self.at != self.b.len() {
            return Err(Error::invalid("a buffer carries trailing bytes"));
        }
        Ok(())
    }
}

pub fn put_uvarint(b: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        b.push((v as u8) | 0x80);
        v >>= 7;
    }
    b.push(v as u8);
}

pub fn put_varint(b: &mut Vec<u8>, v: i64) {
    put_uvarint(b, ((v << 1) ^ (v >> 63)) as u64);
}

pub fn put_str(b: &mut Vec<u8>, s: &str) {
    put_uvarint(b, s.len() as u64);
    b.extend_from_slice(s.as_bytes());
}
