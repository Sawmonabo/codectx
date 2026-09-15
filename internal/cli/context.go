package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/workspace"
	"github.com/spf13/cobra"
)

// Flag spellings for the Section 18.1 `context` commands. As everywhere else in
// this package, a command declares only the flags its own request type has a
// field for. In particular no command here declares --generation: a session
// pins its own generation and snapshot when it is opened, so a flag that
// claimed to choose one would be help text describing a request none of these
// commands can build. `context plan` is no exception -- the generation it
// compiles against is the active one.
const (
	contextActorFlag           = "actor"
	contextOffsetFlag          = "offset"
	contextMaxBytesFlag        = "max-bytes"
	contextConfirmReceiptFlag  = "confirm-receipt"
	contextReceiptFlag         = "receipt"
	contextFileReviewFlag      = "file-review"
	contextExpectedVersionFlag = "expected-version"
	contextTaskFlag            = "task"
	contextPhaseFlag           = "phase"
	contextSeedFlag            = "seed"
	contextBudgetFlag          = "budget"
	contextBudgetBytesFlag     = "budget-bytes"
	contextBudgetFilesFlag     = "budget-files"
	contextBudgetSlicesFlag    = "budget-slices"
	contextViewFlag            = "view"
	contextReasonFlag          = "reason"
	contextKindFlag            = "kind"
	contextExpectedScopeFlag   = "expected-scope"
	contextInputFlag           = "input"
	contextRelationFlag        = "relation"
	contextNodeFlag            = "node"
	contextSourceFlag          = "source"
	contextNoteFlag            = "note"
	contextOutputFlag          = "output"
)

// maxObservationInputBytes bounds what --input may read. It is a bound on ONE
// local file the CLI decodes, not on how many references an observation may
// carry: workflow.max_observation_references is the caller's own ceiling and is
// unlimited by default, so the model no longer fixes a widest legal document.
// The bound exists so a file that is not an observation at all is refused at the
// read rather than decoded.
const maxObservationInputBytes = 1 << 20

// actorFlagHelp is the one sentence every command in this group repeats.
// Section 16.1 scopes coverage to one actor: a parent's receipts never satisfy
// a subagent's requirement, so the actor is part of the request rather than
// something the process identity may stand in for.
const actorFlagHelp = "required: the actor this session belongs to; coverage is per actor, so one agent's reads never satisfy another's requirement"

// newContextCommands builds the Section 18.1 session commands. Like the query
// commands they are returned as a set, so this file never edits the command
// tree it belongs to; the root adds them in one loop.
//
// This is the whole Section 18.1 `context` surface: the session lifecycle from
// `plan` through `close`, plus `export`. Every one of them reaches its
// operation through the one application facade (Workspace.Services), so the CLI
// and the MCP server drive the same code rather than two drifting copies.
func newContextCommands(build model.BuildInfo) []*cobra.Command {
	root := &cobra.Command{
		Use:   "context",
		Short: "Plan, read, review and seal one snapshot-pinned context session",
		Long: "Drives one context session end to end: `plan` compiles an immutable manifest and opens " +
			"the session, `read` serves its source in bounded, lossless chunks, `acknowledge` turns " +
			"delivered bytes into coverage, `record` stores what the actor concluded, `advance` takes " +
			"the guarded transitions, and `capsule`/`export` produce the deterministic completion " +
			"record.\n\n" +
			"Bytes earn coverage only when the signed receipt the read issued is echoed back and " +
			"accepted, so a chunk that was issued but never confirmed -- a broken pipe, a dropped " +
			"connection, an agent that stopped reading -- grants no credit at all. Every readiness " +
			"answer is the honest one: read completeness and strict implementation readiness are " +
			"separate questions, and a waived file is reported as waived rather than as covered.",
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
	// Registered in the Section 18.1 listing order, which is also the order an
	// actor drives them in: plan, learn what is outstanding, read it, confirm
	// it, review it, advance, seal, close.
	root.AddCommand(
		newContextPlanCommand(build),
		newContextStatusCommand(build),
		newContextNextCommand(build),
		newContextEntriesCommand(build),
		newContextIncludeCommand(build),
		newContextReadCommand(build),
		newContextAcknowledgeCommand(build),
		newContextWaiveCommand(build),
		newContextRecordCommand(build),
		newContextAdvanceCommand(build),
		newContextCapsuleCommand(build),
		newContextCloseCommand(build),
		newContextExportCommand(build),
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
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
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
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
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
			"implementation readiness is a stronger and separate question: it is true only when " +
			"every Section 16.3 precondition holds at this instant, and a waived file is " +
			"reported as waived rather than as covered.",
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
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				files, status, err = svc.SessionStatus(ctx, req, page)
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
				// The body only: emitQuery's header already printed this
				// answer's generation and snapshot, and the session pins the
				// same two (SessionStatus.Binding is the page's binding).
				writeSessionBody(b, status)
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
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
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
			expected, err := contextVersionValue(cmd, contextExpectedVersionFlag, "state", args[0])
			if err != nil {
				return err
			}
			var status model.SessionStatus
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				status, err = svc.CloseSession(ctx, req, expected)
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
	addExpectedVersionFlag(cmd, "close")
	return cmd
}

// addContextFlags declares what every command in this group shares. It is not
// addQueryFlags: that one also declares --generation, which no command here
// sets. ContextRequest does carry a generation, but `context plan` compiles
// against the active one for the reason the flag block at the top of this file
// gives -- a session pins its generation when it is opened, and every later
// command reads the one the session pinned.
func addContextFlags(cmd *cobra.Command) {
	addRepoFlag(cmd)
	cmd.Flags().String(contextActorFlag, "", actorFlagHelp)
	cmd.Flags().Duration(queryTimeoutFlag, 0, "give up after this much wall clock"+zeroBoundHelp)
}

// sessionRequest builds the request the commands share whose whole input is the
// session and its actor. Its Validate is what rejects a missing --actor, and it
// runs before any workspace is opened.
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
	// The session's own binding, for the seven commands that render outside
	// emitQuery and would otherwise report no generation at all.
	fmt.Fprintf(b, "generation  %d\nsnapshot    %s\n", st.Binding.GenerationID, st.Binding.SnapshotID)
	writeSessionBody(b, st)
}

// writeSessionBody is writeSessionStatus without the binding header, for the
// one caller -- `context status` -- whose answer carries a real model.QueryMeta
// and therefore already printed the generation and snapshot through emitQuery's
// shared header. Printing them again from the session block told the operator
// the same two facts twice.
func writeSessionBody(b *strings.Builder, st model.SessionStatus) {
	fmt.Fprintf(b, "session     %s\nactor       %s\nmanifest    %s\n", st.SessionID, st.ActorID, st.ManifestID)
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
	writeGuaranteeLimit(b, st)
	if st.Superseded {
		b.WriteString("warning     a newer generation has superseded this session's snapshot\n")
	}
	fmt.Fprintf(b, "expires     %s\n", st.ExpiresAt.UTC().Format(time.RFC3339))
	writeCapabilities(b, st.Completeness)
}

// writeGuaranteeLimit prints the evaluator's own words about the two readiness
// booleans printed above it: when the gate is open it states that readiness is
// point-in-time, and when the gate is shut it names the precondition that shut
// it. Printing the booleans without it is the bare "yes" Section 16.3 forbids,
// and a shut gate whose reason stays inside the service is a refusal the
// operator cannot act on.
//
// The text is wrapped instead of being handed whole to tableCell: the
// open-gate sentence is several table cells long, and clipping it would drop
// exactly the half that says what to do before the write phase.
func writeGuaranteeLimit(b *strings.Builder, st model.SessionStatus) {
	if strings.TrimSpace(st.GuaranteeLimit) == "" {
		return
	}
	// The gate's own two answers read as different kinds of sentence, so they
	// are not printed under one label that would fit only one of them.
	label := "reason"
	if st.ReadyForImplementation {
		label = "guarantee"
	}
	for i, line := range wrapCells(st.GuaranteeLimit) {
		if i == 0 {
			fmt.Fprintf(b, "%-11s %s\n", label, line)
			continue
		}
		fmt.Fprintf(b, "            %s\n", line)
	}
}

// wrapCells breaks one bounded sentence into table-cell-wide lines on word
// boundaries, each sanitized by tableCell. A word longer than a cell is left
// to tableCell's own clip: nothing this renders is a single unbroken token.
func wrapCells(s string) []string {
	var (
		lines []string
		line  string
	)
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= tableCellWidth:
			line += " " + word
		default:
			lines = append(lines, tableCell(line))
			line = word
		}
	}
	if line != "" {
		lines = append(lines, tableCell(line))
	}
	return lines
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

