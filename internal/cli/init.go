package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// initResult is the `init` payload. Both fields are facts about what this run
// did to the filesystem, which is the whole of what the command does: where the
// file is now, and whether an existing one was replaced to put it there.
type initResult struct {
	Path        string `json:"path"`
	Overwritten bool   `json:"overwritten"`
}

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
			"the current directory when no path is given. The file declares the " +
			"configuration version it is written against and carries every key a " +
			"repository is permitted to set, commented out beside the built-in " +
			"default, so the whole permitted surface is visible without guessing " +
			"and no value is asserted on the operator's behalf.\n\n" +
			"This is the only ordinary command that writes project configuration. " +
			"An existing file is never overwritten without --force, and nothing " +
			"else on disk is touched: no source file is written, no cache is " +
			"created and no index is built. Run `codectx index` afterwards to " +
			"capture the workspace.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd, args, build)
		},
	}
	cmd.Flags().Bool(initForceFlag, false,
		"overwrite an existing "+config.ProjectConfigName+" instead of refusing")
	return cmd
}

// runInit resolves the target, writes the file and renders the one result.
func runInit(cmd *cobra.Command, args []string, build model.BuildInfo) error {
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return &model.Error{Code: model.CodeArgumentInvalid,
			Message:     fmt.Sprintf("the path %q cannot be resolved", root),
			Remediation: "Give an existing directory, or no argument to use the current one."}
	}
	info, err := os.Stat(absRoot)
	if err != nil || !info.IsDir() {
		// A path that is not a directory is an argument the operator typed, not
		// a workspace that is missing: `init` is the one command that runs
		// before a workspace exists, so it can never report CTX_WORKSPACE_NOT_FOUND.
		return &model.Error{Code: model.CodeArgumentInvalid,
			Message:     fmt.Sprintf("%q is not an existing directory", root),
			Remediation: "Give an existing directory, or no argument to use the current one."}
	}

	force, err := boolFlag(cmd, initForceFlag)
	if err != nil {
		return err
	}
	path := filepath.Join(absRoot, config.ProjectConfigName)
	overwritten, err := writeProjectConfig(path, force)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	result := initResult{Path: path, Overwritten: overwritten}
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), result))
	}
	verb := "Wrote"
	if overwritten {
		verb = "Replaced"
	}
	if _, err := fmt.Fprintf(out, "%s %s\nRun `codectx index` to capture the workspace.\n", verb, path); err != nil {
		// The file is already on disk, so the failure is the report of it, not
		// the work: outputFailure keeps a broken pipe out of the defect class
		// exactly as it does for the --json path.
		return outputFailure(err)
	}
	return nil
}

// writeProjectConfig writes the file, reporting whether it replaced one.
//
// The refusal is the open itself: O_EXCL is what makes "refuses to overwrite"
// true rather than merely likely. A Stat-then-write check would leave the
// window in which a file appears between the two calls, and this is the one
// ordinary command that can destroy settings an operator tuned by hand.
func writeProjectConfig(path string, force bool) (bool, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	overwritten := false
	if force {
		if _, err := os.Stat(path); err == nil {
			overwritten = true
		}
	}
	// 0o644: the project file is committed alongside the repository and read by
	// everyone who can read the checkout. It carries no secret -- every key it
	// may set is source-inclusion or budget policy (Section 20.2), and the
	// trusted half of the configuration lives in the user's own directory.
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, &model.Error{Code: model.CodeArgumentInvalid,
				Message:     config.ProjectConfigName + " already exists in this repository",
				Remediation: "Pass --force to replace it. The existing file is not merged: its settings are lost."}
		}
		return false, &model.Error{Code: model.CodeInternal,
			Message:     "the project configuration file could not be created",
			Remediation: "Check that the directory is writable."}
	}
	if _, err := f.WriteString(projectConfigTemplate(config.Defaults())); err != nil {
		f.Close()
		return false, &model.Error{Code: model.CodeInternal,
			Message:     "the project configuration file could not be written",
			Remediation: "Check the free space and permissions of the directory."}
	}
	if err := f.Close(); err != nil {
		return false, &model.Error{Code: model.CodeInternal,
			Message:     "the project configuration file could not be completed",
			Remediation: "Check the free space and permissions of the directory."}
	}
	return overwritten, nil
}

