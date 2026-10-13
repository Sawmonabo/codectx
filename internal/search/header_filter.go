package search

import (
	"path"
	"strings"

	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// decidedHeaders answers, for each extension several grammars declare (a
// shared header, tslang.SharedHeader), the language filter's verdict when it
// is the same whatever grammar a header's parse kept: a header's tag is always
// one of the grammars that declare its extension (the kept parse's grammar, or
// the census's first choice when the generation holds no parse), so a filter
// naming every one of them keeps every such header, and a filter naming none
// of them keeps none. Those headers are then decided with no lookup. An
// extension the filter names some but not all of the grammars of is absent:
// its headers are tagged by the bound tagger. all reports that every shared
// extension is decided, so nothing the filter can meet needs the tagger's
// binding.
func decidedHeaders(languages map[string]bool) (decided map[string]bool, all bool) {
	declaring := map[string][]string{}
	for _, l := range tslang.All {
		for _, ext := range l.Extensions {
			declaring[ext] = append(declaring[ext], l.Name)
		}
	}
	decided = map[string]bool{}
	all = true
	for ext, names := range declaring {
		if len(names) < 2 {
			continue
		}
		named := 0
		for _, n := range names {
			if languages[n] {
				named++
			}
		}
		switch named {
		case len(names):
			decided[ext] = true
		case 0:
			decided[ext] = false
		default:
			all = false
		}
	}
	return decided, all
}

// keepsLanguage reports whether the language filter keeps a path: a shared
// header the filter decides without its tag (decidedHeaders) costs no lookup;
// every other path is tagged.
func (f hitFilter) keepsLanguage(p string) bool {
	if tslang.SharedHeader(p) {
		if keep, ok := f.headers[strings.ToLower(path.Ext(p))]; ok {
			return keep
		}
	}
	return f.languages[f.tag.Of(p)]
}
