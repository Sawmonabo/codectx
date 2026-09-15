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
	queryDirectionFlag  = "direction"
)

// addQueryFlags declares the flags every query command shares. --repo reuses
// the one spelling the rest of the tree already has. cursored says whether this
// command also declares --cursor: the generation help may only mention a flag
// the command actually has, so a command that issues no continuation is not
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
// bound its page without being resumable, and `path` is the reverse: it is
// resumable but takes no --limit, because a route set is bounded by the
// reason-path cap rather than paged.
func addCursorFlag(cmd *cobra.Command) {
	// The repeat-every-flag clause is the first half of the help because it is
	// the refusal an operator actually hits: the request's direction, relation
	// kinds, seeds, depth and page size are hashed into the token, so `--cursor`
	// alone -- the shape a paging loop naturally takes -- is refused as a
	// different query. It names no individual flag, for the reason
	// addQueryFlags takes a `cursored` argument: this help is shared by
	// `search`, `symbol` and the `context` commands, which have no --depth, and
	// help that names a flag its command does not declare is worse than help
	// that names none. verifyContinuation names the inputs in the message it
	// returns, and repo-map's own Long text names its two.
	//
	// The refusal list is worded for BOTH families this flag serves. `search`
	// and `symbol` repin the cursor's own generation, so a newer one cannot
	// disturb them; the graph commands read the ACTIVE generation and refuse a
	// cursor that pins a different one (verifyContinuation in graph/cursor.go:
	// "cursor pins a generation that is no longer the one being read"), which is
	// a refusal the caller can hit without altering the token.
	cmd.Flags().String(queryCursorFlag, "", "continue a previous answer from the token it printed as next; repeat every other flag and argument of the original request unchanged, because a token is bound to the question it was issued for and a page asked with a different one is refused rather than answered; the continuation stays on the generation that answer was read from, and is refused if the token has expired, was altered, was issued for a different query or command, or -- for the graph commands -- names a generation that is no longer the active one")
}

// pageRequest reads the page flags of a command that declares them. A command
// with --limit but no --cursor simply builds a page request with no cursor.
func pageRequest(cmd *cobra.Command) (model.PageRequest, error) {
	// Both reads are Lookup-guarded, because the two flags are declared
	// independently: `path` declares --cursor and no --limit (a route set is
	// bounded by the reason-path cap, not paged), and reading a flag the
	// command never registered is a cobra error that kills the invocation
	// before it reaches the workspace. A missing --limit leaves Limit zero,
	// which is "take the configured page bound".
	var limit int
	if cmd.Flags().Lookup(queryLimitFlag) != nil {
		v, err := intFlag(cmd, queryLimitFlag)
		if err != nil {
			return model.PageRequest{}, err
		}
		limit = v
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
