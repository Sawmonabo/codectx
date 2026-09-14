package cli

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/config"
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
// stdout belongs to the SDK's framing from the moment the server starts: every
// diagnostic this command emits goes to stderr, and a startup failure is
// reported before a single protocol byte is written.
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
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, err := repoFlagValue(cmd)
			if err != nil {
				return err
			}
			// Configuration is resolved BEFORE the workspace is opened: it
			// decides whether this session watches, and a rejection that first
			// took the workspace lock has already made the workspace busy for a
			// session it will not run. mcp.transport is not re-checked here --
			// config validation already rejects anything but stdio, and a
			// second check would be a branch nothing can reach.
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
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
			ws, err := app.OpenWorkspace(cmd.Context(), repo, indexLockWait, false)
			if err != nil {
				return err
			}
			defer ws.Close()

			// The logger is the process's one diagnostic channel and it is
			// bound to stderr: a byte of ours on stdout corrupts the framing.
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			svc := ws.Services()
			server, err := mcpserver.New(mcpserver.Options{
				Index:    svc,
				Explore:  svc,
				Context:  svc,
				Diagnose: svc,
				Config:   cfg,
				Build:    build,
				Logger:   log,
			})
			if err != nil {
				return err
			}
			return serveMCP(cmd.Context(), ws, server, log, watch)
		},
	}
	addRepoFlag(cmd)
	// The flag name is the one `index` and `watch` already use: "keep refreshing
	// as the workspace changes" is one question, and a second spelling of it
	// would be two contracts for one.
	cmd.Flags().Bool(indexWatchFlag, false,
		"refresh the index as the workspace changes for the life of the session; defaults to the mcp.watch setting, which is on unless the configuration turns it off")
	return cmd
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
