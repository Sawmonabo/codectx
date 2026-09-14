package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/spf13/cobra"
)

const (
	toolsRepoFlag = "repo"
	toolsAllFlag  = "all"
)

// toolEntry is one line of the `tools` report. It mirrors toolchain.Status with
// explicit wire tags, because the JSON shape of Section 18.1 is a CLI contract
// and must not drift with a field rename inside the toolchain package.
type toolEntry struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	State     string   `json:"state"`
	Languages []string `json:"languages"`
	Detail    string   `json:"detail,omitempty"`
}

// toolReport is the data payload of `tools status`, `tools prefetch` and
// `tools verify`. One shape for the three so a consumer parses the toolchain
// report once; `prefetch` reports the same rows restricted to what it installed,
// which is also its post-condition.
type toolReport struct {
	Platform string      `json:"platform"`
	Store    string      `json:"store"`
	Tools    []toolEntry `json:"tools"`
}

// toolsGCReport is the data payload of `tools gc`.
type toolsGCReport struct {
	Store   string `json:"store"`
	Removed int    `json:"removed"`
}

// newToolsCommand builds `codectx tools status|prefetch|verify|gc`, the
// lifecycle surface of Section 11.7. None of it is required for ordinary use:
// resolution installs what a repository needs on demand, and these commands
// exist so CI and offline hosts can do that work ahead of time, see what the
// store holds, re-check it, and reclaim what a binary upgrade superseded.
func newToolsCommand(build model.BuildInfo) *cobra.Command {
	tools := &cobra.Command{
		Use:   "tools",
		Short: "Inspect and manage the managed analyzer toolchain",
		Long: "The product owns every external analyzer and every runtime one needs, " +
			"pinned by the tool lock this binary embeds. These commands install, " +
			"report on and reclaim those payloads; none of them is a prerequisite " +
			"for indexing or querying.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if jsonRequested(cmd, args) {
				return &model.Error{Code: model.CodeArgumentInvalid,
					Message: "no tools subcommand given; run \"codectx tools --help\" for the list"}
			}
			return cmd.Help()
		},
	}
	// The store a tools command reports on is the one the repository at --repo
	// would resolve through, because the data directory is per workspace unless
	// tools.cache_dir names a shared one.
	tools.PersistentFlags().String(toolsRepoFlag, ".", "repository whose resolved configuration selects the tool store")

	status := &cobra.Command{
		Use:           "status",
		Short:         "Report every lock entry and what the store holds for it",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, store, err := openToolchain(cmd)
			if err != nil {
				return err
			}
			// Status is the cheap report: presence and publication, no rehashing,
			// so it stays usable for every lock entry including the 1.8 GB engine.
			return emitToolReport(cmd, build, args, report(store, res.Status(cmd.Context()), nil), nil)
		},
	}

	prefetch := &cobra.Command{
		Use:   "prefetch [--all | NAME...]",
		Short: "Install pinned payloads ahead of time",
		Long: "Installs the payloads a later run would fetch on demand. Name the " +
			"tools to install, or pass --all for every entry the lock carries for " +
			"this platform; one of the two is required, so a bare prefetch never " +
			"downloads gigabytes by accident.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := prefetchNames(cmd, args)
			if err != nil {
				return err
			}
			res, store, err := openToolchain(cmd)
			if err != nil {
				return err
			}
			failure := firstToolFailure(res.Prefetch(cmd.Context(), names))
			// The rows are the post-condition of the install, so they are read
			// back from the store rather than assumed from a nil error.
			return emitToolReport(cmd, build, args, report(store, res.Status(cmd.Context()), names), failure)
		},
	}
	prefetch.Flags().Bool(toolsAllFlag, false, "install every entry the lock carries for this platform")

	verify := &cobra.Command{
		Use:   "verify",
		Short: "Rehash every installed entry against the lock",
		Long: "Rehashes each installed entry executable against the digest the lock " +
			"pins for it, so a store a user edited or a disk damaged is reported " +
			"here rather than discovered at the next analyzer run. A corrupt entry " +
			"fails the command: a check that reports damage and exits 0 is a gate " +
			"that passes silently in CI.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, store, err := openToolchain(cmd)
			if err != nil {
				return err
			}
			rows, err := res.Verify(cmd.Context())
			data := report(store, rows, nil)
			if err == nil {
				err = corruptFailure(data.Tools)
			}
			return emitToolReport(cmd, build, args, data, err)
		},
	}

	gc := &cobra.Command{
		Use:   "gc",
		Short: "Remove store versions the current lock does not name",
		Long: "Removes every installed version the embedded lock no longer names, " +
			"plus unpublished and abandoned trees. This is what makes upgrading " +
			"the binary reclaim the previous release's payloads.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, store, err := openToolchain(cmd)
			if err != nil {
				return err
			}
			removed, err := res.GC(cmd.Context())
			if err != nil {
				return err
			}
			data := toolsGCReport{Store: store, Removed: removed}
			out := cmd.OutOrStdout()
			if jsonRequested(cmd, args) {
				return writeEnvelope(out, successEnvelope(build.SchemaVersion, cmd.Name(), data))
			}
			return writeText(out, "store     %s\nremoved   %d superseded tool %s\n",
				data.Store, data.Removed, plural(data.Removed, "version", "versions"))
		},
	}

	tools.AddCommand(status, prefetch, verify, gc)
	return tools
}

