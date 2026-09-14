package cli

import (
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

// initForceFlag is the one thing that lets `init` overwrite project
// configuration. It is a flag rather than a prompt because this command is run
// by agents as often as by people, and a prompt an agent cannot answer is a
// hang; it is required rather than implied because this is the only ordinary
// command that writes project configuration, and a silent overwrite would
// destroy settings nothing else in this build can recover.
const initForceFlag = "force"

// newInitCommand builds `codectx init`.
//
// It writes the project configuration file and nothing else: it opens no
// database, captures no snapshot, starts no analyzer and reads no source. That
// is why it takes no service -- `init` is deliberately not a facade operation
// (digest 17 Section 4), and it is the reason this command runs in a directory
// that is not yet a workspace, where every other command refuses.
func newInitCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Write the project configuration file for a repository",
		Long: "Writes " + config.ProjectConfigName + " at the root of the named repository, or of " +
			"the current directory when no path is given, carrying the keys an " +
			"operator is expected to set. Every other key keeps the built-in " +
			"default, so the file stays short enough to read.\n\n" +
			"This is the only ordinary command that writes project configuration. " +
			"An existing file is never overwritten without --force, and nothing " +
			"else on disk is touched: no source file is written, no cache is " +
			"created and no index is built. Run `codectx index` afterwards to " +
			"capture the workspace.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The body is delivered by this task's INIT lane. It is a typed
			// refusal rather than a panic so a caller that reaches it gets the
			// one envelope Section 18.2 promises and an exit class that says
			// "defect in this build" instead of a stack trace on stderr.
			return &model.Error{Code: model.CodeInternal,
				Message:     "cli: init has no writer in this build",
				Remediation: "this command waits on the project configuration writer"}
		},
	}
	cmd.Flags().Bool(initForceFlag, false,
		"overwrite an existing "+config.ProjectConfigName+" instead of refusing")
	return cmd
}
