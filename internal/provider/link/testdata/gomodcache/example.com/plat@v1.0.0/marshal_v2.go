//go:build plat.v2

package plat

type Marshaler = Encoder

type Encoder interface {
	Marshal() ([]byte, error)
}
