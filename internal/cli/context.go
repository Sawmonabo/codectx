package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/coverage"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// Flag spellings for the Section 18.1 `context` commands. As everywhere else in
// this package, a command declares only the flags its own request type has a
// field for. In particular none of the five declares --generation: a session
// pins its own generation and snapshot when it is opened, so a flag that
// claimed to choose one would be help text describing a request none of these
// commands can build.
const (
	contextActorFlag           = "actor"
	contextOffsetFlag          = "offset"
	contextMaxBytesFlag        = "max-bytes"
	contextConfirmReceiptFlag  = "confirm-receipt"
	contextReceiptFlag         = "receipt"
	contextFileReviewFlag      = "file-review"
	contextExpectedVersionFlag = "expected-version"
)

// actorFlagHelp is the one sentence every command in this group repeats.
// Section 16.1 scopes coverage to one actor: a parent's receipts never satisfy
// a subagent's requirement, so the actor is part of the request rather than
// something the process identity may stand in for.
const actorFlagHelp = "required: the actor this session belongs to; coverage is per actor, so one agent's reads never satisfy another's requirement"

// newContextCommands builds the Section 18.1 session commands. Like the query
// commands they are returned as a set, so this file never edits the command
// tree it belongs to; the root adds them in one loop.
//
// Task 18 adds the remaining `context` verbs (plan, entries, include, waive,
// record, advance, capsule, export). The five here are the Section 16.2/16.3
// read-and-coverage surface.
func newContextCommands(build model.BuildInfo) []*cobra.Command {
	root := &cobra.Command{
		Use:   "context",
		Short: "Read snapshot-pinned source in bounded chunks and track what was delivered",
		Long: "Serves the source of one context session in bounded, lossless chunks and keeps " +
			"per-actor coverage of what was actually delivered.\n\n" +
			"Bytes earn coverage only when the signed receipt the read issued is echoed back and " +
			"accepted, so a chunk that was issued but never confirmed -- a broken pipe, a dropped " +
			"connection, an agent that stopped reading -- grants no credit at all.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonRequested(cmd, args) {
				return &model.Error{Code: model.CodeArgumentInvalid,
					Message: "no context subcommand given; run \"codectx context --help\" for the list"}
			}
			return cmd.Help()
		},
	}
	root.AddCommand(
		newContextReadCommand(build),
		newContextAcknowledgeCommand(build),
		newContextStatusCommand(build),
		newContextNextCommand(build),
		newContextCloseCommand(build),
	)
	return []*cobra.Command{root}
}

