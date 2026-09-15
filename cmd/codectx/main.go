// Command codectx is the process boundary: it installs signal cancellation,
// builds the command tree and turns the resulting error into an exit code.
// Nothing heavier is initialized here, so version and help stay cheap.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Sawmonabo/codectx/internal/cli"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

func main() {
	// The parser worker re-executes this binary under a hidden argv[1]. It is
	// dispatched before any flag parsing, logging or configuration so a worker
	// never inherits parent state or writes to the parent's streams.
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		stop := startProfiling("worker")
		code := worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}

	// A write to fd 1 or 2 that returns EPIPE raises SIGPIPE, whose default
	// disposition kills the process with status 141 before the write error is
	// ever returned. Ignoring it turns the broken pipe back into the EPIPE the
	// output path already types (Section 18.2: a broken stdout pipe fails the
	// command and confirms no delivery), so `codectx ... | head` exits 7 with a
	// typed envelope instead of being killed by a signal. It is installed after
	// the worker dispatch so a re-executed parser worker keeps the default.
	signal.Ignore(syscall.SIGPIPE)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	stopProfiling := startProfiling("main")

	build := model.CurrentBuildInfo()
	root := cli.NewRoot(build, os.Stdout, os.Stderr)
	err := cli.Execute(ctx, build, root, os.Args[1:])
	stop()
	stopProfiling()
	os.Exit(cli.ExitCode(err))
}
