// Package cli translates command-line requests and renders results. It holds
// no product logic and, for version and help, opens no database, loads no
// grammar and detects no analyzer (Section 7.2).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

const jsonFlag = "json"

// NewRoot builds the command tree. stdout carries results, including JSON
// envelopes; stderr carries logs and human error text only.
func NewRoot(build model.BuildInfo, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:   "codectx",
		Short: "Codebase intelligence for development agents",
		Long: "codectx indexes a local repository and answers bounded structural " +
			"queries offline, through this CLI and an MCP server.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Help is human output. A machine consumer that asks for JSON
			// without naming a command gets the envelope, not usage text.
			if jsonRequested(cmd, args) {
				return &model.Error{Code: model.CodeArgumentInvalid, Message: "no command given; run \"codectx --help\" for the command list"}
			}
			return cmd.Help()
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.PersistentFlags().Bool(jsonFlag, false, "emit one machine-stable JSON envelope on stdout")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	})
	root.AddCommand(newVersionCommand(build))
	root.AddCommand(newToolsCommand(build))
	return root
}

// Execute runs the command tree and reports any failure through the single
// envelope policy of Section 18.2. The failure is returned, already rendered,
// for the process boundary to map with ExitCode.
func Execute(ctx context.Context, build model.BuildInfo, root *cobra.Command, args []string) error {
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(ctx)
	if err == nil {
		return nil
	}
	if cmd == nil {
		cmd = root
	}
	// Every command Run path returns *model.Error, so an untyped error can only
	// be Cobra rejecting the flags or the command name: the exit-2 class.
	var typed *model.Error
	if !errors.As(err, &typed) {
		typed = &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	if jsonRequested(cmd, args) {
		if writeErr := writeEnvelope(root.OutOrStdout(), failureEnvelope(build.SchemaVersion, cmd.Name(), typed)); writeErr != nil {
			return writeErr
		}
		return typed
	}
	fmt.Fprintln(root.ErrOrStderr(), "Error:", typed.Message)
	return typed
}

// jsonRequested reports whether machine-readable output was asked for. The
// parsed flag is authoritative, but flag parsing stops at the offending
// argument, so a rejected command falls back to the raw arguments.
func jsonRequested(cmd *cobra.Command, args []string) bool {
	// GetBool reads the parsed value, so an explicit --json=false is honored.
	if v, err := cmd.Flags().GetBool(jsonFlag); err == nil && v {
		return true
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--"+jsonFlag || a == "--"+jsonFlag+"=true" {
			return true
		}
	}
	return false
}

func newVersionCommand(build model.BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		Short:         "Print the build identity of this binary",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// BuildInfo carries the explicit wire tags, so the version
			// payload is the build record itself rather than a second copy of
			// the same four fields.
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, cmd.Name(), build))
			}
			_, err := fmt.Fprintf(out, "codectx %s (commit %s, %s, schema %s)\n",
				build.Version, build.Commit, build.Toolchain, build.SchemaVersion)
			if err != nil {
				return &model.Error{Code: model.CodeInternal, Message: "failed to write output: " + err.Error()}
			}
			return nil
		},
	}
}
