//go:build !plat.v2

package plat

type Marshaler interface {
	Marshal() ([]byte, error)
}