// projectConfigTemplate renders the file from the build's own defaults.
//
// Exactly one key is set: `version`, which declares the configuration schema
// the file is written against and is the only value that cannot conflict with
// anything, because it is this build's own. Every other permitted key is
// written commented out beside the default it would replace. That is deliberate
// and it is the trade-off this command makes:
//
//   - A generated value is a decision the operator did not make. Five of the
//     eleven remaining keys are lower-only (load.go): a repository may narrow a
//     limit but never raise one, so a file that asserted this build's default
//     would fail to load for anyone whose user configuration had lowered the
//     same limit -- `init` would brick every later command in that checkout.
//   - `providers.tree_sitter.languages` written out freezes today's language
//     set into the repository, so a language a later build supports would be
//     silently switched off by a file the operator never edited.
//
// What the operator needs from this file is the permitted surface, which is
// otherwise unknowable: the keys a repository may set, each with the default it
// overrides. Uncommenting a line is one edit; recovering from a value that was
// asserted for them is not.
func projectConfigTemplate(d config.Config) string {
	var b strings.Builder
	b.WriteString("# codectx project configuration.\n")
	b.WriteString("#\n")
	b.WriteString("# This file is written by whoever can write the repository, so it carries the\n")
	b.WriteString("# least trust: it may set source inclusion, language selection and query\n")
	b.WriteString("# budgets, and nothing else. Executables, argv, environment, network, path\n")
	b.WriteString("# roots and the strict read gate are user-level settings only, and a project\n")
	b.WriteString("# file that sets one is rejected rather than ignored.\n")
	b.WriteString("#\n")
	b.WriteString("# Every key below is commented out and shows the built-in default. Uncomment\n")
	b.WriteString("# a line to override it. A limit marked \"lower only\" may be narrowed but\n")
	b.WriteString("# never raised above the effective user-level value.\n\n")

	b.WriteString("# The configuration schema this file is written against.\n")
	fmt.Fprintf(&b, "version = %d\n\n", d.Version)

	b.WriteString("[workspace]\n")
	b.WriteString("# Index files that git does not track.\n")
	fmt.Fprintf(&b, "# include_untracked = %t\n", d.Workspace.IncludeUntracked)
	b.WriteString("# Index files a build step produced.\n")
	fmt.Fprintf(&b, "# index_generated = %t\n", d.Workspace.IndexGenerated)
	b.WriteString("# Index vendored dependencies.\n")
	fmt.Fprintf(&b, "# index_vendor = %t\n", d.Workspace.IndexVendor)
	b.WriteString("# Most files one snapshot may carry (lower only).\n")
	fmt.Fprintf(&b, "# max_files = %d\n", d.Workspace.MaxFiles)
	b.WriteString("# Largest file that is parsed for structure (lower only).\n")
	fmt.Fprintf(&b, "# max_parse_file_bytes = %d\n", d.Workspace.MaxParseFileBytes)
	b.WriteString("# Largest file that is searched textually (lower only).\n")
	fmt.Fprintf(&b, "# max_search_file_bytes = %d\n\n", d.Workspace.MaxSearchFileBytes)

	b.WriteString("[providers.tree_sitter]\n")
	b.WriteString("# The languages parsed for structure. Narrowing this list switches the\n")
	b.WriteString("# others off for this repository, including any a later build adds.\n")
	fmt.Fprintf(&b, "# languages = [%s]\n\n", quotedList(d.Providers.TreeSitter.Languages))

	b.WriteString("[context]\n")
	b.WriteString("# The workflow phase a context request assumes when it names none.\n")
	fmt.Fprintf(&b, "# default_phase = %q\n", d.Context.DefaultPhase)
	b.WriteString("# The token budget a context plan assumes when it names none (lower only).\n")
	fmt.Fprintf(&b, "# default_estimated_tokens = %d\n", d.Context.DefaultEstimatedTokens)
	b.WriteString("# The byte budget a context plan assumes when it names none (lower only).\n")
	fmt.Fprintf(&b, "# default_max_bytes = %d\n", d.Context.DefaultMaxBytes)
	b.WriteString("# The file budget a context plan assumes when it names none (lower only).\n")
	fmt.Fprintf(&b, "# default_max_files = %d\n", d.Context.DefaultMaxFiles)
	return b.String()
}

// quotedList renders a string slice as a TOML array in the order given, which
// is the order the defaults declare: a rendered list that reordered itself
// between two runs would make this command's output unstable for no reason.
func quotedList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, strconv.Quote(v))
	}
	return strings.Join(quoted, ", ")
}
