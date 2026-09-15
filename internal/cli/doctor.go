package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/spf13/cobra"
)

const (
	doctorOfflineFlag = "offline"
	doctorDeepFlag    = "deep"
)

// doctorWorkspaceCheck names the one check this command produces itself: the
// workspace open. Every other check in the report is Section 22's list, built
// by internal/diagnostics behind the facade, and the two never run together --
// a workspace that opened is diagnosed by the service, and one that did not is
// diagnosed here, because there is nothing to hand the service.
const doctorWorkspaceCheck = "workspace_open"

// newDoctorCommand builds `codectx doctor`.
//
// It is the one command in the tree that does NOT go through runService, and
// that is deliberate rather than drift. runService turns a failed open into a
// failed command, which for `doctor` is the exact inversion of its purpose: the
// command an operator reaches for when their workspace is broken would refuse
// to run on a broken workspace and tell them only what they already knew. So
// `doctor` opens for itself and keeps the error, and an open that fails becomes
// a FAILING CHECK inside an otherwise ordinary report -- the build identity is
// still reported, the failure carries its typed code and its remediation, and
// the diagnosis completed. The command therefore succeeds: Section 18.2's exit
// 0 is "successful complete operation", and the operation here is the
// diagnosis, not the workspace.
//
// It keeps the Section 18.1 spelling `doctor [path]` -- it is meant to be
// pointed at a directory that may not be a working installation at all -- and
// also declares the shared --repo flag every other workspace command spells
// the same question with. One concept must not have two spellings across
// sibling commands: an operator who reaches for `doctor` after `repo-map` or
// `status` should not have their --repo rejected as an unknown flag. Naming
// the repository both ways is refused by repoTarget rather than resolved.
func newDoctorCommand(build model.BuildInfo) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor [path]",
		Short: "Diagnose this installation and the named workspace",
		Long: "Runs the Section 22 diagnostic checks against the named repository, or the " +
			"current directory when no path is given: build and toolchain pins, workspace " +
			"and data-directory permissions, free space, the SQLite schema, FTS and WAL, " +
			"the active generation pointer, recent capture and freshness, sampled content " +
			"blocks, bundled grammar availability, managed toolchain status per lock entry, " +
			"orphan temporary state and session and lease retention.\n\n" +
			"Each check carries its own state, reason code and remediation, and a metric " +
			"this host cannot measure is reported as unavailable rather than as zero, and a " +
			"check this run deliberately skipped is reported as unverified rather than as " +
			"passed. A " +
			"workspace that cannot be opened is itself reported as a failing check, so this " +
			"command answers on exactly the installations it exists to diagnose.\n\n" +
			"The repository is named as a positional path, or with --repo, and naming it " +
			"both ways is refused rather than resolved to one of them.\n\n" +
			"Without --deep the checks that walk the whole database are not run: the " +
			"database integrity and referential checks and the row-count accounting are " +
			"each reported as a check in state unverified, naming --deep as what verifies " +
			"them. The retained-object sample is bounded either way and reports what it " +
			"sampled; it is unverified only when that sample came back empty, which " +
			"without the row counts cannot be told from an unreadable one. " +
			"The header, page count, schema " +
			"fingerprint, write-ahead-log mode and size are still read and can still fail " +
			"the report. --deep runs those checks in full; no full database scan happens " +
			"without it. --offline asks for the offline-policy " +
			"checks, and what the report says is what those checks found -- the flag itself " +
			"asserts nothing about this installation.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, args, build)
		},
	}
	addRepoFlag(cmd)
	cmd.Flags().Bool(doctorOfflineFlag, false,
		"include the offline-policy checks in the report")
	cmd.Flags().Bool(doctorDeepFlag, false,
		"also run the whole-database checks an ordinary run reports as unverified -- database integrity, referential integrity, the search index and the row-count accounting -- and widen the retained-object sample")
	return cmd
}

// runDoctor resolves the target, produces one report and renders it.
func runDoctor(cmd *cobra.Command, args []string, build model.BuildInfo) error {
	req := model.DoctorRequest{}
	root, err := repoTarget(cmd, args)
	if err != nil {
		return err
	}
	if req.Offline, err = boolFlag(cmd, doctorOfflineFlag); err != nil {
		return err
	}
	if req.Deep, err = boolFlag(cmd, doctorDeepFlag); err != nil {
		return err
	}
	report, err := doctorReport(cmd.Context(), build, root, req)
	if err != nil {
		// Not the open -- that is a check above. This is the diagnostics
		// producer itself failing, which is a defect in this build and is
		// reported as one rather than dressed up as a clean report.
		return err
	}
	out := cmd.OutOrStdout()
	if jsonRequested(cmd, args) {
		return writeEnvelope(out, successEnvelope(build.SchemaVersion, commandName(cmd), report))
	}
	return writeDoctorReport(out, report)
}

// doctorReport opens the workspace for a read-only report and asks the facade
// for the check list. A failed open is not returned: it is turned into the
// one-check report unopenableReport builds, which is the whole reason this
// command owns its own open.
//
// The report is returned as the service produced it. Validating it here would
// be the service's own contract checked a second time in a renderer, and a
// report this command refused to print is a diagnosis the operator never sees.
func doctorReport(ctx context.Context, build model.BuildInfo, root string,
	req model.DoctorRequest) (model.DoctorReport, error) {
	ws, openErr := app.OpenWorkspaceForReport(ctx, root)
	if openErr != nil {
		return unopenableReport(build, req, openErr, time.Now().UTC()), nil
	}
	defer ws.Close()
	return ws.Services().Doctor(ctx, req)
}

