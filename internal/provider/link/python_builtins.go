package link

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// pythonBuiltins are the callable names the builtins module declares
// (Python 3.13, the Library Reference's "Built-in Functions", "Built-in
// Constants" and "Built-in Exceptions"): the scope a name no local,
// enclosing or global scope binds is read from last (the Language
// Reference §4.2.2). A call of one that the file does not shadow is a call
// outside the project.
var pythonBuiltins = func() map[string]bool {
	out := map[string]bool{}
	for n := range strings.FieldsSeq(`
		abs aiter all anext any ascii bin bool breakpoint bytearray bytes callable chr classmethod compile
		complex copyright credits delattr dict dir divmod enumerate eval exec exit filter float format
		frozenset getattr globals hasattr hash help hex id input int isinstance issubclass iter len license
		list locals map max memoryview min next object oct open ord pow print property quit range repr
		reversed round set setattr slice sorted staticmethod str sum super tuple type vars zip __import__
		__build_class__
		BaseException BaseExceptionGroup GeneratorExit KeyboardInterrupt SystemExit Exception
		ArithmeticError FloatingPointError OverflowError ZeroDivisionError AssertionError AttributeError
		BufferError EOFError ExceptionGroup ImportError ModuleNotFoundError LookupError IndexError KeyError
		MemoryError NameError UnboundLocalError OSError EnvironmentError IOError BlockingIOError
		ChildProcessError ConnectionError BrokenPipeError ConnectionAbortedError ConnectionRefusedError
		ConnectionResetError FileExistsError FileNotFoundError InterruptedError IsADirectoryError
		NotADirectoryError PermissionError ProcessLookupError TimeoutError ReferenceError RuntimeError
		NotImplementedError PythonFinalizationError RecursionError StopAsyncIteration StopIteration
		SyntaxError IndentationError TabError SystemError TypeError ValueError UnicodeError
		UnicodeDecodeError UnicodeEncodeError UnicodeTranslateError Warning BytesWarning DeprecationWarning
		EncodingWarning FutureWarning ImportWarning PendingDeprecationWarning ResourceWarning RuntimeWarning
		SyntaxWarning UnicodeWarning UserWarning`) {
		out[n] = true
	}
	return out
}()

// pythonBuiltinCall reports a bare site of tab that calls a builtin: its
// callee is a builtin's name, no scope of the file the call can see binds
// it (visibleCandidates, an import), and no import of every member of a
// module may supply it.
func pythonBuiltinCall(tab *Table, s *Site) bool {
	if s.Shape != wire.ShapeBare || !pythonBuiltins[s.Callee] || len(visibleCandidates(tab, s)) > 0 {
		return false
	}
	for _, imp := range tab.Imports {
		if imp.Name == ImportAll {
			return false
		}
	}
	return true
}
