package plat

type Count = int

const Size = 16

type Counter interface {
	Count() Num
}

type Grid interface {
	Cells() [Width]int
}

type Tag struct{}

type Regs struct{}