// --- Section 18.1 commands beyond the read-and-coverage five ----------------

// newContextPlanCommand builds `codectx context plan`.
func newContextPlanCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Compile one manifest and open a session over it",
		Long: "Compiles the context for a task into one immutable manifest and opens this actor's " +
			"session over it, pinned to the generation and snapshot the manifest was compiled " +
			"against.\n\n" +
			"The phase is the honest one: `sweep` gathers, `verify` confirms. `consolidate` cannot be " +
			"planned -- it is compiled by the workflow service for a session that has already been " +
			"advanced into it, so a plan that named it would open a session no transition could have " +
			"produced.\n\n" +
			"The manifest is content-addressed: the same request against the same generation compiles " +
			"to the same manifest, and a second actor asking for it gets its own session over that " +
			"same manifest rather than a shared one.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			actor, err := stringFlag(cmd, contextActorFlag)
			if err != nil {
				return err
			}
			task, err := stringFlag(cmd, contextTaskFlag)
			if err != nil {
				return err
			}
			phase, err := stringFlag(cmd, contextPhaseFlag)
			if err != nil {
				return err
			}
			seeds, err := stringsFlag(cmd, contextSeedFlag)
			if err != nil {
				return err
			}
			budget, err := contextBudgetValue(cmd)
			if err != nil {
				return err
			}
			req := model.PlanRequest{
				Context: model.ContextRequest{Task: task, Seeds: seeds, Phase: model.Phase(phase), Budget: budget},
				ActorID: actor,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var (
				result model.PlanResult
				status model.SessionStatus
			)
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				result, status, err = svc.Plan(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, contextPlan{Plan: result, Session: status}, func(b *strings.Builder) {
				writePlanResult(b, result)
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	cmd.Flags().String(contextTaskFlag, "", "required: what this session is for, in the actor's own words; it is part of the manifest's identity")
	cmd.Flags().String(contextPhaseFlag, "", "required: \"sweep\" to gather or \"verify\" to confirm; \"consolidate\" is not plannable and is reached with \"context advance\"")
	cmd.Flags().StringArray(contextSeedFlag, nil,
		fmt.Sprintf("a path or symbol to start selection from; repeat the flag for more than one, up to %d", model.MaxSeeds))
	cmd.Flags().Int64(contextBudgetFlag, 0, "estimated tokens one slice may cost at most"+zeroBoundHelp)
	cmd.Flags().Int64(contextBudgetBytesFlag, 0, "bytes one slice may cost at most"+zeroBoundHelp)
	cmd.Flags().Int(contextBudgetFilesFlag, 0, "distinct files the whole plan may select at most"+zeroBoundHelp)
	cmd.Flags().Int(contextBudgetSlicesFlag, 0, "slices the plan may be split into at most"+zeroBoundHelp)
	return cmd
}

// contextPlan is the `context plan` answer. Planning both compiles a manifest
// and opens a session, and a machine consumer needs both in the one envelope
// Section 18.2 allows it -- otherwise the session it has to use next is only
// reachable by a second round trip.
type contextPlan struct {
	Plan    model.PlanResult    `json:"plan"`
	Session model.SessionStatus `json:"session"`
}

// contextBudgetValue reads the four budget flags. Section 18.1 keeps the
// byte/file/slice budgets as explicit flags beside the --budget token-estimate
// shorthand, because a token estimate is an estimate: an operator bounding
// transport cost needs to bound the bytes themselves.
func contextBudgetValue(cmd *cobra.Command) (model.Budget, error) {
	tokens, err := int64Flag(cmd, contextBudgetFlag)
	if err != nil {
		return model.Budget{}, err
	}
	bytes, err := int64Flag(cmd, contextBudgetBytesFlag)
	if err != nil {
		return model.Budget{}, err
	}
	files, err := intFlag(cmd, contextBudgetFilesFlag)
	if err != nil {
		return model.Budget{}, err
	}
	slices, err := intFlag(cmd, contextBudgetSlicesFlag)
	if err != nil {
		return model.Budget{}, err
	}
	// Budget.Validate rejects a negative field and names it; zero means "use
	// the configured budget" here exactly as it does everywhere else in this
	// package, and never means unlimited.
	return model.Budget{MaxEstimatedTokens: tokens, MaxBytes: bytes, MaxFiles: files, MaxSlices: slices}, nil
}

// newContextEntriesCommand builds `codectx context entries`.
func newContextEntriesCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entries <session-id>",
		Short: "Page one projection of the session's current manifest",
		Long: "Pages the manifest this session is pinned to, one projection at a time: the ranked " +
			"entries it selected, the slices they were packed into, or the candidates it excluded and " +
			"why.\n\n" +
			"It is metadata only and carries no source bytes; `context read` serves those. A page " +
			"carries records for exactly the view it names, so a continuation token is never ambiguous " +
			"about which list it advances through.\n\n" +
			"The manifest is the session's *current* one: after `context include` extends scope, the " +
			"entries reported here are the extended set.",
		Args:          cobra.ExactArgs(1),
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
			view, err := stringFlag(cmd, contextViewFlag)
			if err != nil {
				return err
			}
			page, err := pageRequest(cmd)
			if err != nil {
				return err
			}
			req := model.ContextPageRequest{
				SessionID: model.SessionID(args[0]),
				ActorID:   actor,
				View:      model.ContextView(view),
				Page:      page,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.ContextPage
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				result, err = svc.Entries(ctx, req)
				return err
			}); err != nil {
				return err
			}
			// Like `context status`, this answer carries a real QueryMeta, so it
			// renders through emitQuery and gets the shared binding, warning and
			// continuation lines.
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeContextPage(b, result)
			})
		},
	}
	addContextFlags(cmd)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	cmd.Flags().String(contextViewFlag, string(model.ViewEntries),
		"which projection to page: \"entries\", \"slices\" or \"excluded\"")
	return cmd
}

