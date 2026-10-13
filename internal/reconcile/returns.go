package reconcile

// ReturnDisambiguator is a callable's return node's disambiguator: its
// callable's, then the return mark, so a call site in another file that
// reads the callable's result keeps naming the same node through any edit
// that keeps the callable. No parameter's disambiguator spells it, since a
// parameter's ends in its position.
func ReturnDisambiguator(callable string) string {
	return bounded(join(callable, "return"))
}
