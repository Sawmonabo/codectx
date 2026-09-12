package lsp

import (
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The subset of the Language Server Protocol 3.17 this client speaks. Only the
// fields the overlay reads or sends are declared; everything else a server
// sends is decoded into json.RawMessage or ignored. Positions here are LSP
// positions: zero-based lines and columns counted in the negotiated encoding.
// They never leave this package; every public coordinate is a byte-
// authoritative model.SourceRange converted against the pinned bytes.

type position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

// locationLink is the richer definition result. TargetSelectionRange is the
// identifier itself; TargetRange is the whole declaration.
type locationLink struct {
	TargetURI            string    `json:"targetUri"`
	TargetRange          lspRange  `json:"targetRange"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange,omitempty"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

type textDocumentPositionParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     position               `json:"position"`
}

type referenceParams struct {
	textDocumentPositionParams
	Context referenceContext `json:"context"`
}

type referenceContext struct {
	IncludeDeclaration bool `json:"includeDeclaration"`
}

type documentSymbolParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

type workspaceSymbolParams struct {
	Query string `json:"query"`
}

type didOpenParams struct {
	TextDocument textDocumentItem `json:"textDocument"`
}

type textDocumentItem struct {
	URI        string `json:"uri"`
	LanguageID string `json:"languageId"`
	Version    int    `json:"version"`
	Text       string `json:"text"`
}

type didCloseParams struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
}

// documentSymbol is the hierarchical result shape; symbolInformation is the
// flat one. Servers return one or the other for textDocument/documentSymbol.
type documentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          lspRange         `json:"range"`
	SelectionRange lspRange         `json:"selectionRange"`
	Children       []documentSymbol `json:"children,omitempty"`
}

type symbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// workspaceSymbol may carry a location without a range when the client
// advertises resolve support. This client does not, so a range is required.
type workspaceSymbol struct {
	Name          string          `json:"name"`
	Kind          int             `json:"kind"`
	Location      json.RawMessage `json:"location"`
	ContainerName string          `json:"containerName,omitempty"`
}

// callHierarchyItem is echoed back to the server for incoming/outgoing calls,
// so the raw item is retained verbatim including its opaque data field.
type callHierarchyItem struct {
	Name           string          `json:"name"`
	Kind           int             `json:"kind"`
	Detail         string          `json:"detail,omitempty"`
	URI            string          `json:"uri"`
	Range          lspRange        `json:"range"`
	SelectionRange lspRange        `json:"selectionRange"`
	Data           json.RawMessage `json:"data,omitempty"`
}

type callHierarchyCallsParams struct {
	Item json.RawMessage `json:"item"`
}

// An incoming or outgoing call is decoded in overlay.calls with the peer item
// kept as a json.RawMessage, so that the item can be echoed back verbatim.
// There is no typed call edge here, because a typed one would discard the raw
// item.

// initializeParams declares exactly what this client can do. Position
// encodings are listed in preference order; UTF-16 is last because the
// protocol makes it mandatory, not because it is preferred.
type initializeParams struct {
	ProcessID             *int               `json:"processId"`
	ClientInfo            clientInfo         `json:"clientInfo"`
	RootURI               string             `json:"rootUri"`
	WorkspaceFolders      []workspaceFolder  `json:"workspaceFolders"`
	Capabilities          clientCapabilities `json:"capabilities"`
	InitializationOptions json.RawMessage    `json:"initializationOptions,omitempty"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type clientCapabilities struct {
	General      generalCapabilities      `json:"general"`
	TextDocument textDocumentCapabilities `json:"textDocument"`
	Workspace    workspaceCapabilities    `json:"workspace"`
}

type generalCapabilities struct {
	PositionEncodings []string `json:"positionEncodings"`
}

type textDocumentCapabilities struct {
	Definition     linkCapability           `json:"definition"`
	TypeDefinition linkCapability           `json:"typeDefinition"`
	Implementation linkCapability           `json:"implementation"`
	References     struct{}                 `json:"references"`
	DocumentSymbol documentSymbolCapability `json:"documentSymbol"`
	CallHierarchy  struct{}                 `json:"callHierarchy"`
}

type linkCapability struct {
	LinkSupport bool `json:"linkSupport"`
}

type documentSymbolCapability struct {
	HierarchicalDocumentSymbolSupport bool `json:"hierarchicalDocumentSymbolSupport"`
}

type workspaceCapabilities struct {
	Configuration    bool     `json:"configuration"`
	WorkspaceFolders bool     `json:"workspaceFolders"`
	Symbol           struct{} `json:"symbol"`
}

type initializeResult struct {
	Capabilities serverCapabilities `json:"capabilities"`
	ServerInfo   *serverInfo        `json:"serverInfo,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// serverCapabilities keeps each provider field raw: the protocol allows a
// boolean or an options object for every one of them.
type serverCapabilities struct {
	PositionEncoding        string          `json:"positionEncoding,omitempty"`
	TextDocumentSync        json.RawMessage `json:"textDocumentSync,omitempty"`
	DefinitionProvider      json.RawMessage `json:"definitionProvider,omitempty"`
	TypeDefinitionProvider  json.RawMessage `json:"typeDefinitionProvider,omitempty"`
	ImplementationProvider  json.RawMessage `json:"implementationProvider,omitempty"`
	ReferencesProvider      json.RawMessage `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider  json.RawMessage `json:"documentSymbolProvider,omitempty"`
	WorkspaceSymbolProvider json.RawMessage `json:"workspaceSymbolProvider,omitempty"`
	CallHierarchyProvider   json.RawMessage `json:"callHierarchyProvider,omitempty"`
}

// provided reports whether a raw provider capability is enabled: true, or any
// options object. false, null and absence all mean unsupported.
func provided(raw json.RawMessage) bool {
	switch {
	case len(raw) == 0, string(raw) == "null", string(raw) == "false":
		return false
	}
	return true
}

// opensDocuments reports whether the server accepts didOpen/didClose: a sync
// kind other than None, or options with openClose set.
func opensDocuments(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var kind int
	if json.Unmarshal(raw, &kind) == nil {
		return kind != 0
	}
	var opts struct {
		OpenClose bool `json:"openClose"`
	}
	return json.Unmarshal(raw, &opts) == nil && opts.OpenClose
}

// configurationParams is the one server request this client answers.
type configurationParams struct {
	Items []json.RawMessage `json:"items"`
}

// maxConfigurationItems bounds the answer to workspace/configuration. A
// server asking for more sections than this is not asking for settings.
const maxConfigurationItems = 64

// nodeKindOf maps an LSP SymbolKind to the Section 9.2 vocabulary. LSP has
// value kinds (string, number, boolean, array, object, key, null) that the
// model treats as variables, and constructors and operators that it treats as
// methods and functions; the mapping is documented in docs/providers-lsp.md.
func nodeKindOf(kind int) model.NodeKind {
	switch kind {
	case 1:
		return model.NodeFile
	case 2:
		return model.NodeModule
	case 3:
		return model.NodeNamespace
	case 4:
		return model.NodePackage
	case 5:
		return model.NodeClass
	case 6, 9:
		return model.NodeMethod
	case 7, 8, 24:
		return model.NodeField
	case 10:
		return model.NodeEnum
	case 11:
		return model.NodeInterface
	case 12, 25:
		return model.NodeFunction
	case 14, 22:
		return model.NodeConstant
	case 23:
		return model.NodeStruct
	default:
		// 13 variable, 15–21 value kinds, 26 type parameter and anything
		// this client does not know are variables: honest about being a named
		// entity, never a fabricated declaration kind.
		return model.NodeVariable
	}
}
