//go:build amd64

package plat

func (Regs) PC() uint64 { return 0 }

func (Regs) SetPC(uint64) {}
