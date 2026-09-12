package cli

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Envelope is the single bounded output shape of Section 18.2. Every --json
// request emits exactly one of these on stdout, success or failure. The typed
// failure itself is model.Error: the Section 22 vocabulary is owned by the
// model package, which every service can import.
type Envelope[T any] struct {
	SchemaVersion string       `json:"schema_version"`
	Command       string       `json:"command"`
	OK            bool         `json:"ok"`
	Data          T            `json:"data"`
	Warnings      []string     `json:"warnings"`
	Error         *model.Error `json:"error"`
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

func failureEnvelope(schemaVersion, command string, err *model.Error) Envelope[any] {
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
		return &model.Error{Code: model.CodeInternal, Message: "failed to write JSON output: " + err.Error()}
	}
	return nil
}

// ExitCode maps a command failure to the process exit code table of Section
// 18.2. Only the process boundary acts on the result; no service exits.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var typed *model.Error
	if !errors.As(err, &typed) {
		// The command tree returns *model.Error from every Run path, so an
		// untyped error can only come from Cobra rejecting the flags or the
		// command name, which is the exit-2 class.
		return 2
	}
	switch typed.Code {
	case model.CodeArgumentInvalid:
		return 2
	case model.CodeWorkspaceNotFound, model.CodeConfigInvalid, model.CodeTrustRequired,
		model.CodePathEscape, model.CodeSchemaMismatch:
		return 3
	case model.CodeNoActiveGeneration:
		return 4
	case model.CodeProviderUnavailable, model.CodeProviderOutputInvalid, model.CodeProviderTimeout,
		model.CodeSnapshotUnstable, model.CodeSnapshotChanged, model.CodeSourceIntegrity,
		model.CodeStorageCorrupt:
		return 5
	case model.CodeSourceBindingUnverified, model.CodeCoverageIncomplete, model.CodeScopeIncomplete,
		model.CodeActorMismatch:
		return 6
	case model.CodeQueryTruncated, model.CodeQueryDeadline, model.CodeResourceLimit,
		model.CodeMinimumBudget, model.CodeDiskFull, model.CodeCanceled:
		// Cancellation belongs to the "explicit incomplete work" class: the
		// operator stopped the work themselves, so it is neither a defect nor
		// an invalid command line.
		return 7
	case model.CodeVersionConflict, model.CodeWorkspaceBusy, model.CodeSessionSuperseded,
		model.CodeSessionExpired, model.CodeCursorInvalid, model.CodeScopeChanged:
		return 8
	default:
		return 10
	}
}