// unopenableReport is the report for an installation whose workspace could not
// be opened. It carries the build identity -- which is a fact about this binary
// and is knowable without a workspace -- and exactly one check, failing, with
// the typed code and remediation the open produced.
//
// An open failure that is NOT typed falls back to a generic detail rather than
// the error's own text. Every other detail in a report is generated from a
// typed code (internal/diagnostics/doctor.go), and an untyped error can carry
// a path, a filesystem message or provider output -- the ordinary-log rule of
// Section 21 applied to the one place a report can still leak.
//
// Deep and Offline are echoed from the request rather than asserted: no check
// ran, so nothing here knows more than what was asked for, and claiming an
// offline policy that was never resolved would be the "reports merely that the
// flag was passed" failure Section 21 names.
func unopenableReport(build model.BuildInfo, req model.DoctorRequest, err error,
	now time.Time) model.DoctorReport {
	check := model.DoctorCheck{
		Name:        doctorWorkspaceCheck,
		State:       model.CheckFail,
		Code:        model.CodeInternal,
		Detail:      "the workspace could not be opened, so no further check could run",
		Remediation: "re-run this command naming the repository path, and report it with that command if the failure persists",
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		// The typed message and remediation are the product's own bounded,
		// code-derived text (Section 21: remediation is generated from typed
		// codes), so they are reported as they are rather than re-worded here
		// into a second vocabulary for the same failure.
		check.Code = typed.Code
		check.Detail = clip(typed.Message, model.MaxDetailBytes)
		check.Remediation = clip(typed.Remediation, model.MaxDetailBytes)
	}
	return model.DoctorReport{
		Build:     build,
		Deep:      req.Deep,
		Offline:   req.Offline,
		State:     model.CheckFail,
		Checks:    []model.DoctorCheck{check},
		CheckedAt: now,
	}
}

// writeDoctorReport renders the human report.
//
// Every rendered value comes from the report, never from the command line:
// printing "offline yes" because --offline was typed would say the policy is in
// force when all that is known is that it was asked about, which is the
// distinction Section 21 draws for this exact flag.
//
// Check details pass through tableCell because a check may quote a path or a
// name read out of the repository, whose content no Validate bounds; the
// remediation does not, because it is product-authored from a typed code and a
// clipped instruction is an instruction an operator cannot follow.
func writeDoctorReport(w io.Writer, r model.DoctorReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "build       %s (commit %s, %s, schema %s)\n",
		r.Build.Version, r.Build.Commit, r.Build.Toolchain, r.Build.SchemaVersion)
	fmt.Fprintf(&b, "state       %s\nmode        %s\noffline     %s\nchecked     %s\n\n",
		r.State, deepLabel(r.Deep), offlineLabel(r.Offline), r.CheckedAt.Format(time.RFC3339))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATE\tCODE\tDETAIL")
	counts := map[model.CheckState]int{}
	for _, c := range r.Checks {
		counts[c.State]++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", tableCell(c.Name), c.State, c.Code, tableCell(c.Detail))
	}
	flushTableInto(tw)
	fmt.Fprintf(&b, "\n%d %s: %s\n", len(r.Checks), plural(len(r.Checks), "check", "checks"),
		checkSummary(counts))
	// A remediation the table clipped is a remediation nobody can act on, so
	// every check that is not passing repeats its instruction in full below.
	for _, c := range r.Checks {
		if c.State == model.CheckPass || c.Remediation == "" {
			continue
		}
		fmt.Fprintf(&b, "\n%s (%s)\n  %s\n", c.Name, c.State, c.Remediation)
	}
	return writeText(w, "%s", b.String())
}

// checkSummary counts the states in the order Section 22 spells them, so two
// runs of the same installation render their summary the same way.
func checkSummary(counts map[model.CheckState]int) string {
	parts := make([]string, 0, len(counts))
	for _, state := range []model.CheckState{model.CheckPass, model.CheckWarn,
		model.CheckFail, model.CheckUnavailable, model.CheckUnverified} {
		if n := counts[state]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, state))
		}
	}
	if len(parts) == 0 {
		return "none reported"
	}
	return strings.Join(parts, ", ")
}

// deepLabel names the depth the report was produced at. It is a value, not a
// hint: a field an operator reads as data must not carry an instruction, which
// is why the suggestion to pass --deep lives in the command's help text.
func deepLabel(deep bool) string {
	if deep {
		return "deep"
	}
	return "ordinary"
}

// offlineLabel says whether the offline-policy checks were asked for, and no
// more. It deliberately claims nothing about an operating-system level
// restriction on analyzer execution: Section 21 requires that to be reported,
// but it is a CHECK, produced behind the facade, and a renderer that asserted
// it from a boolean would be reporting the flag as the boundary -- precisely
// the substitution Section 21 forbids. See hand-off 4 in the lane report: no
// lane currently owns that check.
func offlineLabel(offline bool) string {
	if offline {
		return "policy checks requested"
	}
	return "policy checks not requested"
}