// newContextReadCommand builds `codectx context read`.
//
// The delivery-confirmation ordering of Section 16.3 is structural here rather
// than checked: the receipt this command prints is never confirmed by this
// command. The only confirming input is --confirm-receipt, whose tokens ride in
// the request and are confirmed inside the service before any byte is
// serialized, and there is no service call of any kind after the output is
// written. A failed write therefore fails the command with nothing confirmed --
// a successful write is never treated as delivery.
func newContextReadCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "read <session-id> <file-id>",
		Short: "Read one bounded chunk of a session's pinned source",
		Long: "Reads one chunk of one file as the session pinned it, never as the working tree " +
			"holds it now. Chunk boundaries are lossless: CRLF endings and a final line without a " +
			"newline are preserved byte for byte, a line longer than the chunk budget is split and " +
			"reported as partial rather than truncated, and a file whose bytes are not valid UTF-8 " +
			"is returned base64-encoded rather than replaced with substitution characters.\n\n" +
			"The answer carries a signed receipt. The receipt is what earns coverage: echo it back " +
			"with --confirm-receipt on a later read, or with `context acknowledge --receipt`, or " +
			"the bytes count as undelivered. Printing a chunk is not delivery.\n\n" +
			"Both arguments are resolved ids of " + fmt.Sprintf("%d", model.IDHexLen) + " lowercase hex characters: " +
			"the session `context plan` opened and a file id from `context status` or `context next`.",
		Args:          cobra.ExactArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The whole request is screened before a workspace is opened: a
			// rejection that first opened the database paid for work it will
			// not do. Validate() is also the actor gate -- storage skips its
			// actor check for an empty actor, so an unvalidated request would
			// read another agent's session.
			if err := checkContextIDs(args); err != nil {
				return err
			}
			actor, err := stringFlag(cmd, contextActorFlag)
			if err != nil {
				return err
			}
			offset, err := contextOffsetValue(cmd)
			if err != nil {
				return err
			}
			maxBytes, err := contextMaxBytesValue(cmd)
			if err != nil {
				return err
			}
			receipts, err := stringsFlag(cmd, contextConfirmReceiptFlag)
			if err != nil {
				return err
			}
			req := model.ReadChunkRequest{
				SessionID:       model.SessionID(args[0]),
				ActorID:         actor,
				FileID:          model.FileID(args[1]),
				Offset:          offset,
				MaxBytes:        maxBytes,
				ConfirmReceipts: receipts,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var resp model.ReadChunkResponse
			if err := runContext(cmd, func(ctx context.Context, svc *coverage.Service) error {
				var err error
				resp, err = svc.Read(ctx, req)
				return err
			}); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), resp))
			}
			var b strings.Builder
			writeChunkHeader(&b, resp)
			if err := writeText(out, "%s", b.String()); err != nil {
				return err
			}
			// The chunk body is written last and verbatim. It is never clipped
			// or sanitized the way a table cell is: the point of this command is
			// that the bytes reach the reader exactly as the snapshot holds
			// them, and a base64 body is printed as the encoded string rather
			// than decoded into a terminal.
			// No trailing newline: a final line the snapshot stores without one
			// must reach the reader without one.
			return writeText(out, "%s", resp.Content)
		},
	}
	addContextFlags(cmd)
	cmd.Flags().Int64(contextOffsetFlag, 0, "byte offset within the file to read from; a continuation uses the next offset the previous chunk reported")
	cmd.Flags().Int64(contextMaxBytesFlag, 0,
		fmt.Sprintf("raw bytes to read at most: 0 uses the configured chunk size, otherwise at least %d so a chunk always makes progress and at most %d; a value above the configured chunk size is clamped to it rather than rejected",
			utf8.UTFMax, model.MaxRawChunkBytes))
	cmd.Flags().StringArray(contextConfirmReceiptFlag, nil,
		fmt.Sprintf("confirm a receipt an earlier read issued before serving this chunk; repeat the flag for more than one, up to %d", model.MaxReceiptsPerConfirmation))
	return cmd
}

