package dep

type Named interface {
	Name() string
}

type Base struct{}

func (Base) Name() string { return "" }
