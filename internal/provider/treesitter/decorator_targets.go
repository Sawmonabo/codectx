package treesitter

import (
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// keepParameterDecorators validates the decorators of a declaration's
// parameters and keeps each by its parameter's name token, for the
// parameter's variable node.
func (b *builder) keepParameterDecorators(pds []wire.ParameterDecorator) error {
	for _, pd := range pds {
		if _, err := b.rangeOf(pd.Parameter.Start, pd.Parameter.End); err != nil {
			return err
		}
		ds, err := b.decoratorList([]wire.Decorator{pd.Decorator})
		if err != nil {
			return err
		}
		if len(ds) == 0 {
			continue
		}
		if b.parameterDecorators == nil {
			b.parameterDecorators = map[wire.Span][]model.Decorator{}
		}
		b.parameterDecorators[pd.Parameter] = append(b.parameterDecorators[pd.Parameter], ds...)
	}
	return nil
}

// placeParameterDecorators adds to a variable's metadata the decorators of
// the parameter whose name token is v: a parameter decorator decorates the
// parameter, so it is on the parameter's node, not on its callable's.
func (b *builder) placeParameterDecorators(v wire.Span, meta map[string]any) map[string]any {
	ds := b.parameterDecorators[v]
	if len(ds) == 0 {
		return meta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta[model.MetaDecorators] = ds
	if b.placed == nil {
		b.placed = map[wire.Span]bool{}
	}
	b.placed[v] = true
	return meta
}

// unplacedParameterDecorators counts the decorators of each parameter no
// variable node was published for (its function's analysis failed, or the
// parameter is a pattern that declares no one name), so the file reports
// partial rather than dropping them in silence.
func (b *builder) unplacedParameterDecorators() {
	for v, ds := range b.parameterDecorators {
		if !b.placed[v] {
			b.dropped[structure] += len(ds)
		}
	}
}
