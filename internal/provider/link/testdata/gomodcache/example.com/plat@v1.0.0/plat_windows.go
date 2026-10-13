//go:build windows

package plat

type Handle interface {
	Fd() int
}
