//go:build unix

package plat

type Handle interface {
	Fd() uintptr
}
