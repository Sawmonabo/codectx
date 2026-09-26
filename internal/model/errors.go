package model

import (
	"context"
	"errors"
	"fmt"
)

// Stable error families of Section 22. Every service raises one of these codes;
// internal/cli maps them to the Section 18.2 exit-code table. They live here
// because the model package is the only package every other one may import.
const (
	CodeWorkspaceNotFound   = "CTX_WORKSPACE_NOT_FOUND"
	CodeConfigInvalid       = "CTX_CONFIG_INVALID"
	CodeTrustRequired       = "CTX_TRUST_REQUIRED"
	CodePathEscape          = "CTX_PATH_ESCAPE"
	CodeSchemaMismatch      = "CTX_SCHEMA_MISMATCH"
	CodeSnapshotUnstable    = "CTX_SNAPSHOT_UNSTABLE"
	CodeSnapshotChanged     = "CTX_SNAPSHOT_CHANGED"
	CodeSourceIntegrity     = "CTX_SOURCE_INTEGRITY"
	CodeNoActiveGeneration  = "CTX_NO_ACTIVE_GENERATION"
	CodeProviderUnavailable = "CTX_PROVIDER_UNAVAILABLE"
	// CodeBinaryContent is content that is not text at all, so a capability
	// that only has meaning over text has nothing to report for it. It is
	// honest absence of a lexical view, not a provider that could not run,
	// which is why it is its own family and not a reuse of the one above.
	CodeBinaryContent           = "CTX_BINARY_CONTENT"
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

	// The managed-toolchain family of Section 11.7. CodeToolOffline is a tool
	// that is not installed while tools.offline is set, refused without opening
	// a socket. CodeToolUnsupportedPlatform is a lock entry with no payload for
	// the running platform: honest absence, not a failure.
	// CodeToolFetchFailed is a transport, status or truncation failure and is
	// retryable. CodeToolDigestMismatch is fetched bytes whose size or SHA-256
	// disagrees with the lock; it is never retried and never extracted.
	// CodeToolCorrupt is the pinned bytes being unusable rather than wrong -- a
	// malformed or hostile archive, or an installed tree whose entry executable
	// no longer hashes to the lock -- so a fresh install is the repair, which is
	// why it is a separate family from the digest mismatch.
	// CodeToolOverrideInvalid is a [tools.override.<name>] whose executable is
	// missing, is not a regular file, or does not hash to the declared checksum.
	CodeToolOffline             = "CTX_TOOL_OFFLINE"
	CodeToolUnsupportedPlatform = "CTX_TOOL_UNSUPPORTED_PLATFORM"
	CodeToolFetchFailed         = "CTX_TOOL_FETCH_FAILED"
	CodeToolDigestMismatch      = "CTX_TOOL_DIGEST_MISMATCH"
	CodeToolCorrupt             = "CTX_TOOL_CORRUPT"
	CodeToolOverrideInvalid     = "CTX_TOOL_OVERRIDE_INVALID"

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
	// incomplete work" class of the Section 18.2 table, alongside the other
	// bounds a caller can hit deliberately.
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

// DeadlineKey names the two settings that install a query deadline, for the
// detail of every CTX_QUERY_DEADLINE a bounded surface returns. The operator
// who set one is the only one who can see this error, so the error says which
// knob to move.
const DeadlineKey = "resources.query_timeout / --timeout"

// QueryDeadlineExceeded types an expired query deadline on a surface whose
// answer is ONE bounded read or one mutation: there is nothing to resume past,
// so the honest answer is the deadline itself, naming the setting that set it.
// A surface that accumulates work across passes or pages mints a continuation
// cursor instead and never reaches this.
//
// op names the request. err is joined so errors.Is still finds
// context.DeadlineExceeded below the typed code.
func QueryDeadlineExceeded(op string, err error) error {
	typed := (&Error{Code: CodeQueryDeadline, Retryable: true,
		Message:     "the " + op + " request did not finish within its query deadline",
		Remediation: "raise or clear the query deadline, or narrow the request"}).
		WithDetail("deadline", DeadlineKey)
	if err == nil {
		return typed
	}
	return errors.Join(typed, err)
}

// ClassifyQueryDeadline converts an expired deadline into the typed answer
// above and returns every other error unchanged, so a typed *Error from a
// dependency survives. It reads ctx as well as err, because a store call may
// report a deadline as a bare driver error.
func ClassifyQueryDeadline(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return QueryDeadlineExceeded(op, err)
	}
	return err
}

// Canceled types work the caller stopped: a CTX_CANCELED error joined with the
// context error, so errors.Is still distinguishes a deadline from a
// cancellation while errors.As finds the typed code. Section 22 requires that
// cancellation is never reported as a crash, and internal/cli maps an untyped
// error to the exit-2 usage class, which would call a deliberate stop an
// invalid command. A nil err is treated as context.Canceled.
func Canceled(err error) error {
	if err == nil {
		err = context.Canceled
	}
	typed := &Error{Code: CodeCanceled, Message: "the operation was canceled before it completed",
		Remediation: "run the operation again when it should finish"}
	if errors.Is(err, context.DeadlineExceeded) {
		typed.Message = "the caller's deadline expired before the operation completed"
	}
	return errors.Join(typed, err)
}