// newContextIncludeCommand builds `codectx context include`.
func newContextIncludeCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "include <session-id>",
		Short: "Add discovered scope to a pinned session",
		Long: "Adds scope an actor discovered mid-session: the manifest is recompiled with the new " +
			"seeds and the session's scope version moves on.\n\n" +
			"It is the compare-and-swap of Section 17.1 -- the version this include presents has to be " +
			"the one the caller last saw, so two clients cannot both extend scope from the same " +
			"version and each believe they have the whole picture.\n\n" +
			"A successful include invalidates any scope review recorded against the old version: the " +
			"actor has not reviewed what it has just discovered, and the strict gate says so until a " +
			"review of the new scope is recorded.",
		Args:          cobra.ExactArgs(1),
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
			seeds, err := stringsFlag(cmd, contextSeedFlag)
			if err != nil {
				return err
			}
			expected, err := contextVersionValue(cmd, contextExpectedVersionFlag, "state", args[0])
			if err != nil {
				return err
			}
			req := model.IncludeRequest{
				SessionID:       model.SessionID(args[0]),
				ActorID:         actor,
				Seeds:           seeds,
				ExpectedVersion: expected,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var status model.SessionStatus
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				status, err = svc.Include(ctx, req)
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
	cmd.Flags().StringArray(contextSeedFlag, nil,
		fmt.Sprintf("required: a path or symbol to add to the session's scope; repeat the flag for more than one, up to %d", model.MaxSeeds))
	addExpectedVersionFlag(cmd, "include")
	return cmd
}

