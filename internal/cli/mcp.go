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
// owner while it refreshes, and a *Services is valid only for the workspace it
// was made from. The lock that ownership means is taken for the duration of a
// refresh rather than for the session, so the exploration tools -- which need
// neither it nor the writer -- answer from the moment the session opens,
// including while another process indexes, and an idle session leaves the
// workspace to the person's own commands. The facade is documented safe for
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
			"process is interrupted. Startup takes no workspace lock and writes " +
			"nothing, so the server comes up and answers beside an index another " +
			"process is already running. The lock is taken for the duration of a " +
			"refresh and given back when it ends -- one writer, never two, and " +
			"never a workspace this session keeps while it is idle -- so a " +
			"refresh asked for while another `codectx index`, `refresh` or " +
			"`watch` holds the workspace is the only thing reported as busy, and " +
			"the questions keep being answered throughout. A --watch session " +
			"is the same rule: it takes the lock for the beat that builds and " +
			"gives it back when that beat ends, so your own `codectx index` " +
			"runs beside it and the beat after yours reuses what it " +
			"published.\n\n" +
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
			// No lock wait: the open takes no lock, and the wait it would
			// carry is what a refresh would spend before answering. A client
			// asking for a refresh while the person's own index runs is
			// answered now, with the retryable refusal it can act on, rather
			// than held silent for a wait that cannot outlast that run; the
			// watch loop takes the lock for each beat that builds.
			// The operation the lock will carry is this session: whatever
			// the build behind it turns out to be -- a refresh a client asked
			// for, or a watch pass -- what the person needs to recognise on
			// the refusal is the server they left running.
			ws, err := app.OpenWorkspaceForServer(cmd.Context(), repo, app.OpenOptions{Operation: "mcp server"})
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
			// Two facades over one workspace, and the split is the point: the
			// refresh tool and the session tools write, so they are the
			// writing facade; the exploration tools only ask questions, so
			// they are the reader facade, which reaches no write transaction
			// and therefore neither commits this session's own refresh early
			// nor waits behind it.
			svc, read := ws.Services(), ws.ReadServices()
			server, err := mcpserver.New(mcpserver.Options{
				Index:   svc,
				Explore: read,
				Context: svc,
				Config:  cfg,
				Build:   build,
				Logger:  log,
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
