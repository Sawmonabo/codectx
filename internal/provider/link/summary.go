package link

// The incremental relink's vocabulary (ADR-0014 decisions 2 and 3). Each
// file's table has two summaries, its interface and its own sites, each with
// its own digest. The interface is a set of entries, every one something
// another file's resolution reads; a refresh diffs it entry by entry. Every
// site's resolution records a trace: each key it read, with that key's hash
// at the time, so a refresh reruns exactly the sites whose reads changed.

// EntryKind is what one interface entry describes.
type EntryKind uint8

const (
	// EntryDeclaration is a declaration and its signature.
	EntryDeclaration EntryKind = 1
	// EntrySupertype is one declared subtype relation.
	EntrySupertype EntryKind = 2
	// EntryExport is one name the file exports or re-exports.
	EntryExport EntryKind = 3
	// EntryImport is one import of the file.
	EntryImport EntryKind = 4
	// EntryModule is the file's module identity: its package, module or
	// namespace.
	EntryModule EntryKind = 5
	// EntryInferred is one function's inferred return and parameter values,
	// where resolution follows values.
	EntryInferred EntryKind = 6
	// EntryFieldWrite is the values the file writes to one field, where
	// resolution is field-based.
	EntryFieldWrite EntryKind = 7
)

// Valid reports whether k is a known entry kind.
func (k EntryKind) Valid() bool { return k >= EntryDeclaration && k <= EntryFieldWrite }

// SummaryEntry is one entry of a file's interface summary. Key is the entry's
// identity within the file, stable across edits that do not change it (a
// declaration's descriptor path and disambiguator, an export's name, a
// field's name); Hash is the digest of its content. Two summaries differ
// exactly in the entries whose Key is in one only or whose Hash differs.
type SummaryEntry struct {
	Kind EntryKind
	Key  string
	Hash string
}

// TraceKeyKind is what one recorded read looked up.
type TraceKeyKind uint8

const (
	// TraceScopeLookup is a name looked up in a scope: the result is what
	// that scope declares under the name, so a declaration added or removed
	// under it changes the read.
	TraceScopeLookup TraceKeyKind = 1
	// TraceInterfaceEntry is one entry of another file's interface summary.
	TraceInterfaceEntry TraceKeyKind = 2
	// TraceInferredSummary is one function's inferred values.
	TraceInferredSummary TraceKeyKind = 3
	// TraceManifestField is one field of a manifest's linker-read
	// projection.
	TraceManifestField TraceKeyKind = 4
)

// Valid reports whether k is a known trace key kind.
func (k TraceKeyKind) Valid() bool { return k >= TraceScopeLookup && k <= TraceManifestField }

// TraceKey names one read a site's resolution made. Scope is where the read
// looked: the scope, file or manifest; Name is what it looked up there: the
// name, the interface entry's Key, the function's descriptor, the manifest
// field.
type TraceKey struct {
	Kind  TraceKeyKind
	Scope string
	Name  string
}

// TraceRead is one recorded read: the key and its hash when the site read it.
// A site reruns when any of its reads now hashes differently.
type TraceRead struct {
	Key  TraceKey
	Hash string
}