// newContextWaiveCommand builds `codectx context waive`.
func newContextWaiveCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "waive <session-id> <file-id>",
		Short: "Record an auditable waiver for one required file",
		Long: "Records that a required file was deliberately not read, with the reason, the pinned " +
			"content hash and a timestamp.\n\n" +
			"A waiver is an admission, not a shortcut. It never creates coverage, the file is reported " +
			"as waived rather than as covered, and the strict implementation gate stays unsatisfied " +
			"for as long as any waiver exists on the session. The reason is mandatory because a waiver " +
			"nobody has to justify is indistinguishable from a file that was forgotten.",
		Args:          cobra.ExactArgs(2),
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
			reason, err := stringFlag(cmd, contextReasonFlag)
			if err != nil {
				return err
			}
			req := model.WaiverRequest{
				SessionID: model.SessionID(args[0]),
				ActorID:   actor,
				FileID:    model.FileID(args[1]),
				Reason:    reason,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var (
				record model.WaiverRecord
				status model.SessionStatus
			)
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				record, status, err = svc.Waive(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, contextWaiver{Waiver: record, Session: status}, func(b *strings.Builder) {
				writeWaiver(b, record)
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	cmd.Flags().String(contextReasonFlag, "", "required: why this required file is not being read; it is stored verbatim in the capsule")
	return cmd
}

// contextWaiver is the `context waive` answer: the stored waiver and the
// readiness it deliberately did not grant.
type contextWaiver struct {
	Waiver  model.WaiverRecord  `json:"waiver"`
	Session model.SessionStatus `json:"session"`
}

// newContextRecordCommand builds `codectx context record`.
func newContextRecordCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record <session-id>",
		Short: "Record one explicit observation or a structured scope review",
		Long: "Records what the actor concluded. codectx writes no observation of its own: every " +
			"accepted fact, rejection, contradiction, unresolved item and scope review here is the " +
			"actor's own assertion, stored immutably and cited in the capsule.\n\n" +
			"The two input forms are exclusive. --input reads one bounded typed observation document, " +
			"which is the only way to record the structured scope review with its eight required " +
			"categories. The --relation/--node/--source/--note flags build an ordinary observation " +
			"directly and cannot be combined with --input, because a request assembled from both " +
			"would have to resolve a conflict by guessing which source the operator meant.\n\n" +
			"A cited source interval must already be confirmed served to this actor at the hash it " +
			"names: the hash is the one the session pinned, which `context status` and `context next` " +
			"report, never the working tree's.\n\n" +
			"--expected-scope is the scope version the observation is about. An observation recorded " +
			"against a scope version that has since moved on is refused rather than silently " +
			"reattributed to the new scope.",
		Args:          cobra.ExactArgs(1),
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
			kind, err := stringFlag(cmd, contextKindFlag)
			if err != nil {
				return err
			}
			scope, err := contextVersionValue(cmd, contextExpectedScopeFlag, "scope", args[0])
			if err != nil {
				return err
			}
			req, err := observationRequest(cmd, args[0], actor, model.ObservationKind(kind), scope)
			if err != nil {
				return err
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var (
				record model.Observation
				status model.SessionStatus
			)
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				record, status, err = svc.Record(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, contextObservation{Observation: record, Session: status}, func(b *strings.Builder) {
				writeObservation(b, record)
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	cmd.Flags().String(contextKindFlag, "", "required: \"accept_fact\", \"reject_fact\", \"contradiction\", \"unresolved\" or \"scope_review\"")
	cmd.Flags().Int(contextExpectedScopeFlag, 0, "required: the scope version this observation is about, as \"codectx context status\" last reported it")
	cmd.Flags().String(contextInputFlag, "",
		"read the whole observation body -- references, scope review and note -- from this typed JSON file; not combinable with --"+
			contextRelationFlag+", --"+contextNodeFlag+", --"+contextSourceFlag+" or --"+contextNoteFlag)
	cmd.Flags().StringArray(contextRelationFlag, nil, "cite this relation id; repeat the flag for more than one")
	cmd.Flags().StringArray(contextNodeFlag, nil, "cite this node id; repeat the flag for more than one")
	cmd.Flags().StringArray(contextSourceFlag, nil,
		"cite an already-served interval as FILE:HASH:START:END, using the file id and pinned content hash \"codectx context status\" reports and half-open byte bounds; repeat the flag for more than one")
	cmd.Flags().String(contextNoteFlag, "", "the actor's own note; it is stored verbatim and is part of the observation's identity")
	return cmd
}

// contextObservation is the `context record` answer: the stored record and the
// readiness that recording it produced.
type contextObservation struct {
	Observation model.Observation   `json:"observation"`
	Session     model.SessionStatus `json:"session"`
}

// observationInput is the --input document: the observation's body only. The
// session, actor, kind and expected scope version stay on the command line
// where Section 18.1 puts them, so a file can never disagree with the session
// it is being recorded against.
type observationInput struct {
	References []model.ClaimReference `json:"references,omitempty"`
	Review     *model.ScopeReview     `json:"review,omitempty"`
	Note       string                 `json:"note"`
}

// observationRequest assembles the request from exactly one of the two input
// forms. The exclusivity is enforced here, before a workspace is opened,
// because there is no honest merge of the two: a document and a set of flags
// that disagree about the cited references describe two different observations,
// and either choice records a claim the operator did not make.
func observationRequest(cmd *cobra.Command, session, actor string, kind model.ObservationKind, scope int) (model.ObservationRequest, error) {
	req := model.ObservationRequest{
		SessionID:     model.SessionID(session),
		ActorID:       actor,
		ExpectedScope: scope,
		Kind:          kind,
	}
	input, err := stringFlag(cmd, contextInputFlag)
	if err != nil {
		return req, err
	}
	relations, err := stringsFlag(cmd, contextRelationFlag)
	if err != nil {
		return req, err
	}
	nodes, err := stringsFlag(cmd, contextNodeFlag)
	if err != nil {
		return req, err
	}
	sources, err := stringsFlag(cmd, contextSourceFlag)
	if err != nil {
		return req, err
	}
	note, err := stringFlag(cmd, contextNoteFlag)
	if err != nil {
		return req, err
	}
	direct := len(relations) > 0 || len(nodes) > 0 || len(sources) > 0 || note != ""
	if input != "" && direct {
		return req, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "--" + contextInputFlag + " carries the whole observation body, so it cannot be combined with --" +
				contextRelationFlag + ", --" + contextNodeFlag + ", --" + contextSourceFlag + " or --" + contextNoteFlag,
			Remediation: "put every reference and the note in the --" + contextInputFlag +
				" document, or drop --" + contextInputFlag + " and build the observation from the flags"}
	}
	if input != "" {
		body, err := readObservationInput(input)
		if err != nil {
			return req, err
		}
		req.References, req.Review, req.Note = body.References, body.Review, body.Note
		return req, nil
	}
	if !direct {
		return req, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "nothing to record: pass --" + contextInputFlag + " with a typed observation document, or build one with --" +
				contextRelationFlag + ", --" + contextNodeFlag + ", --" + contextSourceFlag + " and --" + contextNoteFlag}
	}
	refs := make([]model.ClaimReference, 0, len(relations)+len(nodes)+len(sources))
	for _, id := range relations {
		refs = append(refs, model.ClaimReference{RelationID: model.RelationID(id)})
	}
	for _, id := range nodes {
		refs = append(refs, model.ClaimReference{NodeID: model.NodeID(id)})
	}
	for _, spec := range sources {
		citation, err := parseSourceCitation(spec)
		if err != nil {
			return req, err
		}
		refs = append(refs, model.ClaimReference{Source: &citation})
	}
	req.References, req.Note = refs, note
	return req, nil
}

// readObservationInput decodes the --input document. Unknown fields are refused
// rather than ignored: a misspelled "review" that was silently dropped would
// record an observation missing the very attestation the operator wrote.
func readObservationInput(path string) (observationInput, error) {
	var body observationInput
	f, err := os.Open(path)
	if err != nil {
		return body, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s could not be read: %s", contextInputFlag, clip(err.Error(), model.MaxDetailBytes))}
	}
	defer f.Close()
	// The size is checked before the decode rather than by bounding the reader,
	// so an oversized file is refused for being oversized. A LimitReader would
	// cut the document mid-token and report the truncation as malformed JSON,
	// which sends the operator looking for a syntax error that is not there.
	// The LimitReader stays as the guard for what the stat cannot size -- a
	// growing file, a fifo, a device.
	info, err := f.Stat()
	if err != nil {
		return body, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s could not be read: %s", contextInputFlag, clip(err.Error(), model.MaxDetailBytes))}
	}
	if info.Size() > maxObservationInputBytes {
		return body, errObservationInputTooLarge()
	}
	dec := json.NewDecoder(io.LimitReader(f, maxObservationInputBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return observationInput{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message:     fmt.Sprintf("--%s is not a typed observation document: %s", contextInputFlag, clip(err.Error(), model.MaxDetailBytes)),
			Remediation: "the document carries \"references\", an optional \"review\" and \"note\"; the session, actor, kind and scope version stay on the command line"}
	}
	// A second JSON value in the file is two observations, and recording the
	// first silently would drop the second without saying so.
	if dec.More() {
		return observationInput{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "--" + contextInputFlag + " carries more than one JSON document; one observation is recorded per command"}
	}
	// Reached by what the stat could not size: a stream the LimitReader cut at
	// the bound after consuming a complete JSON value.
	if dec.InputOffset() > maxObservationInputBytes {
		return observationInput{}, errObservationInputTooLarge()
	}
	return body, nil
}

// errObservationInputTooLarge is the one refusal both size checks report.
func errObservationInputTooLarge() error {
	return &model.Error{Code: model.CodeArgumentInvalid,
		Message: fmt.Sprintf("--%s is larger than %d bytes, which no observation document is",
			contextInputFlag, maxObservationInputBytes)}
}

// parseSourceCitation reads one --source FILE:HASH:START:END. The pinned
// content hash is part of the spelling because it is part of the claim: Section
// 17.2 proves a citation against the interval confirmed served for that
// session, file AND hash, so a citation without one names no provable interval.
// It is the hash the session pinned, which is why the help points at `context
// status` rather than at the working tree.
func parseSourceCitation(spec string) (model.SourceCitation, error) {
	malformed := func(why string) error {
		return (&model.Error{Code: model.CodeArgumentInvalid,
			Message:     fmt.Sprintf("--%s %q is not FILE:HASH:START:END: %s", contextSourceFlag, clip(spec, model.MaxPathBytes), why),
			Remediation: "take the file id and content hash from \"codectx context status\", and the byte bounds as half-open [start, end)"}).
			WithDetail("source", clip(spec, model.MaxPathBytes))
	}
	parts := strings.Split(spec, ":")
	if len(parts) != 4 {
		return model.SourceCitation{}, malformed(fmt.Sprintf("it has %d colon-separated fields, not 4", len(parts)))
	}
	start, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil {
		return model.SourceCitation{}, malformed("the start offset is not a non-negative whole number")
	}
	end, err := strconv.ParseUint(parts[3], 10, 64)
	if err != nil {
		return model.SourceCitation{}, malformed("the end offset is not a non-negative whole number")
	}
	// SourceCitation.Validate refuses an empty interval and an out-of-range
	// bound; the id and hash shapes it checks are reported by requireID there.
	return model.SourceCitation{
		FileID:      model.FileID(parts[0]),
		ContentHash: parts[1],
		Bytes:       model.ByteRange{Start: start, End: end},
	}, nil
}

// newContextAdvanceCommand builds `codectx context advance`.
func newContextAdvanceCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "advance <session-id> verify|consolidate|complete",
		Short: "Take one guarded, version-checked transition",
		Long: "Moves the session forward. The transition is guarded twice: it presents the state " +
			"version the caller last saw and is refused when the session has moved on since, and it is " +
			"refused outright when the phase's own preconditions are not met.\n\n" +
			"Those preconditions are the honest gate of Sections 16.3 and 17.1, not a formality: " +
			"advancing to `consolidate` requires the required files to be fully served to this actor " +
			"and a scope review recorded against the current scope version, and advancing to " +
			"`complete` seals the deterministic capsule inside that guard and moves the state " +
			"only once its hash is verified -- sealing is not a separate step to run first. " +
			"A refusal here reports which " +
			"precondition is unmet; it is not an error in the command line.\n\n" +
			"`closed` is not a target of this command: closing a session is \"codectx context close\", " +
			"which is the same guarded transition under the name operators reach for.",
		Args:          cobra.ExactArgs(2),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Only the first argument is an id: the second is the target word,
			// which checkContextIDs would reject as malformed hex.
			if err := checkContextIDs(args[:1]); err != nil {
				return err
			}
			actor, err := stringFlag(cmd, contextActorFlag)
			if err != nil {
				return err
			}
			target, err := advanceTarget(args[1])
			if err != nil {
				return err
			}
			expected, err := contextVersionValue(cmd, contextExpectedVersionFlag, "state", args[0])
			if err != nil {
				return err
			}
			req := model.AdvanceRequest{
				SessionID:       model.SessionID(args[0]),
				ActorID:         actor,
				Target:          target,
				ExpectedVersion: expected,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var (
				workflow model.WorkflowStatus
				status   model.SessionStatus
			)
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				workflow, status, err = svc.Advance(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitContext(cmd, build, args, contextAdvance{Workflow: workflow, Session: status}, func(b *strings.Builder) {
				writeWorkflowStatus(b, workflow)
				writeSessionStatus(b, status)
			})
		},
	}
	addContextFlags(cmd)
	addExpectedVersionFlag(cmd, "transition")
	return cmd
}

// contextAdvance is the `context advance` answer: the new workflow state and
// the readiness the session now honestly reports.
type contextAdvance struct {
	Workflow model.WorkflowStatus `json:"workflow"`
	Session  model.SessionStatus  `json:"session"`
}

// advanceTarget maps the Section 18.1 argument words onto the stored workflow
// states. The two vocabularies differ on purpose -- the state is named for the
// phase being open, the argument for the phase being entered -- so the mapping
// is explicit here rather than left to a cast that would pass "verify" through
// as a state no transition table knows.
func advanceTarget(arg string) (model.WorkflowState, error) {
	switch arg {
	case string(model.PhaseVerify):
		return model.StateVerifyOpen, nil
	case string(model.PhaseConsolidate):
		return model.StateConsolidateOpen, nil
	case string(model.StateComplete):
		return model.StateComplete, nil
	}
	return "", &model.Error{Code: model.CodeArgumentInvalid,
		Message: fmt.Sprintf("%q is not a transition target; the targets are %s, %s and %s",
			clip(arg, model.MaxIdentifierBytes), model.PhaseVerify, model.PhaseConsolidate, model.StateComplete),
		Remediation: "to close a session use \"codectx context close\" instead"}
}

// newContextCapsuleCommand builds `codectx context capsule`.
func newContextCapsuleCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capsule <session-id>",
		Short: "Page one projection of the session's sealed capsule",
		Long: "Pages the capsule: the deterministic completion record assembled only from stored facts " +
			"and the actor's own observations. codectx writes no summary of its own into it.\n\n" +
			"The capsule's lists are paged one projection at a time -- accepted and rejected facts, " +
			"contradictions, unresolved items, coverage and waivers -- because a whole capsule can " +
			"exceed the response budget. Every page carries the capsule's canonical hash, so two pages " +
			"of the same capsule are recognisable as such and a page of a recomputed one is not.\n\n" +
			"\"codectx context export\" writes the whole capsule to a file instead.",
		Args:          cobra.ExactArgs(1),
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
			view, err := stringFlag(cmd, contextViewFlag)
			if err != nil {
				return err
			}
			page, err := pageRequest(cmd)
			if err != nil {
				return err
			}
			req := model.CapsuleRequest{
				SessionID: model.SessionID(args[0]),
				ActorID:   actor,
				View:      model.CapsuleView(view),
				Page:      page,
			}
			if err := req.Validate(); err != nil {
				return err
			}
			var result model.CapsulePage
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				result, err = svc.Capsule(ctx, req)
				return err
			}); err != nil {
				return err
			}
			return emitQuery(cmd, build, args, result, result.Meta, func(b *strings.Builder) {
				writeCapsulePage(b, result)
			})
		},
	}
	addContextFlags(cmd)
	addLimitFlag(cmd)
	addCursorFlag(cmd)
	cmd.Flags().String(contextViewFlag, string(model.CapsuleViewAcceptedFacts),
		"which of the capsule's lists to page: \"accepted_facts\", \"rejected_facts\", \"contradictions\", \"unresolved\", \"coverage\" or \"waivers\"")
	return cmd
}

