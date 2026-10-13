// Package io is the slice of the standard library's io package the Go
// goldens read: its interfaces as the standard library declares them.
package io

type Reader interface {
	Read(p []byte) (n int, err error)
}

type Writer interface {
	Write(p []byte) (n int, err error)
}

type Closer interface {
	Close() error
}

type ReadWriter interface {
	Reader
	Writer
}

type ReadCloser interface {
	Reader
	Closer
}

// RuneScanner's unexported method makes it implementable only inside io.
type RuneScanner interface {
	ReadRune() (r rune, size int, err error)
	unreadRune() error
}