// openToolchain is the composition root of the managed toolchain: it resolves
// the layered configuration for the repository at --repo and builds one
// resolver over it. It lives here only until internal/app exists, at which
// point it moves there whole and the command tree receives the resolver.
func openToolchain(cmd *cobra.Command) (*toolchain.Resolver, string, error) {
	repo, err := cmd.Flags().GetString(toolsRepoFlag)
	if err != nil {
		return nil, "", &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	cfg, err := config.Load(repo)
	if err != nil {
		return nil, "", err
	}
	// toolchain.Options names a data directory and appends "tools" to it, and
	// config's data directory is per workspace; tools.cache_dir is how one store
	// is shared by every checkout on the machine.
	dataDir := cfg.Storage.DataDir
	if cfg.Tools.CacheDir != "" {
		dataDir = cfg.Tools.CacheDir
	}
	overrides := make(map[string]toolchain.Override, len(cfg.Tools.Override))
	for name, ov := range cfg.Tools.Override {
		overrides[name] = toolchain.Override(ov)
	}
	res, err := toolchain.New(toolchain.Options{
		DataDir:       dataDir,
		Offline:       cfg.Tools.Offline,
		Mirror:        cfg.Tools.Mirror,
		MaxFetchBytes: cfg.Tools.MaxFetchBytes,
		FetchTimeout:  cfg.Tools.FetchTimeout.Std(),
		Overrides:     overrides,
		// Section 11.7 requires one record per completed fetch in ordinary
		// operation; Section 18.2 puts logs on stderr, never on the result
		// stream a --json consumer reads.
		Log: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return nil, "", err
	}
	return res, toolchain.StoreDir(dataDir), nil
}

// prefetchNames validates the requested tools against the embedded lock. A name
// the lock does not carry is user input, not a product defect, so it is rejected
// here as an argument error rather than reaching Resolve, which reports an
// unknown name as CTX_INTERNAL because profiles name lock entries.
func prefetchNames(cmd *cobra.Command, args []string) ([]string, error) {
	all, err := cmd.Flags().GetBool(toolsAllFlag)
	if err != nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid, Message: err.Error()}
	}
	known := toolchain.Embedded().Names()
	switch {
	case all && len(args) > 0:
		return nil, (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "--all installs every pinned tool, so it cannot be combined with tool names"}).
			WithRemediation("run `codectx tools prefetch --all`, or name the tools without --all")
	case all:
		// A nil list is every entry the lock carries for this platform.
		return nil, nil
	case len(args) == 0:
		return nil, (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "prefetch needs the tools to install: pass --all or name them"}).
			WithRemediation("pinned tools: " + strings.Join(known, ", "))
	}
	valid := make(map[string]bool, len(known))
	for _, name := range known {
		valid[name] = true
	}
	seen := make(map[string]bool, len(args))
	names := make([]string, 0, len(args))
	for _, name := range args {
		if !valid[name] {
			return nil, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "no pinned tool is named " + fmt.Sprintf("%q", name)}).
				WithRemediation("pinned tools: " + strings.Join(known, ", "))
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// report converts the toolchain report into the CLI shape, keeping only the
// requested names when the caller asked for some.
func report(store string, rows []toolchain.Status, only []string) toolReport {
	wanted := make(map[string]bool, len(only))
	for _, name := range only {
		wanted[name] = true
	}
	out := toolReport{Platform: toolchain.Current().Key(), Store: store, Tools: make([]toolEntry, 0, len(rows))}
	for _, r := range rows {
		if len(wanted) > 0 && !wanted[r.Name] {
			continue
		}
		languages := r.Languages
		if languages == nil {
			// A runtime unlocks no language of its own. An empty array says that;
			// a null would make a consumer guess whether the field was omitted.
			languages = []string{}
		}
		out.Tools = append(out.Tools, toolEntry{
			Name: r.Name, Version: r.Version, State: string(r.State),
			Languages: languages, Detail: r.Detail,
		})
	}
	return out
}

