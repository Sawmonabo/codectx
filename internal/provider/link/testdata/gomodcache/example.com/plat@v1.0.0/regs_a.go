//go:build 386

package plat

func (Regs) PC() uint32 { return 0 }

func (Regs) SetPC(uint32) {}
