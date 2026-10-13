package model

// InputSource is the part of a request a request-input read takes its
// value from.
type InputSource string

const (
	InputQuery  InputSource = "query"
	InputBody   InputSource = "body"
	InputPath   InputSource = "path"
	InputHeader InputSource = "header"
	InputCookie InputSource = "cookie"
	InputForm   InputSource = "form"
	InputFile   InputSource = "file"
	InputURL    InputSource = "url"
	InputRaw    InputSource = "raw"
	// InputCombined is a read the framework answers from several parts at
	// once: Flask's `request.values` (the query and the form), Express's
	// `req.param(name)` (the path, the body and the query), Go's
	// `FormValue` and `Form`, Spring's `@RequestParam`.
	InputCombined InputSource = "combined"
	// InputRequest is the request object itself handed to code that reads
	// it (a Django view's request parameter): every part is reachable from
	// it.
	InputRequest InputSource = "request"
)

// Valid reports whether s is a known input source.
func (s InputSource) Valid() bool {
	switch s {
	case InputQuery, InputBody, InputPath, InputHeader, InputCookie, InputForm, InputFile, InputURL, InputRaw,
		InputCombined, InputRequest:
		return true
	}
	return false
}

// MetaInput is the metadata key of a request-input read on its node.
const MetaInput = "input"

// RequestInput is a read of the network request a framework hands its
// handler: the framework whose model names the read, the part of the
// request it reads, and the key it reads when the source writes it as a
// literal (`request.args.get("id")` reads "id"). It is the metadata
// MetaInput of a `variable` node at the reading expression, which the
// enclosing function `reads`.
type RequestInput struct {
	Framework string      `json:"framework"`
	Source    InputSource `json:"source"`
	Key       string      `json:"key,omitempty"`
}

// Validate enforces the request-input shape.
func (r RequestInput) Validate() error {
	if err := requireField("input.framework", r.Framework, MaxIdentifierBytes); err != nil {
		return err
	}
	if !r.Source.Valid() {
		return invalid("input.source %q is not a known input source", truncateForMessage(string(r.Source)))
	}
	if err := boundField("input.key", r.Key, MaxNameBytes); err != nil {
		return err
	}
	return nil
}