// corruptFailure turns a report carrying damaged entries into the typed failure
// `tools verify` exits with. The names travel as one bounded detail so the JSON
// envelope still says which entries to reinstall.
func corruptFailure(rows []toolEntry) error {
	var bad []string
	for _, r := range rows {
		if r.State == string(toolchain.StateCorrupt) {
			bad = append(bad, r.Name)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return (&model.Error{Code: model.CodeToolCorrupt,
		Message: fmt.Sprintf("%d installed %s no longer %s the digests the lock pins",
			len(bad), plural(len(bad), "tool", "tools"), plural(len(bad), "matches", "match"))}).
		WithDetail("tools", strings.Join(bad, ",")).
		// A corrupt entry is repaired by a fresh install, not by gc: gc removes
		// versions the lock no longer names, and a damaged tree at the pinned
		// version is one the lock does name.
		WithRemediation("run `codectx tools prefetch " + strings.Join(bad, " ") + "` to reinstall the pinned payloads")
}

// firstToolFailure reduces the joined error Prefetch returns to the one typed
// failure the envelope reports. Every part is a typed CTX_TOOL_* error and the
// common case -- an offline or unreachable host -- makes all of them the same,
// so joining a dozen identical sentences into one message would be noise rather
// than information; the count of failures travels as a bounded detail instead.
func firstToolFailure(err error) error {
	if err == nil {
		return nil
	}
	parts := flattenJoined(err)
	var first *model.Error
	for _, part := range parts {
		var typed *model.Error
		if errors.As(part, &typed) && first == nil {
			first = typed
		}
	}
	if first == nil {
		return err
	}
	if len(parts) > 1 {
		return first.WithDetail("failed_tools", fmt.Sprint(len(parts)))
	}
	return first
}

// flattenJoined expands errors.Join's tree into its leaves.
func flattenJoined(err error) []error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, part := range joined.Unwrap() {
		out = append(out, flattenJoined(part)...)
	}
	return out
}

// emitToolReport renders one toolchain report under the single-envelope rule of
// Section 18.2. A JSON failure replaces the report, because two documents on
// stdout is exactly what that rule forbids; human output still prints the rows,
// since they are what the operator asked for and Execute writes the failure to
// stderr.
func emitToolReport(cmd *cobra.Command, build model.BuildInfo, args []string, data toolReport, failure error) error {
	if jsonRequested(cmd, args) {
		if failure != nil {
			return failure
		}
		return writeEnvelope(cmd.OutOrStdout(), successEnvelope(build.SchemaVersion, cmd.Name(), data))
	}
	if err := writeToolTable(cmd.OutOrStdout(), data); err != nil {
		return err
	}
	return failure
}

// writeToolTable renders the human report. Every column is a bounded value the
// toolchain produced -- a name, a version, a state and a language list -- so
// nothing here needs display sanitization beyond the store path, which is the
// operator's own.
func writeToolTable(w io.Writer, data toolReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "platform  %s\nstore     %s\n\n", data.Platform, data.Store)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATE\tLANGUAGES\tDETAIL")
	counts := map[string]int{}
	for _, t := range data.Tools {
		counts[t.State]++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Version, t.State,
			strings.Join(t.Languages, ", "), t.Detail)
	}
	if err := tw.Flush(); err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "failed to render output: " + err.Error()}
	}
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, fmt.Sprintf("%d %s", counts[state], state))
	}
	sort.Strings(states)
	fmt.Fprintf(&b, "\n%d %s: %s\n", len(data.Tools), plural(len(data.Tools), "entry", "entries"),
		strings.Join(states, ", "))
	return writeText(w, "%s", b.String())
}

// writeText emits human output, failing the command when stdout cannot take it
// rather than reporting success for something nobody received.
func writeText(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return &model.Error{Code: model.CodeInternal, Message: "failed to write output: " + err.Error()}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
