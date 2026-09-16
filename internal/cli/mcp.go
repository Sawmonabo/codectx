package cli

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/mcpserver"
	"github.com/Sawmonabo/codectx/internal/model"
)

// newMCPCommand builds `codectx mcp serve`, the Section 18.1 spelling of the
// Section 19 server. `mcp` itself runs nothing: it exists so the one server
// subcommand has a place to live beside any later `mcp` verb.
func newMCPCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this workspace to an MCP client",
		Long: "Exposes the Section 19.2 tool set over the Model Context Protocol. " +
			"The transport is stdio and is not selectable here: mcp.transport " +
			"accepts no other value.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonRequested(cmd, args) {
				return &model.Error{Code: model.CodeArgumentInvalid,
					Message: "no mcp subcommand given; run \"codectx mcp --help\" for the list"}
			}
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMCPServeCommand(build))
	return cmd
}

// newMCPServeCommand builds `codectx mcp serve --repo PATH [--watch]`.
//
// The server owns ONE workspace and ONE *app.Services for the whole process,
// unlike every other command here, which opens a report workspace per
// invocation. That is forced by what the tool set does: codectx_refresh_index
// publishes generations, so the process must be the Section 13.2 cross-process
// owner and hold the workspace lock for its lifetime, and a *Services is valid
// only for the workspace it was made from. The facade is documented safe for
// concurrent use over one workspace (services.go, Task 19 Q2), which is what
// lets the bounded set of concurrent tool calls share this single instance.
//
// stdout belongs to the SDK's framing for this whole invocation: every
// diagnostic AND every refusal this command emits goes to stderr, including the
// --json failure envelope for a pre-server rejection such as a busy workspace.
// Section 18.2's envelope is written to the root command's output stream, so
// claimProtocolStdout redirects that stream to stderr rather than letting a
// machine-readable refusal land on the channel the protocol owns.
func newMCPServeCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server over stdio for one workspace",
		Long: "Serves one workspace over stdio until the client disconnects or the " +
			"process is interrupted. The workspace lock is held for the whole " +
			"session -- one writer, never two -- so a workspace another `codectx " +
			"index`, `refresh` or `watch` already holds is reported as busy at " +
			"startup instead of being waited on forever.\n\n" +
			"Results and errors travel as MCP tool answers on the protocol " +
			"stream; logs and startup failures go to stderr.",
		Args: func(cmd *cobra.Command, args []string) error {
			// Cobra validates the positional arguments after it has parsed the
			// flags and before any Run hook, so this is the earliest point on
			// the path that survives flag parsing. Claiming stdout here rather
			// than inside RunE covers the argument rejection too.
			claimProtocolStdout(cmd)
			return cobra.NoArgs(cmd, args)
		},
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoFlagValue(cmd)
			if err != nil {
				return err
			}
			ws, err := app.OpenWorkspace(cmd.Context(), repo, app.OpenOptions{Wait: indexLockWait})
			if err != nil {
				return err
			}
			defer ws.Close()

			// The configuration is the one the open already resolved. Loading
			// the files a second time here would be a second answer to one
			// question, and it would buy nothing: the open loads configuration
			// before it takes the workspace lock, so a rejected configuration
			// still fails without ever making the workspace busy.
			// mcp.transport is not re-checked -- config validation already
			// rejects anything but stdio, and a second check would be a branch
			// nothing can reach.
			cfg := ws.Config()
			// mcp.watch is the default and --watch is the override. The flag is
			// declared false and resolved by Changed() rather than defaulted to
			// the configured value, because a cobra default is baked into
			// --help: printing "(default true)" for a repository whose
			// mcp.watch is false would be help text that lies.
			watch := cfg.MCP.Watch
			if cmd.Flags().Changed(indexWatchFlag) {
				if watch, err = boolFlag(cmd, indexWatchFlag); err != nil {
					return err
				}
			}

			// The logger is the process's one diagnostic channel and it is
			// bound to stderr: a byte of ours on stdout corrupts the framing.
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			svc := ws.Services()
			server, err := mcpserver.New(mcpserver.Options{
				Index:   svc,
				Explore: svc,
				Context: svc,
				Config:  cfg,
				Build:   build,
				// The server reports progress and logs stages from the one run
				// ledger this workspace composed. It never opens a ledger of
				// its own: the file has a single writer, and a second one would
				// contend with the very run these notifications describe.
				// IndexRun names the run those stages belong to, so a call
				// following an index counts its stages and not those of the
				// per-process overlay run a language server's start opens
				// beside it.
				Spans:    ws.Spans,
				IndexRun: ws.IndexRunID,
				Logger:   log,
			})
			if err != nil {
				return err
			}
			return serveMCP(cmd.Context(), ws, server, log, watch)
		},
	}
	// A flag Cobra could not parse never reaches Args or RunE -- it is
	// refused inside ParseFlags -- so the claim has to be made from the flag
	// error hook as well. Without it `codectx mcp serve --bogus --json` writes
	// its refusal envelope to stdout, the one channel this command may not
	// write a byte of its own to. The typed rejection is the root's
	// (root.go, SetFlagErrorFunc) restated verbatim rather than varied:
	// setting a hook on this command REPLACES the inherited one instead of
	// wrapping it, and a divergence here would type one command's flag
	// refusals differently from every other command's.
	//
	// `--help` is unaffected: Cobra answers the help flag before Args runs, so
	// help still renders on stdout, which is what an operator asked for and
	// not a refusal on a stream the protocol owns. The `mcp` parent needs
	// neither hook -- it frames no protocol stream.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		claimProtocolStdout(c)
		return &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	})
	addRepoFlag(cmd)
	// The flag name is the one `index` and `watch` already use: "keep refreshing
	// as the workspace changes" is one question, and a second spelling of it
	// would be two contracts for one.
	cmd.Flags().Bool(indexWatchFlag, false,
		"refresh the index as the workspace changes for the life of the session; defaults to the mcp.watch setting, which is on unless the configuration turns it off")
	return cmd
}

