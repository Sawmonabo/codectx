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
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	build := model.CurrentBuildInfo()
	root := cli.NewRoot(build, os.Stdout, os.Stderr)
	err := cli.Execute(ctx, build, root, os.Args[1:])
	stop()
	os.Exit(cli.ExitCode(err))
}