// newContextExportCommand builds `codectx context export`.
func newContextExportCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export <session-id>",
		Short: "Write the whole sealed capsule to a file",
		Long: "Writes the session's capsule, whole, to the file named by --output.\n\n" +
			"It writes only that file and it refuses to overwrite an existing one: a capsule is the " +
			"durable record a later session replays, and silently replacing a file -- a source file " +
			"above all -- would destroy work on the strength of a mistyped path. Choose another path, " +
			"or remove the existing file deliberately.\n\n" +
			"It also refuses any path inside the repository. codectx writes nothing into the tree it " +
			"indexes, so a capsule belongs beside the repository rather than in it -- a new file in " +
			"the working tree would be indexed as source, committed by accident, or both.\n\n" +
			"Use \"codectx context capsule\" to read the capsule a page at a time without writing it.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := sessionRequest(cmd, args)
			if err != nil {
				return err
			}
			output, err := stringFlag(cmd, contextOutputFlag)
			if err != nil {
				return err
			}
			if strings.TrimSpace(output) == "" {
				return &model.Error{Code: model.CodeArgumentInvalid,
					Message: "--" + contextOutputFlag + " is required: export writes the capsule to a file and prints none of it"}
			}
			// Screened before the workspace is opened, so a mistyped path that
			// names an existing file -- a source file above all -- is reported
			// without first assembling a capsule that will be thrown away.
			// O_EXCL below is still what actually refuses an existing file: it
			// is the only check that cannot lose a race with another writer.
			// The repository screen has no O_EXCL equivalent, because the path
			// it refuses is precisely one that does not exist yet.
			if err := screenCapsuleOutput(cmd, output); err != nil {
				return err
			}
			if _, err := os.Lstat(output); err == nil {
				return errOutputExists()
			}
			var capsule model.Capsule
			if err := runService(cmd, openForReport(), func(ctx context.Context, _ *app.Workspace, svc *app.Services) error {
				var err error
				capsule, err = svc.Export(ctx, req)
				return err
			}); err != nil {
				return err
			}
			written, err := writeCapsuleFile(output, capsule)
			if err != nil {
				return err
			}
			return emitContext(cmd, build, args, contextExport{Output: output, Bytes: written, Capsule: capsuleSummary(capsule)},
				func(b *strings.Builder) {
					fmt.Fprintf(b, "session     %s\ncapsule     %s\n", capsule.SessionID, capsule.CanonicalHash)
					fmt.Fprintf(b, "output      %s (%d bytes)\n", tableCell(output), written)
				})
		},
	}
	addContextFlags(cmd)
	cmd.Flags().String(contextOutputFlag, "", "required: the file to write the capsule to, outside the repository; an existing file is never overwritten")
	return cmd
}

