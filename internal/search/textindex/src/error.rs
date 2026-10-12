//! Failures as the header's status codes.

use std::io;

use tantivy::directory::error::{LockError, OpenDirectoryError, OpenReadError, OpenWriteError};
use tantivy::TantivyError;

pub const CX_OK: i32 = 0;
pub const CX_INVALID: i32 = 1;
pub const CX_BUSY: i32 = 2;
pub const CX_CANCELED: i32 = 3;
pub const CX_CORRUPT: i32 = 4;
pub const CX_DISK_FULL: i32 = 5;
pub const CX_INTERNAL: i32 = 6;
pub const CX_TOO_MANY_TERMS: i32 = 7;
pub const CX_NO_POSITION: i32 = 8;

/// A failure: its status code and the message written into *err.
#[derive(Debug)]
pub struct Error {
    pub status: i32,
    pub msg: String,
}

impl Error {
    pub fn new(status: i32, msg: impl Into<String>) -> Error {
        Error { status, msg: msg.into() }
    }
    pub fn invalid(msg: impl Into<String>) -> Error {
        Error::new(CX_INVALID, msg)
    }
    pub fn internal(msg: impl Into<String>) -> Error {
        Error::new(CX_INTERNAL, msg)
    }
    pub fn corrupt(msg: impl Into<String>) -> Error {
        Error::new(CX_CORRUPT, msg)
    }
}

/// The status an io error maps to: a full filesystem is CX_DISK_FULL.
fn io_status(e: &io::Error) -> i32 {
    if e.kind() == io::ErrorKind::StorageFull || is_enospc(e) {
        CX_DISK_FULL
    } else {
        CX_INTERNAL
    }
}

#[cfg(unix)]
fn is_enospc(e: &io::Error) -> bool {
    // ENOSPC is 28 on every Unix codectx builds for.
    e.raw_os_error() == Some(28)
}

#[cfg(windows)]
fn is_enospc(e: &io::Error) -> bool {
    // ERROR_HANDLE_DISK_FULL and ERROR_DISK_FULL.
    matches!(e.raw_os_error(), Some(39) | Some(112))
}

#[cfg(not(any(unix, windows)))]
fn is_enospc(_: &io::Error) -> bool {
    false
}

impl From<io::Error> for Error {
    fn from(e: io::Error) -> Error {
        let status = if e.kind() == io::ErrorKind::InvalidData { CX_CORRUPT } else { io_status(&e) };
        Error::new(status, e.to_string())
    }
}

impl From<TantivyError> for Error {
    fn from(e: TantivyError) -> Error {
        let msg = e.to_string();
        let status = match &e {
            TantivyError::LockFailure(LockError::LockBusy, _) => CX_BUSY,
            TantivyError::LockFailure(LockError::IoError(io), _) => io_status(io),
            TantivyError::IoError(io) => {
                if io.kind() == io::ErrorKind::InvalidData {
                    CX_CORRUPT
                } else {
                    io_status(io)
                }
            }
            TantivyError::OpenWriteError(OpenWriteError::IoError { io_error, .. }) => io_status(io_error),
            TantivyError::OpenReadError(OpenReadError::IoError { io_error, .. }) => io_status(io_error),
            TantivyError::OpenReadError(OpenReadError::FileDoesNotExist(_)) => CX_CORRUPT,
            TantivyError::OpenReadError(OpenReadError::IncompatibleIndex(_)) => CX_CORRUPT,
            TantivyError::OpenDirectoryError(OpenDirectoryError::IoError { io_error, .. }) => io_status(io_error),
            TantivyError::DataCorruption(_) | TantivyError::IncompatibleIndex(_) => CX_CORRUPT,
            TantivyError::SchemaError(_) | TantivyError::FieldNotFound(_) => CX_CORRUPT,
            TantivyError::InvalidArgument(_) => CX_INVALID,
            _ => CX_INTERNAL,
        };
        Error::new(status, msg)
    }
}

pub type Result<T> = std::result::Result<T, Error>;