// claimProtocolStdout hands the root command's output stream to stderr for the
// rest of this invocation. Execute writes the Section 18.2 failure envelope to
// the ROOT command's stream, not this command's, so the redirect has to be on
// the root: a --json rejection on this path -- an unparsable flag, a stray
// positional argument, a bad --repo, a workspace another writer holds --
// otherwise emits a JSON object on the channel the SDK is about to frame.
func claimProtocolStdout(cmd *cobra.Command) {
	cmd.Root().SetOut(cmd.ErrOrStderr())
}

// serveMCP runs the session: the optional watch loop beside the server, both
// under one cancelable context, with the watcher joined before the workspace
// closes.
//
// A watch that FAILS stops the session. The alternative -- log it and keep
// serving -- leaves a server that was asked to track the workspace answering
// from a generation it has stopped refreshing, and no tool answer says so; a
// client cannot tell that apart from a workspace that simply has not changed.
// Ending the session reports it where the operator is already looking.
func serveMCP(ctx context.Context, ws *app.Workspace, server *mcpserver.Server, log *slog.Logger, watch bool) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	var wg sync.WaitGroup
	var watchErr error
	if watch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each refresh is a log record on stderr, never a line on stdout:
			// this is the same event `codectx watch` prints, on the only
			// channel this process may use for it.
			watchErr = ws.Coordinator().Watch(ctx, func(result model.IndexResult) {
				log.Info("refreshed",
					"generation", int64(result.Binding.GenerationID),
					"health", string(result.Health),
					"reused", result.UnitsReused,
					"built", result.UnitsBuilt,
					"invalidated", result.UnitsInvalidated)
			})
			if watchErr != nil && !isCanceled(watchErr) {
				stop()
			}
		}()
	}

	serveErr := server.Serve(ctx)
	// The server has returned, so the watcher has no session left to refresh
	// for: stop it and join it before the deferred Close tears the workspace
	// out from under it.
	stop()
	wg.Wait()

	if serveErr != nil {
		return serveFailure(serveErr)
	}
	// Serve reports a canceled context as the clean shutdown it is, so a watch
	// failure is the only remaining way this session ended badly.
	if watchErr != nil && !isCanceled(watchErr) {
		return serveFailure(watchErr)
	}
	return nil
}

// serveFailure types what the session returned. Section 18.2's envelope and
// ExitCode are built on *model.Error, and the SDK's transport errors are
// untyped, so an unwrapped one would be reported as an argument rejection --
// the exit-2 class -- for a session that failed while running.
func serveFailure(err error) error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed
	}
	return &model.Error{Code: model.CodeInternal, Message: "the MCP session ended: " + err.Error()}
}
