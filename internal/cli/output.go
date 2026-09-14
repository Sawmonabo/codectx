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

// successEnvelope builds the success shape. Warnings are the bounded,
// product-authored notes about work the command did not complete; the field is
// always an array, never null, so a consumer never has to distinguish the two.
func successEnvelope[T any](schemaVersion, command string, data T, warnings ...string) Envelope[T] {
	return Envelope[T]{
		SchemaVersion: schemaVersion,
		Command:       command,
		OK:            true,
		Data:          data,
		Warnings:      append([]string{}, warnings...),
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
		model.CodePathEscape, model.CodeSchemaMismatch, model.CodeToolOverrideInvalid:
		// An unusable [tools.override.<name>] is a configuration error the user
		// fixes in their own file, which is why Task 22 Step 3 separates it from
		// the rest of the CTX_TOOL_* family.
		return 3
	case model.CodeNoActiveGeneration:
		return 4
	case model.CodeProviderUnavailable, model.CodeProviderOutputInvalid, model.CodeProviderTimeout,
		model.CodeSnapshotUnstable, model.CodeSnapshotChanged, model.CodeSourceIntegrity,
		model.CodeStorageCorrupt,
		// A managed tool that cannot be resolved is a provider that cannot run,
		// so the whole CTX_TOOL_* family lands in the provider class (Task 22
		// Step 3). CTX_TOOL_UNSUPPORTED_PLATFORM only reaches here when a caller
		// asked for that tool by name; resolution treats it as honest absence.
		model.CodeToolOffline, model.CodeToolUnsupportedPlatform, model.CodeToolFetchFailed,
		model.CodeToolDigestMismatch, model.CodeToolCorrupt:
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
