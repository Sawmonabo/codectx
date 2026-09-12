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
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	build := model.CurrentBuildInfo()
	root := cli.NewRoot(build, os.Stdout, os.Stderr)
	err := cli.Execute(ctx, build, root, os.Args[1:])
	stop()
	os.Exit(cli.ExitCode(err))
}
