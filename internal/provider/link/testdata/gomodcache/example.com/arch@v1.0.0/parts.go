package arch

type Lo struct{}

func (Lo) Narrow() {}

type Hi struct{}

func (Hi) Wide() {}
