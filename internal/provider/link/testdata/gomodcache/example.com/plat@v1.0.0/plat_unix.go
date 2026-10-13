//go:build unix

package plat

type Handle interface {
	Fd() uintptr
}

type Num = Count

const Width = 4

type Sizer interface {
	Size() Count
}

func (Tag) Sum() [16]byte { return [16]byte{} }
