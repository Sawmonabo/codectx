//go:build windows

package plat

type Handle interface {
	Fd() int
}

type Num = int

const Width = 2 + 2

type Sizer interface {
	Size() int
}

func (Tag) Sum() [Size]byte { return [Size]byte{} }