// newContextAcknowledgeCommand builds `codectx context acknowledge`.
func newContextAcknowledgeCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "acknowledge <session-id> [file-id]",
		Short: "Confirm delivered receipts, or record a full-file review",
		Long: "Acknowledges delivery. The two kinds are separate assertions and are never " +
			"interchangeable.\n\n" +
			"--receipt confirms one or more receipts a read issued: that is what turns issued bytes " +
			"into served coverage, and it is idempotent, so replaying a receipt adds credit once.\n\n" +
			"--file-review records that the actor has reviewed a whole file, and takes the file id as " +
			"a second argument. It is refused unless that file is already fully served for this " +
			"actor: a review never creates coverage it is only allowed to attest to.",
		Args:          cobra.RangeArgs(1, 2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkContextIDs(args); err != nil {
				return err
			}
			actor, err := stringFlag(cmd, contextActorFlag)
			if err != nil {
				return err
			}
			receipts, err := stringsFlag(cmd, contextReceiptFlag)
			if err != nil {
				return err
			}
			fileReview, err := boolFlag(cmd, contextFileReviewFlag)
			if err != nil {
				return err
			}
			req, err := acknowledgeRequest(args, actor, receipts, fileReview)
			if err != nil {
				return err
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var status model.SessionStatus
			if err := runContext(cmd, func(ctx context.Context, svc *coverage.Service) error {
				var err error
				status, err = svc.Acknowledge(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, status, func(b *strings.Builder) {
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	cmd.Flags().StringArray(contextReceiptFlag, nil,
		fmt.Sprintf("confirm this receipt from an earlier read; repeat the flag for more than one, up to %d", model.MaxReceiptsPerConfirmation))
	cmd.Flags().Bool(contextFileReviewFlag, false,
		"record a full-file review of the file named as the second argument; refused unless that file is already fully served")
	return cmd
}

// acknowledgeRequest turns the two mutually exclusive forms into one request.
// The kinds are rejected here, before any workspace is opened, because they are
// two different assertions about two different things: confirming delivery of
// bytes and attesting to having reviewed a file. A command line that asked for
// both would have to be resolved by guessing which one the operator meant, and
// Section 16.3 is explicit that neither stands in for the other.
func acknowledgeRequest(args []string, actor string, receipts []string, fileReview bool) (model.AcknowledgeRequest, error) {
	req := model.AcknowledgeRequest{SessionID: model.SessionID(args[0]), ActorID: actor}
	switch {
	case fileReview && len(receipts) > 0:
		return req, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "--" + contextReceiptFlag + " and --" + contextFileReviewFlag + " are separate acknowledgments and cannot be combined",
			Remediation: "confirm the receipts first with --" + contextReceiptFlag +
				", then record the review with --" + contextFileReviewFlag + " once the file is fully served"}
	case fileReview:
		if len(args) != 2 {
			return req, &model.Error{Code: model.CodeArgumentInvalid,
				Message: "--" + contextFileReviewFlag + " needs the file id as a second argument"}
		}
		req.Kind, req.FileID = model.AcknowledgeFile, model.FileID(args[1])
	case len(receipts) > 0:
		if len(args) != 1 {
			return req, &model.Error{Code: model.CodeArgumentInvalid,
				Message: "a receipt acknowledgment takes only the session id; the file it covers is named by the receipt itself"}
		}
		req.Kind, req.Receipts = model.AcknowledgeReceipt, receipts
	default:
		return req, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "nothing to acknowledge: pass --" + contextReceiptFlag + " to confirm delivered bytes, or --" +
				contextFileReviewFlag + " to record a full-file review"}
	}
	return req, nil
}

// newContextStatusCommand builds `codectx context status`.
func newContextStatusCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <session-id>",
		Short: "Report this actor's coverage of one session",
		Long: "Reports what this actor has actually been served in this session, one page of files " +
			"at a time, with the session's own honest readiness.\n\n" +
			"Read completeness is about the pinned snapshot: it stays true for the snapshot the " +
			"session was opened against even after a newer generation becomes active. Strict " +
			"implementation readiness is a stronger and separate question this command never " +
			"answers true, and a waived file is reported as waived rather than as covered.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := sessionRequest(cmd, args)
			if err != nil {
				return err
			}
			page, err := pageRequest(cmd)
			if err != nil {
				return err
			}
			if err := page.Validate(); err != nil {
				return err
			}
			var (
				files  model.Page[model.FileCoverage]
				status model.SessionStatus
			)
			if err := runContext(cmd, func(ctx context.Context, svc *coverage.Service) error {
				var err error
				files, status, err = svc.Status(ctx, req, page)
				return err
			}); err != nil {
				return err
			}
			// Status is the one command in this group whose answer carries a
			// real model.QueryMeta, so it renders through emitQuery and gets the
			// shared binding, warning and continuation lines. The other four
			// carry no page and no completeness, and feeding emitQuery an empty
			// QueryMeta would print a generation and snapshot nobody reported.
			data := contextStatus{Session: status, Files: files}
			return emitQuery(cmd, build, args, data, files.Meta, func(b *strings.Builder) {
				writeSessionStatus(b, status)
				writeCoverageTable(b, files.Items)
			})
		},
	}
	addContextFlags(cmd)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	return cmd
}

// contextStatus is the `context status` answer. The command reports two things
// the service returns separately -- the session's readiness and one page of its
// files -- and a machine consumer needs both in the one envelope Section 18.2
// allows it.
type contextStatus struct {
	Session model.SessionStatus            `json:"session"`
	Files   model.Page[model.FileCoverage] `json:"files"`
}

