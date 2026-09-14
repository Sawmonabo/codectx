// Package cli translates command-line requests and renders results. It holds
// no product logic and, for version and help, opens no database, loads no
// grammar and detects no analyzer (Section 7.2).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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
	root.AddCommand(newInitCommand(build))
	root.AddCommand(newToolsCommand(build))
	root.AddCommand(newIndexCommand(build))
	root.AddCommand(newRefreshCommand(build))
	root.AddCommand(newStatusCommand(build))
	root.AddCommand(newWatchCommand(build))
	root.AddCommand(newSearchCommand(build))
	root.AddCommand(newSymbolCommand(build))
	root.AddCommand(newMCPCommand(build))
	// The Section 18.1 query commands are built as a set so query.go never
	// edits the command tree it belongs to.
	for _, c := range newQueryCommands(build) {
		root.AddCommand(c)
	}
	// The Section 18.1 context commands, built as a set for the same reason.
	for _, c := range newContextCommands(build) {
		root.AddCommand(c)
	}
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
		if writeErr := writeEnvelope(root.OutOrStdout(), failureEnvelope(build.SchemaVersion, commandName(cmd), typed)); writeErr != nil {
			return writeErr
		}
		return typed
	}
	fmt.Fprintln(root.ErrOrStderr(), "Error:", typed.Message)
	if typed.Remediation != "" {
		// The remediation is where a rejection puts what the operator has to act
		// on -- the candidate ids of an ambiguous name, the key to raise. On the
		// --json path it rides in the envelope; without this line the text path
		// is the only consumer that is told less.
		fmt.Fprintln(root.ErrOrStderr(), " ", typed.Remediation)
	}
	return typed
}

// commandName is the envelope's `command` field: the command path with the
// root binary's own name removed, so `codectx tools status` is "tools status"
// and `codectx status` is "status". Both used to report "status" for two
// different data shapes, which left a consumer unable to tell which report it
// was decoding. The root itself keeps its own name: an argument the tree could
// not route to any command belongs to the binary.
func commandName(cmd *cobra.Command) string {
	path, root := cmd.CommandPath(), cmd.Root().Name()
	if path == root {
		return root
	}
	return strings.TrimPrefix(path, root+" ")
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
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), build))
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
