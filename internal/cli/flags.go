package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Flag names shared by every Section 18.1 query command. `--repo` is not here
// because it already has one spelling, declared by addRepoFlag.
const (
	queryGenerationFlag = "generation"
	queryLimitFlag      = "limit"
	queryCursorFlag     = "cursor"
	queryTimeoutFlag    = "timeout"
)

// addQueryFlags declares the flags every query command shares. --repo reuses
// the one spelling the rest of the tree already has. cursored says whether this
// command also declares --cursor: the generation help may only mention a flag
// the command actually has, so `path`, which issues no continuation, is not
// told about a combination it cannot make.
func addQueryFlags(cmd *cobra.Command, cursored bool) {
	addRepoFlag(cmd)
	generation := "answer from this generation instead of the active one (0 pins the active generation"
	if cursored {
		generation += "; not combinable with --cursor"
	}
	cmd.Flags().Int64(queryGenerationFlag, 0, generation+")")
	cmd.Flags().Duration(queryTimeoutFlag, 0, "give up after this much wall clock"+zeroBoundHelp)
}

// addLimitFlag declares --limit, for the commands whose request has a page.
// `path` has none: its routes are bounded by the reason-path cap.
func addLimitFlag(cmd *cobra.Command) {
	cmd.Flags().Int(queryLimitFlag, 0, "items in one page"+zeroBoundHelp)
}

// addCursorFlag declares --cursor, for the commands that actually MINT a
// continuation token. It is separate from addLimitFlag because a command can
// bound its page without being resumable: `path` returns its routes under the
// reason-path cap and prints no token, and offering the flag there would
// advertise a workflow the command refuses.
func addCursorFlag(cmd *cobra.Command) {
	// The refusal list is worded for BOTH families this flag serves. `search`
	// and `symbol` repin the cursor's own generation, so a newer one cannot
	// disturb them; the graph commands read the ACTIVE generation and refuse a
	// cursor that pins a different one (verifyContinuation in graph/cursor.go:
	// "cursor pins a generation that is no longer the one being read"), which is
	// a refusal the caller can hit without altering the token.
	cmd.Flags().String(queryCursorFlag, "", "continue a previous answer from the token it printed as next; the continuation stays on the generation that answer was read from, and is refused if the token has expired, was altered, was issued for a different query or command, or -- for the graph commands -- names a generation that is no longer the active one")
}

// pageRequest reads the page flags of a command that declares them. A command
// with --limit but no --cursor simply builds a page request with no cursor.
func pageRequest(cmd *cobra.Command) (model.PageRequest, error) {
	limit, err := intFlag(cmd, queryLimitFlag)
	if err != nil {
		return model.PageRequest{}, err
	}
	if cmd.Flags().Lookup(queryCursorFlag) == nil {
		return model.PageRequest{Limit: limit}, nil
	}
	cursor, err := stringFlag(cmd, queryCursorFlag)
	if err != nil {
		return model.PageRequest{}, err
	}
	return model.PageRequest{Limit: limit, Cursor: cursor}, nil
}

func generationFlag(cmd *cobra.Command) (model.GenerationID, error) {
	v, err := cmd.Flags().GetInt64(queryGenerationFlag)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return model.GenerationID(v), nil
}

func intFlag(cmd *cobra.Command, name string) (int, error) {
	v, err := cmd.Flags().GetInt(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func stringFlag(cmd *cobra.Command, name string) (string, error) {
	v, err := cmd.Flags().GetString(name)
	if err != nil {
		return "", &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	return v, nil
}

func durationFlag(cmd *cobra.Command, name string) (time.Duration, error) {
	v, err := cmd.Flags().GetDuration(name)
	if err != nil {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	if v < 0 {
		return 0, &model.Error{Code: model.CodeArgumentInvalid, Message: "--" + name + " must not be negative"}
	}
	return v, nil
}
