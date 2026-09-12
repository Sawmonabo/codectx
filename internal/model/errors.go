package model

import "fmt"

// Stable error families of Section 22. Every service raises one of these codes;
// internal/cli maps them to the Section 18.2 exit-code table. They live here
// because the model package is the only package every other one may import.
const (
	CodeWorkspaceNotFound       = "CTX_WORKSPACE_NOT_FOUND"
	CodeConfigInvalid           = "CTX_CONFIG_INVALID"
	CodeTrustRequired           = "CTX_TRUST_REQUIRED"
	CodePathEscape              = "CTX_PATH_ESCAPE"
	CodeSchemaMismatch          = "CTX_SCHEMA_MISMATCH"
	CodeSnapshotUnstable        = "CTX_SNAPSHOT_UNSTABLE"
	CodeSnapshotChanged         = "CTX_SNAPSHOT_CHANGED"
	CodeSourceIntegrity         = "CTX_SOURCE_INTEGRITY"
	CodeNoActiveGeneration      = "CTX_NO_ACTIVE_GENERATION"
	CodeProviderUnavailable     = "CTX_PROVIDER_UNAVAILABLE"
	CodeProviderOutputInvalid   = "CTX_PROVIDER_OUTPUT_INVALID"
	CodeProviderTimeout         = "CTX_PROVIDER_TIMEOUT"
	CodeSourceBindingUnverified = "CTX_SOURCE_BINDING_UNVERIFIED"
	CodeQueryTruncated          = "CTX_QUERY_TRUNCATED"
	CodeQueryDeadline           = "CTX_QUERY_DEADLINE"
	CodeResourceLimit           = "CTX_RESOURCE_LIMIT"
	CodeMinimumBudget           = "CTX_MINIMUM_BUDGET"
	CodeCoverageIncomplete      = "CTX_COVERAGE_INCOMPLETE"
	CodeScopeIncomplete         = "CTX_SCOPE_INCOMPLETE"
	CodeScopeChanged            = "CTX_SCOPE_CHANGED"
	CodeSessionSuperseded       = "CTX_SESSION_SUPERSEDED"
	CodeSessionExpired          = "CTX_SESSION_EXPIRED"
	CodeActorMismatch           = "CTX_ACTOR_MISMATCH"
	CodeCursorInvalid           = "CTX_CURSOR_INVALID"
	CodeVersionConflict         = "CTX_VERSION_CONFLICT"
	CodeWorkspaceBusy           = "CTX_WORKSPACE_BUSY"
	CodeDiskFull                = "CTX_DISK_FULL"
	CodeStorageCorrupt          = "CTX_STORAGE_CORRUPT"

	// CodeArgumentInvalid covers an invalid command, argument, input shape or
	// flag combination. Section 22 lists the families it "includes" and none of
	// them names the exit-2 class, so this code names it. Every Validate method
	// in this package reports structural rejections with it.
	CodeArgumentInvalid = "CTX_ARGUMENT_INVALID"
	// CodeCanceled is a run the caller stopped: a deliberate, complete-in-
	// itself outcome, not a failure. Section 22 requires cancellation not to be
	// reported as an unexpected crash and already names `canceled` as a run
	// state; its family list "includes" the codes it names, which is what lets
	// this one be named here. internal/cli maps it to exit 7, the "explicit
	// incomplete work" class of Section 18.2.
	CodeCanceled = "CTX_CANCELED"
	// CodeInternal is the exit-10 class: a defect, not a user-correctable input.
	CodeInternal = "CTX_INTERNAL"
)

// MaxErrorDetails bounds the structured detail map required by Section 22;
// details are diagnostic key/value pairs, never a source body or a secret.
const MaxErrorDetails = 16

// Error is the typed failure of Section 22: a stable code, a safe message,
// retryability, bounded structured details and user-readable remediation kept
// separate from the machine reason code.
type Error struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	Retryable   bool              `json:"retryable"`
	Details     map[string]string `json:"details,omitempty"`
	Remediation string            `json:"remediation,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// WithDetail attaches one bounded diagnostic pair. Detail values are truncated
// to MaxDetailBytes and the map is capped at MaxErrorDetails entries, so an
// error can never grow with the size of the input that produced it.
func (e *Error) WithDetail(key, value string) *Error {
	if key == "" {
		return e
	}
	if e.Details == nil {
		e.Details = make(map[string]string, 1)
	}
	if _, replacing := e.Details[key]; !replacing && len(e.Details) >= MaxErrorDetails {
		return e
	}
	e.Details[key] = truncateUTF8(value, MaxDetailBytes)
	return e
}

// WithRemediation attaches bounded user-readable remediation text.
func (e *Error) WithRemediation(text string) *Error {
	e.Remediation = truncateUTF8(text, MaxDetailBytes)
	return e
}

// invalid builds the CTX_ARGUMENT_INVALID rejection used by every validator.
func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}
