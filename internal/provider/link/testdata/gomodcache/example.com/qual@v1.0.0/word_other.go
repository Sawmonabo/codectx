//go:build !386 && !amd64

package qual

type Word = int8

type Regs struct{}

type Getter interface{ Get() int8 }

const N = 1