// contextExport is the `context export` answer. The capsule itself is in the
// file, so the envelope carries where it went and enough of its identity to
// verify that the file on disk is the capsule this command exported.
type contextExport struct {
	Output  string          `json:"output"`
	Bytes   int64           `json:"bytes"`
	Capsule capsuleIdentity `json:"capsule"`
}

// capsuleIdentity is the capsule's identity without its body.
type capsuleIdentity struct {
	SessionID           model.SessionID `json:"session_id"`
	ActorID             string          `json:"actor_id"`
	ManifestHash        string          `json:"manifest_hash"`
	CanonicalHash       string          `json:"canonical_hash"`
	ScopeVersion        int             `json:"scope_version"`
	StrictGateSatisfied bool            `json:"strict_gate_satisfied"`
}

func capsuleSummary(c model.Capsule) capsuleIdentity {
	return capsuleIdentity{
		SessionID:           c.SessionID,
		ActorID:             c.ActorID,
		ManifestHash:        c.ManifestHash,
		CanonicalHash:       c.CanonicalHash,
		ScopeVersion:        c.ScopeVersion,
		StrictGateSatisfied: c.StrictGateSatisfied,
	}
}

// writeCapsuleFile writes the capsule and reports how many bytes reached disk.
// O_EXCL is the refusal: it is the only check that cannot lose a race with
// another writer between a stat and a create, which matters precisely because
// the file this command must never clobber may be a source file. A failed write
// removes the partial file rather than leaving something that looks like a
// capsule but is half of one.
func writeCapsuleFile(path string, capsule model.Capsule) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return 0, errOutputExists()
		}
		return 0, &model.Error{Code: model.CodeInternal,
			Message: "failed to create the capsule file: " + clip(err.Error(), model.MaxDetailBytes)}
	}
	counter := &countingWriter{w: f}
	encErr := json.NewEncoder(counter).Encode(capsule)
	closeErr := f.Close()
	if encErr == nil && closeErr == nil {
		return counter.n, nil
	}
	reason := closeErr
	if encErr != nil {
		reason = encErr
	}
	// The path was created by this command, so removing it destroys nothing the
	// operator had.
	_ = os.Remove(path)
	return 0, &model.Error{Code: model.CodeInternal,
		Message: "failed to write the capsule file: " + clip(reason.Error(), model.MaxDetailBytes)}
}

