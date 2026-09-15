package lsp

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestSymbolFieldsAreBoundedAndTheCutIsRecorded protects the R5 contract at
// this boundary. The storage ceilings no longer reject an over-long value, so
// a producer that does not truncate serves the server's whole string into a
// model record -- a 3000-byte detail becomes a 3000-byte Signature. Every
// Symbol in this package is built by newSymbol, so bounding there covers the
// document-symbol, workspace-symbol and call-hierarchy paths at once.
func TestSymbolFieldsAreBoundedAndTheCutIsRecorded(t *testing.T) {
	name := strings.Repeat("n", model.MaxNameBytes+7)
	detail := strings.Repeat("d", 3000)
	sym := newSymbol(name, detail, name, model.NodeFunction, Location{Path: "main.go"})

	if len(sym.Name) != model.MaxNameBytes || len(sym.Container) != model.MaxNameBytes {
		t.Fatalf("name %d bytes, container %d bytes, want both bounded to %d", len(sym.Name), len(sym.Container), model.MaxNameBytes)
	}
	if len(sym.Detail) > model.MaxSignatureBytes {
		t.Fatalf("detail is %d bytes; a signature over %d reaches the record whole", len(sym.Detail), model.MaxSignatureBytes)
	}
	if got := sym.TruncatedFields["name"]; got != len(name) {
		t.Fatalf("truncated_fields[name] = %d, want the original length %d", got, len(name))
	}
	if got := sym.TruncatedFields["container"]; got != len(name) {
		t.Fatalf("truncated_fields[container] = %d, want the original length %d", got, len(name))
	}
	if _, cut := sym.TruncatedFields["signature"]; cut {
		t.Fatalf("a %d-byte detail under the %d-byte ceiling was reported as cut", len(detail), model.MaxSignatureBytes)
	}

	long := strings.Repeat("s", model.MaxSignatureBytes+1)
	if sym = newSymbol("f", long, "", model.NodeFunction, Location{}); sym.TruncatedFields["signature"] != len(long) {
		t.Fatalf("truncated_fields[signature] = %d, want the original length %d", sym.TruncatedFields["signature"], len(long))
	}
}