// newContextNextCommand builds `codectx context next`.
func newContextNextCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "next <session-id>",
		Short: "Name the next required file this actor has not fully read",
		Long: "Names the next required file this actor still owes a read, in the manifest's own " +
			"reading order, with the offset to resume from and how many required files remain.\n\n" +
			"It is metadata only and carries no source bytes: it says what to read and where to " +
			"start, and `context read` serves it. When nothing is outstanding the reported action " +
			"says so rather than naming a file.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := sessionRequest(cmd, args)
			if err != nil {
				return err
			}
			var item model.NextContextItem
			if err := runContext(cmd, func(ctx context.Context, svc *coverage.Service) error {
				var err error
				item, err = svc.Next(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, item, func(b *strings.Builder) {
				writeNextItem(b, item)
			})
		},
	}
	addContextFlags(cmd)
	return cmd
}

// newContextCloseCommand builds `codectx context close`.
func newContextCloseCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "close <session-id>",
		Short: "Close one session with a version-checked transition",
		Long: "Closes the session. The close is the guarded transition of Section 17.1: it presents " +
			"the state version the caller last saw, and is refused when the session has moved on " +
			"since -- including a session that has already reached its terminal state, which has no " +
			"outgoing transition left to take.\n\n" +
			"The version to present is the state version `context status` reports.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := sessionRequest(cmd, args)
			if err != nil {
				return err
			}
			expected, err := intFlag(cmd, contextExpectedVersionFlag)
			if err != nil {
				return err
			}
			// Versions start at 1, so zero is not "the configured default" the
			// budget flags of this package use: it is a version that cannot
			// exist. Rejecting it here names the flag the operator has to fix
			// instead of surfacing a field name from inside the service.
			if expected < 1 {
				return &model.Error{Code: model.CodeArgumentInvalid,
					Message:     fmt.Sprintf("--%s is %d; state versions start at 1", contextExpectedVersionFlag, expected),
					Remediation: "pass the state_version that `codectx context status " + clip(args[0], model.IDHexLen) + "` reports"}
			}
			var status model.SessionStatus
			if err := runContext(cmd, func(ctx context.Context, svc *coverage.Service) error {
				var err error
				status, err = svc.Close(ctx, req, expected)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, status, func(b *strings.Builder) {
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	// No backquotes in a flag usage string: cobra reads the first backquoted
	// word as the flag's type name, so "`context status`" would render the
	// value placeholder as "context status" instead of "int".
	cmd.Flags().Int(contextExpectedVersionFlag, 0,
		"the state version this close expects, as \"codectx context status\" last reported it; the close is refused if the session has moved on")
	return cmd
}

// addContextFlags declares what every command in this group shares. It is not
// addQueryFlags: that one also declares --generation, which no session request
// has a field for.
func addContextFlags(cmd *cobra.Command) {
	addRepoFlag(cmd)
	cmd.Flags().String(contextActorFlag, "", actorFlagHelp)
	cmd.Flags().Duration(queryTimeoutFlag, 0, "give up after this much wall clock"+zeroBoundHelp)
}

// sessionRequest builds the request the three single-argument commands share.
// Its Validate is what rejects a missing --actor, and it runs before any
// workspace is opened.
func sessionRequest(cmd *cobra.Command, args []string) (model.SessionRequest, error) {
	if err := checkContextIDs(args); err != nil {
		return model.SessionRequest{}, err
	}
	actor, err := stringFlag(cmd, contextActorFlag)
	if err != nil {
		return model.SessionRequest{}, err
	}
	req := model.SessionRequest{SessionID: model.SessionID(args[0]), ActorID: actor}
	if err := req.Validate(); err != nil {
		return model.SessionRequest{}, err
	}
	return req, nil
}

// checkContextIDs screens the positional arguments before a database is opened.
// Unlike the graph commands these take no names: a session id and a file id are
// both resolved ids, so an argument that is not one cannot be looked up and is
// rejected rather than carried into the service.
func checkContextIDs(args []string) error {
	for _, arg := range args {
		if model.ValidHexID(arg) {
			continue
		}
		return (&model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("%q is not a resolved id (%d lowercase hex characters)", clip(arg, 64), model.IDHexLen)}).
			WithDetail("argument", clip(arg, 64))
	}
	return nil
}

// contextOffsetValue reads --offset. The request field is unsigned and bounded
// to the signed 64-bit range storage keeps, so both ends are checked before the
// conversion rather than after it, where a negative would arrive as an enormous
// offset past the end of every file.
func contextOffsetValue(cmd *cobra.Command) (uint64, error) {
	v, err := int64Flag(cmd, contextOffsetFlag)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s is %d; a byte offset must not be negative", contextOffsetFlag, v)}
	}
	return uint64(v), nil
}