// screenCapsuleOutput refuses a --output path that lands inside the repository
// this command is about. A capsule is a record *about* the repository, never a
// file in it: Section 6 is explicit that codectx writes no file into the tree
// it indexes, and a capsule dropped into the working tree would be picked up as
// source by the next index, or committed by accident, or both. O_EXCL cannot
// stand in for this check -- the path it would refuse is one that already
// exists, and the path refused here is one that does not.
//
// The repository is discovered exactly as the workspace open discovers it
// (workspace.Discover), so the root refused here is the root that would be
// indexed rather than a second answer to the same question.
func screenCapsuleOutput(cmd *cobra.Command, output string) error {
	repo, err := repoFlagValue(cmd)
	if err != nil {
		return err
	}
	root, err := workspace.Discover(repo)
	if err != nil {
		return err
	}
	defer root.Close()
	abs, err := filepath.Abs(output)
	if err != nil {
		return &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s cannot be resolved to an absolute path: %s",
				contextOutputFlag, clip(err.Error(), model.MaxDetailBytes))}
	}
	// The comparison is between resolved directories rather than the strings
	// the operator typed: a parent directory that is a symlink into the
	// repository names a path inside it that no lexical prefix test would see.
	// Only the parent is resolved, because the file itself must not exist yet.
	if underDir(resolvedDir(filepath.Dir(abs)), resolvedDir(root.Path)) {
		return errOutputInsideRepository()
	}
	return nil
}

// resolvedDir is dir with its symlinks resolved. A directory that does not
// exist yet cannot be resolved, so the deepest ancestor that does exist is
// resolved and the remaining lexical segments are re-attached to it. Resolving
// only whole existing paths would be worse than useless here: it would leave
// one side of the caller's comparison resolved and the other lexical whenever
// the output's parent has still to be created, which is exactly the case the
// caller screens, and a repository reached through a symlinked ancestor would
// then pass the test it must fail.
func resolvedDir(dir string) string {
	existing, missing := dir, []string(nil)
	for {
		if resolved, err := filepath.EvalSymlinks(existing); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// The filesystem root itself did not resolve: nothing below it can
			// be put in resolved form, so both sides stay lexical.
			return dir
		}
		missing = append([]string{filepath.Base(existing)}, missing...)
		existing = parent
	}
}

