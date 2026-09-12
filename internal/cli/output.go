package cli

import (
	"encoding/json"
	"errors"
	"io"
)

// Error is the typed failure carried by the JSON envelope and returned by every
// command path. Section 22 requires a stable code, a safe message and
// retryability; bounded details and remediation are added by the services that
// own them.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Error codes used by this package. The full Section 22 vocabulary is owned by
// the services that raise it; ExitCode already maps every family.
const (
	// CodeArgumentInvalid covers an invalid command, argument, input shape or
	// flag combination. Section 22 lists error families it "includes", and no
	// listed family names the exit-2 class, so this code names it.
	CodeArgumentInvalid = "CTX_ARGUMENT_INVALID"
	CodeInternal        = "CTX_INTERNAL"
)

// Envelope is the single bounded output shape of Section 18.2. Every --json
// request emits exactly one of these on stdout, success or failure.
type Envelope[T any] struct {
	SchemaVersion string   `json:"schema_version"`
	Command       string   `json:"command"`
	OK            bool     `json:"ok"`
	Data          T        `json:"data"`
	Warnings      []string `json:"warnings"`
	Error         *Error   `json:"error"`
}

func successEnvelope[T any](schemaVersion, command string, data T) Envelope[T] {
	return Envelope[T]{
		SchemaVersion: schemaVersion,
		Command:       command,
		OK:            true,
		Data:          data,
		Warnings:      []string{},
	}
}

func failureEnvelope(schemaVersion, command string, err *Error) Envelope[any] {
	return Envelope[any]{
		SchemaVersion: schemaVersion,
		Command:       command,
		Warnings:      []string{},
		Error:         err,
	}
}

// writeEnvelope emits one envelope. A write failure (a closed stdout pipe, for
// example) fails the command rather than being reported as success.
func writeEnvelope[T any](w io.Writer, env Envelope[T]) error {
	if err := json.NewEncoder(w).Encode(env); err != nil {
		return &Error{Code: CodeInternal, Message: "failed to write JSON output: " + err.Error()}
	}
	return nil
}

// ExitCode maps a command failure to the process exit code table of Section
// 18.2. Only the process boundary acts on the result; no service exits.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var typed *Error
	if !errors.As(err, &typed) {
		// The command tree returns *Error from every Run path, so an untyped
		// error can only come from Cobra rejecting the flags or the command
		// name, which is the exit-2 class.
		return 2
	}
	switch typed.Code {
	case CodeArgumentInvalid:
		return 2
	case "CTX_WORKSPACE_NOT_FOUND", "CTX_CONFIG_INVALID", "CTX_TRUST_REQUIRED",
		"CTX_PATH_ESCAPE", "CTX_SCHEMA_MISMATCH":
		return 3
	case "CTX_NO_ACTIVE_GENERATION":
		return 4
	case "CTX_PROVIDER_UNAVAILABLE", "CTX_PROVIDER_OUTPUT_INVALID", "CTX_PROVIDER_TIMEOUT",
		"CTX_SNAPSHOT_UNSTABLE", "CTX_SNAPSHOT_CHANGED", "CTX_SOURCE_INTEGRITY", "CTX_STORAGE_CORRUPT":
		return 5
	case "CTX_SOURCE_BINDING_UNVERIFIED", "CTX_COVERAGE_INCOMPLETE", "CTX_SCOPE_INCOMPLETE",
		"CTX_ACTOR_MISMATCH":
		return 6
	case "CTX_QUERY_TRUNCATED", "CTX_QUERY_DEADLINE", "CTX_RESOURCE_LIMIT",
		"CTX_MINIMUM_BUDGET", "CTX_DISK_FULL":
		return 7
	case "CTX_VERSION_CONFLICT", "CTX_WORKSPACE_BUSY", "CTX_SESSION_SUPERSEDED",
		"CTX_SESSION_EXPIRED", "CTX_CURSOR_INVALID", "CTX_SCOPE_CHANGED":
		return 8
	default:
		return 10
	}
}