// contextMaxBytesValue reads --max-bytes. The bound is the one
// ReadChunkRequest.Validate enforces and the flag's own help states, so an
// out-of-range value is named for what it is here rather than rejected a layer
// later against a different number. A value above the *configured* ceiling is
// not rejected at all: Section 16.2 clamps it, and the service owns that
// ceiling.
func contextMaxBytesValue(cmd *cobra.Command) (uint32, error) {
	v, err := int64Flag(cmd, contextMaxBytesFlag)
	if err != nil {
		return 0, err
	}
	if v < 0 || v > model.MaxRawChunkBytes || (v > 0 && v < utf8.UTFMax) {
		return 0, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s is %d; it must be 0 for the configured chunk size, or between %d and %d",
				contextMaxBytesFlag, v, utf8.UTFMax, int64(model.MaxRawChunkBytes))}
	}
	return uint32(v), nil
}

// runContext opens the workspace in report mode and runs one session operation
// against the coverage service. The --timeout deadline covers the service call
// alone, for the reason queryContext gives: a slow workspace open is not an
// operation that ran out of time.
func runContext(cmd *cobra.Command, fn func(context.Context, *coverage.Service) error) error {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return err
	}
	timeout, err := durationFlag(cmd, queryTimeoutFlag)
	if err != nil {
		return err
	}
	ws, err := app.OpenWorkspaceForReport(cmd.Context(), repo)
	if err != nil {
		return err
	}
	defer ws.Close()
	// The composition builds the coverage service eagerly, so a workspace that
	// opened has one; there is no per-command wiring check to make here.
	svc := ws.Coverage()
	ctx, cancel := queryContext(cmd.Context(), timeout)
	defer cancel()
	// queryFailure types a bare context failure: left untyped, a deadline the
	// operator set with --timeout would reach ExitCode as the invalid-argument
	// class and report itself as a command line they typed wrong.
	return queryFailure(fn(ctx, svc))
}

// emitContext writes the one answer for the commands whose result carries no
// page and no completeness report. A --json request emits exactly one envelope
// on stdout and nothing else; a human request gets the same facts as a bounded
// block, so neither consumer learns less than the other.
func emitContext[T any](cmd *cobra.Command, build model.BuildInfo, args []string, data T,
	render func(*strings.Builder)) error {
	out := cmd.OutOrStdout()
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), data))
	}
	var b strings.Builder
	render(&b)
	return writeText(out, "%s", b.String())
}

// writeChunkHeader renders everything about a chunk except its bytes: where it
// came from, how it is encoded, and the receipt that has to be echoed back
// before any of it counts as delivered. The receipt is printed in full -- a
// text-path reader who never sees it can never confirm anything.
func writeChunkHeader(b *strings.Builder, resp model.ReadChunkResponse) {
	fmt.Fprintf(b, "generation  %d\nsnapshot    %s\n", resp.Binding.GenerationID, resp.Binding.SnapshotID)
	fmt.Fprintf(b, "file        %s\nhash        %s\n", resp.FileID, resp.ContentHash)
	fmt.Fprintf(b, "bytes       [%d, %d) of the pinned source\n", resp.ByteRange.Start, resp.ByteRange.End)
	fmt.Fprintf(b, "lines       %d:%d-%d:%d\n", resp.LineRange.Start.Line, resp.LineRange.Start.Column,
		resp.LineRange.End.Line, resp.LineRange.End.Column)
	fmt.Fprintf(b, "encoding    %s\n", resp.Encoding)
	if resp.PartialLine {
		b.WriteString("partial     this chunk ends inside a line; the rest continues in the next chunk\n")
	}
	// The reported state is the coverage before this chunk: it was issued, not
	// confirmed, so reporting it as served would be the claim this whole
	// command exists to avoid making.
	fmt.Fprintf(b, "coverage    %s (before this chunk; it is issued, not yet confirmed)\n", resp.Coverage)
	if resp.NextOffset != nil {
		fmt.Fprintf(b, "next        --%s %d\n", contextOffsetFlag, *resp.NextOffset)
	} else {
		b.WriteString("next        end of file\n")
	}
	fmt.Fprintf(b, "receipt     %s\n", resp.Receipt)
	fmt.Fprintf(b, "            confirm it with --%s to earn coverage for these bytes\n", contextConfirmReceiptFlag)
	b.WriteString("---\n")
}