// underDir reports whether dir is root itself or lies beneath it.
func underDir(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		// Different volumes on Windows: nothing beneath the root can be named
		// relative to it, so the path is outside it.
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// errOutputInsideRepository names the refusal without naming the root: an
// absolute private path has no place in an error a machine consumer stores.
func errOutputInsideRepository() error {
	return &model.Error{Code: model.CodeArgumentInvalid,
		Message:     "--" + contextOutputFlag + " names a path inside the repository; export never writes into the tree it indexes",
		Remediation: "write the capsule outside the repository, then attach or copy it wherever the record belongs"}
}

// errOutputExists is the one refusal both the pre-open screen and O_EXCL
// report, so an operator is told the same thing whichever of the two caught it.
func errOutputExists() error {
	return &model.Error{Code: model.CodeArgumentInvalid,
		Message:     "--" + contextOutputFlag + " names a file that already exists; export never overwrites",
		Remediation: "choose a path that does not exist, or remove that file deliberately first"}
}

// countingWriter counts what actually reached the file, so the reported byte
// count is the file's size rather than the size of what was handed to the
// encoder.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// addExpectedVersionFlag declares --expected-version for the three commands
// that take the Section 17.1 compare-and-swap guard. subject names what the
// version guards, so each command's help says what is refused rather than
// repeating one generic sentence.
//
// No backquotes in a flag usage string: cobra reads the first backquoted word
// as the flag's type name, so "`context status`" would render the value
// placeholder as "context status" instead of "int".
func addExpectedVersionFlag(cmd *cobra.Command, subject string) {
	cmd.Flags().Int(contextExpectedVersionFlag, 0,
		"required: the state version this "+subject+" expects, as \"codectx context status\" last reported it; it is refused if the session has moved on")
}

// contextVersionValue reads one of the two version guards and rejects a value
// no version can have. Versions start at 1, so zero is not "the configured
// default" the budget flags of this package use: it is a version that cannot
// exist, and an omitted flag is exactly that. Rejecting it here names the flag
// the operator has to fix instead of surfacing a field name from inside the
// service.
func contextVersionValue(cmd *cobra.Command, flag, kind, session string) (int, error) {
	v, err := intFlag(cmd, flag)
	if err != nil {
		return 0, err
	}
	if v < 1 {
		return 0, &model.Error{Code: model.CodeArgumentInvalid,
			Message: fmt.Sprintf("--%s is %d; %s versions start at 1", flag, v, kind),
			Remediation: fmt.Sprintf("pass the %s_version that `codectx context status %s` reports",
				kind, clip(session, model.IDHexLen))}
	}
	return v, nil
}

// writePlanResult renders the compiled manifest: what was selected, under which
// budget, and how complete the selection honestly is. The manifest id is
// printed whole because it is what identifies this plan; the request and
// canonical hashes are shortened because nothing asks the operator to retype
// them.
func writePlanResult(b *strings.Builder, result model.PlanResult) {
	m := result.Manifest
	fmt.Fprintf(b, "manifest    %s\nrequest     %s\ncanonical   %s\n",
		m.ID, shortID(m.RequestHash), shortID(m.CanonicalHash))
	fmt.Fprintf(b, "policy      %s\nestimate    %s\n", tableCell(m.PolicyVersion), tableCell(m.EstimateMethod))
	fmt.Fprintf(b, "selected    %d %s in %d %s\n",
		m.EntryCount, plural(m.EntryCount, "entry", "entries"),
		m.SliceCount, plural(m.SliceCount, "slice", "slices"))
	fmt.Fprintf(b, "budget      %d tokens, %d bytes, %d files, %d slices (0 is the configured default)\n",
		m.Budget.MaxEstimatedTokens, m.Budget.MaxBytes, m.Budget.MaxFiles, m.Budget.MaxSlices)
	writeManifestNotices(b, m.Notices)
}

// writeManifestNotices prints the compile's non-fatal disclosures -- a page
// size the configuration asked for and could not have, and the counts of the
// explanation cuts the compile applied.
//
// They are printed on the text path and not only in JSON because an operator
// reading a plan in a terminal is exactly the reader who needs to know the
// explanation is shorter than the walk that produced it. A notice is not a
// failure, so it does not change the exit status; it is labelled "notice"
// rather than "warning" for that reason, beside the "warning" the session
// block uses for a superseded snapshot.
func writeManifestNotices(b *strings.Builder, notices []string) {
	for _, note := range notices {
		if strings.TrimSpace(note) == "" {
			continue
		}
		// Printed whole, not through tableCell: a notice that says what the
		// compile could not carry must not itself be clipped. This is the
		// shape emitQuery already uses for meta.Notices.
		fmt.Fprintf(b, "notice      %s\n", note)
	}
}

// writeContextPage renders one page of whichever manifest projection was asked
// for. Only the declared view carries records, so exactly one of these lists is
// ever non-empty.
func writeContextPage(b *strings.Builder, page model.ContextPage) {
	fmt.Fprintf(b, "session     %s\nmanifest    %s\nview        %s\n", page.SessionID, page.ManifestID, page.View)
	switch page.View {
	case model.ViewEntries:
		if len(page.Entries) == 0 {
			b.WriteString("entries     none on this page\n")
			return
		}
		fmt.Fprintf(b, "entries     %d on this page\n", len(page.Entries))
		for _, e := range page.Entries {
			fmt.Fprintf(b, "  %5d  %-14s score %d  %d bytes  %d tokens  %s\n",
				e.Ordinal, tableCell(string(e.Requirement)), e.ScoreMicros,
				e.EstimatedBytes, e.EstimatedTokens, contextEntrySubject(e))
		}
	case model.ViewSlices:
		if len(page.Slices) == 0 {
			b.WriteString("slices      none on this page\n")
			return
		}
		fmt.Fprintf(b, "slices      %d on this page\n", len(page.Slices))
		for _, s := range page.Slices {
			fmt.Fprintf(b, "  %5d  %d %s  %d bytes  %d tokens\n",
				s.Index, len(s.EntryOrdinals), plural(len(s.EntryOrdinals), "entry", "entries"),
				s.EstimatedBytes, s.EstimatedTokens)
		}
	case model.ViewExcluded:
		if len(page.Excluded) == 0 {
			b.WriteString("excluded    none on this page\n")
			return
		}
		fmt.Fprintf(b, "excluded    %d on this page\n", len(page.Excluded))
		for _, e := range page.Excluded {
			fmt.Fprintf(b, "  %5d  %s\n", e.Ordinal, tableCell(e.Reason))
		}
	}
}

// contextEntrySubject names what one entry selected. The context_entries check
// is an OR, not an XOR -- a whole file is selected without a node and a symbol
// is selected within its file -- so both are printed when both are present.
func contextEntrySubject(e model.ContextEntry) string {
	switch {
	case e.NodeID != "" && e.FileID != "":
		return string(e.NodeID) + " in " + string(e.FileID)
	case e.NodeID != "":
		return string(e.NodeID)
	default:
		return string(e.FileID)
	}
}

// writeCapsulePage renders one page of whichever capsule list was asked for.
func writeCapsulePage(b *strings.Builder, page model.CapsulePage) {
	fmt.Fprintf(b, "session     %s\nmanifest    %s\ncapsule     %s\nview        %s\n",
		page.SessionID, shortID(page.ManifestHash), page.CanonicalHash, page.View)
	switch page.View {
	case model.CapsuleViewAcceptedFacts, model.CapsuleViewRejectedFacts:
		facts := page.AcceptedFacts
		if page.View == model.CapsuleViewRejectedFacts {
			facts = page.RejectedFacts
		}
		if len(facts) == 0 {
			b.WriteString("facts       none on this page\n")
			return
		}
		fmt.Fprintf(b, "facts       %d on this page\n", len(facts))
		for _, f := range facts {
			fmt.Fprintf(b, "  %s  observed %s  %d evidence\n", f.RelationID, shortID(string(f.ObservationID)),
				len(f.EvidenceIDs))
		}
	case model.CapsuleViewContradictions, model.CapsuleViewUnresolved:
		items := page.Contradictions
		if page.View == model.CapsuleViewUnresolved {
			items = page.Unresolved
		}
		if len(items) == 0 {
			b.WriteString("items       none on this page\n")
			return
		}
		fmt.Fprintf(b, "items       %d on this page\n", len(items))
		for _, o := range items {
			fmt.Fprintf(b, "  %s  %-14s %d %s  %s\n", o.ObservationID, tableCell(string(o.Kind)),
				len(o.References), plural(len(o.References), "reference", "references"), tableCell(o.Note))
		}
	case model.CapsuleViewCoverage:
		writeCoverageTable(b, page.Coverage)
	case model.CapsuleViewWaivers:
		if len(page.Waivers) == 0 {
			b.WriteString("waivers     none on this page\n")
			return
		}
		fmt.Fprintf(b, "waivers     %d on this page\n", len(page.Waivers))
		for _, w := range page.Waivers {
			writeWaiver(b, w)
		}
	}
}

// writeWaiver renders one stored waiver. The reason is printed because a waiver
// whose justification is not shown is indistinguishable from an unexplained gap.
func writeWaiver(b *strings.Builder, w model.WaiverRecord) {
	fmt.Fprintf(b, "waived      %s at %s\n", w.FileID, shortID(w.ContentHash))
	fmt.Fprintf(b, "reason      %s\n", tableCell(w.Reason))
	fmt.Fprintf(b, "recorded    %s by %s\n", w.CreatedAt.UTC().Format(time.RFC3339), tableCell(w.ActorID))
}

// writeObservation renders the stored record. The id is printed whole because
// it is what a capsule cites and what proves a resubmission was idempotent.
func writeObservation(b *strings.Builder, o model.Observation) {
	fmt.Fprintf(b, "observation %s\nkind        %s\n", o.ID, o.Kind)
	fmt.Fprintf(b, "scope       version %d\n", o.ScopeVersion)
	fmt.Fprintf(b, "references  %d\n", len(o.References))
	if o.Review != nil {
		fmt.Fprintf(b, "review      %d of the required categories, bound to manifest %s at scope version %d\n",
			len(o.Review.Entries), shortID(o.Review.ManifestHash), o.Review.ScopeVersion)
		for _, e := range o.Review.Entries {
			blocking := ""
			if e.Blocking {
				blocking = "  blocking"
			}
			fmt.Fprintf(b, "  %-28s %d %s%s\n", tableCell(string(e.Category)),
				len(e.References), plural(len(e.References), "reference", "references"), blocking)
		}
	}
	fmt.Fprintf(b, "note        %s\n", tableCell(o.Note))
	fmt.Fprintf(b, "recorded    %s\n", o.CreatedAt.UTC().Format(time.RFC3339))
}

// writeWorkflowStatus renders the transition's own answer: the new state and
// the version the next compare-and-swap has to present.
func writeWorkflowStatus(b *strings.Builder, s model.WorkflowStatus) {
	fmt.Fprintf(b, "state       %s in phase %s (state version %d, scope version %d)\n",
		s.State, s.Phase, s.StateVersion, s.ScopeVersion)
	fmt.Fprintf(b, "manifest    %s\n", s.ManifestID)
}