// writeSessionStatus renders the honest readiness answer. Read completeness and
// implementation readiness are printed as the two separate questions they are:
// collapsing them into one "ready" line is exactly the dishonest summary
// Section 16.3 forbids.
func writeSessionStatus(b *strings.Builder, st model.SessionStatus) {
	fmt.Fprintf(b, "session     %s\nactor       %s\n", st.SessionID, st.ActorID)
	fmt.Fprintf(b, "generation  %d\nsnapshot    %s\nmanifest    %s\n",
		st.Binding.GenerationID, st.Binding.SnapshotID, st.ManifestID)
	fmt.Fprintf(b, "state       %s in phase %s (state version %d, scope version %d)\n",
		st.State, st.Phase, st.StateVersion, st.ScopeVersion)
	fmt.Fprintf(b, "files       %d required, %d fully served, %d waived\n",
		st.RequiredFiles, st.FullyServedFiles, st.WaivedFiles)
	fmt.Fprintf(b, "scope       %s\n", yesNo(st.ScopeComplete, "complete", "incomplete"))
	fmt.Fprintf(b, "read        %s for the pinned snapshot\n",
		yesNo(st.ReadCompleteForSnapshot, "complete", "incomplete"))
	fmt.Fprintf(b, "ready       %s for implementation (strict gate %s)\n",
		yesNo(st.ReadyForImplementation, "yes", "no"),
		yesNo(st.StrictGateSatisfied, "satisfied", "not satisfied"))
	if st.Superseded {
		b.WriteString("warning     a newer generation has superseded this session's snapshot\n")
	}
	fmt.Fprintf(b, "expires     %s\n", st.ExpiresAt.UTC().Format(time.RFC3339))
	writeCapabilities(b, st.Completeness)
}

// writeNextItem renders the metadata-only answer. A completed session names no
// file, so the file columns are not printed as empty values that would read as
// a file whose id is unknown.
func writeNextItem(b *strings.Builder, item model.NextContextItem) {
	fmt.Fprintf(b, "generation  %d\nsnapshot    %s\n", item.Binding.GenerationID, item.Binding.SnapshotID)
	fmt.Fprintf(b, "action      %s\n", clip(item.Action, model.MaxIdentifierBytes))
	fmt.Fprintf(b, "remaining   %d required %s\n", item.Remaining, plural(int(item.Remaining), "file", "files"))
	if item.FileID == "" {
		return
	}
	fmt.Fprintf(b, "file        %s\n", item.FileID)
	if item.Path != "" {
		fmt.Fprintf(b, "path        %s\n", tableCell(item.Path))
	}
	fmt.Fprintf(b, "hash        %s\n", item.ContentHash)
	if item.Requirement != "" {
		fmt.Fprintf(b, "requirement %s\n", item.Requirement)
	}
	fmt.Fprintf(b, "resume      --%s %d of %d bytes\n", contextOffsetFlag, item.Offset, item.Size)
}

// writeCoverageTable renders one page of per-file coverage. The file id is
// printed whole because it is the argument `context read` takes; the content
// hash is shortened because nothing asks the operator to retype it.
func writeCoverageTable(b *strings.Builder, files []model.FileCoverage) {
	if len(files) == 0 {
		b.WriteString("files       none on this page\n")
		return
	}
	fmt.Fprintf(b, "files       %d on this page\n", len(files))
	for _, f := range files {
		waived := ""
		if f.Waived {
			waived = "  waived"
		}
		fmt.Fprintf(b, "  %s  %-14s %d/%d bytes  %-14s %s%s\n", f.FileID,
			tableCell(string(f.State)), f.ConfirmedBytes, f.Size,
			tableCell(string(f.Requirement)), shortID(f.ContentHash), waived)
	}
}

// yesNo renders a boolean as the two words that name what it decides, rather
// than as "true", which says nothing about which question was asked.
func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}
