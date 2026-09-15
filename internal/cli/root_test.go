package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/cli"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// TestCommandEnvelope protects the machine-stable CLI boundary of Section 18.2:
// a --json request emits exactly one envelope on stdout, logs and human text
// never share that channel, and an argument rejection is reported through the
// same envelope with exit code 2.
//
// The two `tools` rows protect the managed-toolchain boundary of Section 11.7
// and its exit-code mapping in Task 22 Step 3. `tools status` on an empty store
// must report every lock entry as honestly absent and succeed: a report that
// silently dropped entries, or failed because nothing is installed, would make
// an operator believe a language is unsupported when it merely is not fetched
// yet. `tools prefetch` under tools.offline must fail with CTX_TOOL_OFFLINE and
// exit 5, because that family reaching the default branch of ExitCode would
// report a refusal the user configured as an internal defect (exit 10).
//
// The `status` row protects the same boundary for the commands the index
// coordinator answers. Those commands open a workspace, a database, a tool
// store and several analyzer processes, and every one of those failures has to
// reach the operator as one typed envelope in its own exit class. An untyped
// error from any step in that composition falls into ExitCode's default
// branches -- exit 2 ("you typed it wrong") or exit 10 ("this is a defect") --
// and a machine consumer would then retry a workspace that does not exist, or
// file a bug against a path the user mistyped.
//
// Every row also pins the envelope's `command` to the command path without the
// binary's own name, which is what distinguishes `codectx tools status` from
// `codectx status`: the two carry different data shapes under one label
// otherwise.
// hexID is a well-formed resolved id. The context commands screen their
// positional arguments before anything else, so a malformed one would be
// rejected for the wrong reason and prove nothing about the flags.
const hexID = "00000000000000000000000000000000000000000000000000000000000000ab"

func TestCommandEnvelope(t *testing.T) {
	build := model.BuildInfo{
		Version:       "1.2.3",
		Commit:        "abc1234",
		Toolchain:     "go1.27.1",
		SchemaVersion: "1",
	}

	tests := []struct {
		name       string
		args       []string
		userConfig string
		// missingRepo points the command at a path that is not a workspace,
		// which is how a row reaches the composition's own failure classes
		// without depending on the directory the test happens to run in.
		missingRepo bool
		// absentWorkspaceArg does the same for the two Section 18.1 commands
		// that name their repository POSITIONALLY -- `doctor [path]` and
		// `repo-map [path]`.
		absentWorkspaceArg bool
		exitCode           int
		ok                 bool
		command            string
		errCode            string
		checkData          func(t *testing.T, data json.RawMessage)
	}{
		{name: "version json", args: []string{"version", "--json"}, exitCode: 0, ok: true, command: "version", checkData: checkBuildData(build)},
		{name: "unknown flag", args: []string{"version", "--bogus", "--json"}, exitCode: 2, ok: false, command: "version", errCode: "CTX_ARGUMENT_INVALID"},
		{name: "unknown command", args: []string{"bogus", "--json"}, exitCode: 2, ok: false, command: "codectx", errCode: "CTX_ARGUMENT_INVALID"},
		{name: "tools status on an empty store", args: []string{"tools", "status", "--json"}, exitCode: 0, ok: true, command: "tools status", checkData: checkEmptyStoreReport},
		{name: "tools prefetch while offline", args: []string{"tools", "prefetch", "--all", "--json"}, userConfig: "[tools]\noffline = true\n", exitCode: 5, ok: false, command: "tools prefetch", errCode: "CTX_TOOL_OFFLINE"},
		{name: "status on a path that is not a workspace", args: []string{"status", "--json"}, missingRepo: true, exitCode: 3, ok: false, command: "status", errCode: "CTX_WORKSPACE_NOT_FOUND"},
		// `context acknowledge` carries two assertions that Section 16.3 keeps
		// apart: --receipt confirms that bytes were delivered, --file-review
		// asserts that a fully served file was reviewed. A command line naming
		// both has to be resolved by guessing which one the operator meant, and
		// either guess writes a claim nobody made -- credit for bytes that may
		// never have arrived, or a review of a file that may not be covered. The
		// rejection also has to happen before a workspace is opened, which is
		// what this row pins: it points at a path that is not a workspace, so a
		// check that ran after the open would fail on the workspace instead of
		// on the flags, and the service's own rejection of the same request
		// cannot stand in for the command's.
		{name: "context acknowledge naming both acknowledgment kinds", args: []string{"context", "acknowledge", hexID, hexID, "--actor", "agent-a", "--receipt", "tok", "--file-review", "--json"}, missingRepo: true, exitCode: 2, ok: false, command: "context acknowledge", errCode: "CTX_ARGUMENT_INVALID"},
		// Section 11.6 forbids answering a semantic_source=lsp request from
		// canonical facts, and Section 11.5 keeps the choice of language server
		// out of the product's hands: a root marker is a hint, not a choice. So
		// `--semantic-source lsp` with no `--profile` names no server at all,
		// and the only two things the command could do instead of refusing are
		// both defects -- guess a server, or fall through to canonical and
		// return a page the caller would read as "the overlay found nothing".
		//
		// The refusal has to land before the workspace is opened, which is what
		// the missing repository pins: a check that ran after the open would
		// fail on the workspace and prove nothing about the selector. It is the
		// exit-2 argument class rather than the provider class for the reason
		// prefetchNames gives (tools.go): a name the caller did not supply is
		// their input, not a provider that could not run.
		//
		// LANE NOTE (L10): the sibling assertion -- a RESOLVABLE-looking
		// profile whose payload cannot be resolved surfacing the toolchain's
		// CTX_TOOL_* verbatim -- is not reachable from this package in this
		// build. Every CTX_TOOL_* originates in toolchain.Resolver.Resolve
		// inside lsp.Resolve, which is reached only through the facade's
		// overlay route (L7/L8), and building a second resolution here would be
		// the duplicate path the facade exists to prevent. Proving it needs a
		// real workspace and belongs to the integration/verification lane:
		// `codectx symbol NAME --semantic-source lsp --profile gopls` under a
		// user config carrying `[tools]\noffline = true` must exit 5 with
		// CTX_TOOL_OFFLINE. What this row does pin is that the command never
		// reaches an empty page on the lsp route.
		{name: "symbol on the lsp overlay without a profile", args: []string{"symbol", "writeSymbolTable", "--semantic-source", "lsp", "--json"}, missingRepo: true, exitCode: 2, ok: false, command: "symbol", errCode: "CTX_ARGUMENT_INVALID"},
		// `context advance` is the guarded transition of Section 17.1, and the
		// guard is the version the caller presents. An omitted --expected-version
		// arrives as zero, which is not "the configured default" the budget flags
		// of this package use but a version no session can ever have: accepting
		// it would turn a compare-and-swap into an unconditional write, which is
		// exactly the lost update the guard exists to prevent. The rejection also
		// has to happen before a workspace is opened -- this row points at a path
		// that is not a workspace, so a check that ran after the open would fail
		// on the workspace (exit 3) instead of on the flag.
		{name: "context advance without the version guard", args: []string{"context", "advance", hexID, "consolidate", "--actor", "agent-a", "--json"}, missingRepo: true, exitCode: 2, ok: false, command: "context advance", errCode: "CTX_ARGUMENT_INVALID"},
		// T20-L6 rows: `doctor` and `repo-map` are the two Section 18.1
		// spellings Task 20 adds, and the pair exists to pin the asymmetry
		// between them, which is a design decision and not an accident of two
		// commands being written by one lane.
		//
		// `doctor` is the command an operator reaches for when their workspace
		// is broken. If a failed open failed the command, the one tool meant to
		// diagnose a broken installation would refuse to run on one and report
		// only what the operator already knew, so the open failure is a FAILING
		// CHECK inside a completed report: the build identity is still there,
		// the typed code and remediation are still there, and the diagnosis
		// succeeded. `repo-map` must do the opposite with the same failure --
		// an unopenable workspace has no map, and answering with an empty page
		// would render it as a repository containing no packages at all, which
		// a model consuming the map reads as fact.
		//
		// LANE NOTE (T20-L6): the lane plan's second assertion -- `repo-map
		// --cursor` on a TAMPERED token reporting CTX_CURSOR_INVALID and exit 8
		// -- is not reachable from this package at fixture scale. The cursor is
		// verified in graph/cursor.go's verifyContinuation, behind a pinned
		// generation, so the open of a directory that is not a workspace fails
		// first and exit 3 is what the row would actually observe; reaching the
		// verifier needs a real store with an active generation, which is the
		// verification lane's argv, not this table's. Handed to VERIFY: `codectx
		// repo-map --cursor <token with one byte changed> --json` against the
		// proof store must be CTX_CURSOR_INVALID with exit 8. The exit-8 mapping
		// itself is already in ExitCode's table and is not re-asserted here.
		{name: "doctor on a workspace it cannot open", args: []string{"doctor", "--json"}, absentWorkspaceArg: true, exitCode: 0, ok: true, command: "doctor", checkData: checkUnopenableWorkspaceReport},
		{name: "repo-map on a workspace it cannot open", args: []string{"repo-map", "--json"}, absentWorkspaceArg: true, exitCode: 3, ok: false, command: "repo-map", errCode: "CTX_WORKSPACE_NOT_FOUND"},
		// L4 rows: the two cases digest Section 6 budgets for this lane are
		// TestInitRefusesToOverwriteProjectConfig and
		// TestBrokenStdoutPipeIsNotADefect, at the end of this file. Neither is
		// expressible in this table: it writes to a bytes.Buffer and then asserts
		// that stdout parses as exactly one envelope, which a broken pipe makes
		// impossible by construction, and it has no way to name a per-case
		// temporary directory as a positional argument. Contorting the harness
		// for two cases would cost every other row its clarity.
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateUserDirs(t, tc.userConfig)
			args := tc.args
			if tc.missingRepo {
				args = append([]string{"--repo", filepath.Join(t.TempDir(), "absent")}, args...)
			}
			var stdout, stderr bytes.Buffer
			root := cli.NewRoot(build, &stdout, &stderr)
			if tc.absentWorkspaceArg {
				args = append(args, filepath.Join(t.TempDir(), "absent"))
			}
			err := cli.Execute(context.Background(), build, root, args)

			if got := cli.ExitCode(err); got != tc.exitCode {
				t.Fatalf("exit code = %d, want %d (err: %v)", got, tc.exitCode, err)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}

			var env struct {
				SchemaVersion string          `json:"schema_version"`
				Command       string          `json:"command"`
				OK            bool            `json:"ok"`
				Data          json.RawMessage `json:"data"`
				Warnings      []string        `json:"warnings"`
				Error         *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
			if err := dec.Decode(&env); err != nil {
				t.Fatalf("stdout is not one JSON envelope: %v (stdout: %q)", err, stdout.String())
			}
			if dec.More() {
				t.Errorf("stdout carries more than one envelope: %q", stdout.String())
			}

			if env.SchemaVersion != build.SchemaVersion {
				t.Errorf("schema_version = %q, want %q", env.SchemaVersion, build.SchemaVersion)
			}
			if env.Command != tc.command {
				t.Errorf("command = %q, want %q", env.Command, tc.command)
			}
			if env.OK != tc.ok {
				t.Errorf("ok = %v, want %v", env.OK, tc.ok)
			}
			if env.Warnings == nil {
				t.Errorf("warnings = null, want an array")
			}

			if tc.ok {
				if env.Error != nil {
					t.Errorf("error = %+v, want null", env.Error)
				}
				tc.checkData(t, env.Data)
				return
			}

			if env.Error == nil {
				t.Fatalf("error = null, want a typed error")
			}
			if env.Error.Code != tc.errCode {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.errCode)
			}
			if env.Error.Message == "" {
				t.Errorf("error.message is empty")
			}
		})
	}
}

// isolateUserDirs points os.UserConfigDir and os.UserCacheDir at this test's own
// directory on every supported platform, so a CLI run neither reads the
// developer's configuration nor writes into their real tool store, and writes
// the user configuration file when the case needs one. config.Load resolves
// both directories through the standard library, so the environment is the seam
// rather than a production variable that exists only for tests.
func isolateUserDirs(t *testing.T, userConfig string) {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, "config")
	cacheDir := filepath.Join(home, "cache")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("AppData", configDir)
	t.Setenv("LocalAppData", cacheDir)
	if userConfig == "" {
		return
	}
	appDir := filepath.Join(configDir, "codectx")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatalf("user configuration directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "config.toml"), []byte(userConfig), 0o600); err != nil {
		t.Fatalf("user configuration file: %v", err)
	}
}

// checkUnopenableWorkspaceReport holds `doctor` to the contract that makes it
// worth registering at all: the workspace that could not be opened is reported
// as a failing check, with the typed code and a detail that says what happened,
// inside a report whose own state is fail. A report that came back passing, or
// with no check at all, would tell an operator their broken installation is
// healthy -- which is worse than the command not existing.
func checkUnopenableWorkspaceReport(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var data struct {
		State  string `json:"state"`
		Checks []struct {
			Name   string `json:"name"`
			State  string `json:"state"`
			Detail string `json:"detail"`
			Code   string `json:"code"`
		} `json:"checks"`
		Build struct {
			Version string `json:"version"`
		} `json:"build"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if model.CheckState(data.State) != model.CheckFail {
		t.Errorf("report state = %q, want %q", data.State, model.CheckFail)
	}
	// The build identity is knowable without a workspace, and it is the first
	// thing anyone reading a diagnostic report needs.
	if data.Build.Version == "" {
		t.Errorf("the report carries no build identity")
	}
	if len(data.Checks) == 0 {
		t.Fatalf("the report carries no check; the open failure has to be reported as one")
	}
	for _, c := range data.Checks {
		if model.CheckState(c.State) != model.CheckFail {
			continue
		}
		if c.Code != string(model.CodeWorkspaceNotFound) {
			t.Errorf("failing check %q code = %q, want %q", c.Name, c.Code, model.CodeWorkspaceNotFound)
		}
		if c.Detail == "" {
			t.Errorf("failing check %q carries no detail", c.Name)
		}
		return
	}
	t.Errorf("no check failed although the workspace could not be opened: %+v", data.Checks)
}

func checkBuildData(build model.BuildInfo) func(*testing.T, json.RawMessage) {
	return func(t *testing.T, raw json.RawMessage) {
		t.Helper()
		var data struct {
			Version       string `json:"version"`
			Commit        string `json:"commit"`
			Toolchain     string `json:"toolchain"`
			SchemaVersion string `json:"schema_version"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("data: %v", err)
		}
		if data.Version != build.Version || data.Commit != build.Commit ||
			data.Toolchain != build.Toolchain || data.SchemaVersion != build.SchemaVersion {
			t.Errorf("data = %+v, want %+v", data, build)
		}
	}
}

// checkEmptyStoreReport holds `tools status` to the Section 11.7 contract on a
// store that holds nothing: every entry the embedded lock names appears, with a
// version, and in a state that says it is not installed rather than broken.
func checkEmptyStoreReport(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var data struct {
		Platform string `json:"platform"`
		Store    string `json:"store"`
		Tools    []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			State   string `json:"state"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data: %v", err)
	}
	if data.Platform != toolchain.Current().Key() {
		t.Errorf("platform = %q, want %q", data.Platform, toolchain.Current().Key())
	}
	if data.Store == "" {
		t.Errorf("store is empty")
	}
	// A read-only report must not materialize the store it reports on: the data
	// directory is per workspace, so a `tools status` that creates one leaves an
	// empty directory behind in every checkout it is ever run in, and a store
	// that exists because a report looked at it is a false "this machine is
	// provisioned" signal for the next reader.
	if _, err := os.Stat(data.Store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("store %q exists after `tools status`; a read-only report must create nothing (stat err: %v)",
			data.Store, err)
	}
	want := toolchain.Embedded().Names()
	if len(want) == 0 {
		t.Fatalf("the embedded lock carries no tools")
	}
	if len(data.Tools) != len(want) {
		t.Fatalf("reported %d entries, want %d (%v)", len(data.Tools), len(want), want)
	}
	for i, got := range data.Tools {
		if got.Name != want[i] {
			t.Errorf("tools[%d].name = %q, want %q", i, got.Name, want[i])
		}
		if got.Version == "" {
			t.Errorf("tools[%d] (%s) has no version", i, got.Name)
		}
		switch toolchain.State(got.State) {
		case toolchain.StateAvailable, toolchain.StateUnsupportedPlatform:
		default:
			t.Errorf("tools[%d] (%s) state = %q, want available or unsupported_platform on an empty store",
				i, got.Name, got.State)
		}
	}
}

// TestExitCodeClasses covers the codes no command can be made to produce from
// the command line, and which would otherwise reach the exit-10 defect class by
// default. It uses ExitCode directly because Execute can only surface what a
// command returns, and no command cancels itself.
func TestExitCodeClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{
			// The runner joins the typed error with the context error so both
			// errors.Is and errors.As keep working; the exit class must survive
			// that wrapping.
			name: "cancellation as the runner reports it",
			err:  errors.Join(&model.Error{Code: model.CodeCanceled, Message: "canceled"}, context.Canceled),
			want: 7,
		},
		{
			name: "cancellation on its own",
			err:  &model.Error{Code: model.CodeCanceled, Message: "canceled"},
			want: 7,
		},
		{
			name: "an unrecognized code is a defect, not a guess",
			err:  &model.Error{Code: "CTX_NOT_A_REAL_CODE", Message: "unknown"},
			want: 10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cli.ExitCode(tc.err); got != tc.want {
				t.Fatalf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestInitRefusesToOverwriteProjectConfig protects the only ordinary command
// that writes project configuration. The failure mode is destruction: `init`
// re-run in a repository that already carries a tuned .codectx.toml must not
// replace it, because nothing in this build can recover the settings and the
// file is the repository's own declaration of what may be indexed. The run also
// has to leave a file config.Load accepts -- a generated file carrying a key
// the loader rejects would fail every later command in that checkout, which is
// a broken installation reported as a healthy one.
func TestInitRefusesToOverwriteProjectConfig(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	isolateUserDirs(t, "")
	dir := t.TempDir()
	path := filepath.Join(dir, config.ProjectConfigName)

	run := func(args ...string) error {
		var stdout, stderr bytes.Buffer
		root := cli.NewRoot(build, &stdout, &stderr)
		err := cli.Execute(context.Background(), build, root, args)
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
		return err
	}

	if err := run("init", dir, "--json"); err != nil {
		t.Fatalf("init: %v", err)
	}
	generated, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("generated file: %v", err)
	}
	if _, err := config.Load(dir); err != nil {
		t.Fatalf("the generated %s is not loadable: %v", config.ProjectConfigName, err)
	}

	tuned := string(generated) + "\n[workspace]\nmax_files = 17\n"
	if err := os.WriteFile(path, []byte(tuned), 0o644); err != nil {
		t.Fatalf("tuned file: %v", err)
	}
	err = run("init", dir, "--json")
	if got := cli.ExitCode(err); got != 2 {
		t.Fatalf("exit code = %d, want 2 (err: %v)", got, err)
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Fatalf("error = %v, want CTX_ARGUMENT_INVALID", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file after the refusal: %v", err)
	}
	if string(after) != tuned {
		t.Fatalf("the refused run changed the file:\n%s", string(after))
	}

	if err := run("init", dir, "--force", "--json"); err != nil {
		t.Fatalf("init --force: %v", err)
	}
	forced, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file after --force: %v", err)
	}
	if string(forced) != string(generated) {
		t.Fatalf("--force did not replace the file")
	}
}

// TestBrokenStdoutPipeIsNotADefect pins the Section 18.2 rule that a broken
// stdout pipe fails the command in a non-defect exit class, which is exactly
// what it asserts and no more. Two failure modes meet here. Swallowing the
// write error would return success for a response nobody received, and the
// caller's next step -- confirming a source receipt -- would credit bytes that
// were never delivered; so the command must fail. Reporting it as CTX_INTERNAL
// would put `codectx ... | head`, an ordinary shell pipeline, into the exit-10
// defect class and send an operator to file a bug against their own pipe.
//
// What this row does NOT cover: the receipt half (`version --json` issues no
// receipt) and the signal half. The write end here is not fd 1, so the runtime
// returns EPIPE without ever raising SIGPIPE and main.go's signal.Ignore is not
// exercised. That 141 -> 7 path is proven against the real binary instead --
// wave-f-verify-T18.md (b) drove `status --json | head -c 0` and observed exit
// 7, not 141.
func TestBrokenStdoutPipeIsNotADefect(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	isolateUserDirs(t, "")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer writer.Close()
	// The reader goes away before anything is written, which is what `| head -c 0`
	// does. The write end is a real file descriptor, so the write returns EPIPE
	// rather than the in-process io.ErrClosedPipe.
	if err := reader.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}

	var stderr bytes.Buffer
	root := cli.NewRoot(build, writer, &stderr)
	err = cli.Execute(context.Background(), build, root, []string{"version", "--json"})
	if err == nil {
		t.Fatal("the command succeeded although its response was never delivered")
	}
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want a typed error", err)
	}
	if typed.Code == model.CodeInternal {
		t.Errorf("error.code = %s; a closed reader is not a defect in this build", typed.Code)
	}
	if got := cli.ExitCode(err); got != 7 {
		t.Fatalf("exit code = %d, want 7 (err: %v)", got, err)
	}
}

// TestMCPServeRefusalStaysOffTheProtocolStream pins the one command whose
// stdout is not its own. `codectx mcp serve` hands stdout to the MCP framing
// for the life of the process, so a --json refusal must not put an envelope
// there: a client reading the protocol stream would decode the first bytes of
// the session as a malformed message. The refusal Cobra produces while PARSING
// the flags is the case the RunE redirect could never cover -- it is returned
// before any Run hook -- and it is the case an operator hits by typo, which is
// exactly when a corrupted stream is hardest to recognize.
//
// It is not a row in TestCommandEnvelope because that harness asserts stdout
// carries one envelope and stderr is empty, which is the inverse of the
// contract here.
func TestMCPServeRefusalStaysOffTheProtocolStream(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "a flag Cobra cannot parse", args: []string{"mcp", "serve", "--bogus", "--json"}},
		{name: "a positional argument the command takes none of", args: []string{"mcp", "serve", "extra", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateUserDirs(t, "")
			var stdout, stderr bytes.Buffer
			root := cli.NewRoot(build, &stdout, &stderr)
			err := cli.Execute(context.Background(), build, root, tc.args)
			if got := cli.ExitCode(err); got != 2 {
				t.Fatalf("exit code = %d, want 2 (err: %v)", got, err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("the refusal wrote %q to the protocol stream", stdout.String())
			}
			var env struct {
				Command string `json:"command"`
				OK      bool   `json:"ok"`
				Error   *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &env); err != nil {
				t.Fatalf("stderr does not carry one envelope: %v (%q)", err, stderr.String())
			}
			if env.OK || env.Command != "mcp serve" || env.Error == nil ||
				env.Error.Code != "CTX_ARGUMENT_INVALID" {
				t.Fatalf("envelope = %q, want a failed mcp serve envelope carrying CTX_ARGUMENT_INVALID",
					stderr.String())
			}
		})
	}
}

// TestVerifyOnAnEmptyStoreSaysNothingIsInstalled protects the one sentence an
// operator reads after running the command that is supposed to tell them
// whether this machine is provisioned. `tools verify` verifies what is
// installed, so an empty store is honestly `ok` with exit 0 -- and its state
// counts alone then read as a clean bill of health for a store that holds
// nothing at all. The human report has to say so and name the command that
// fixes it; false readiness here strands a run mid-index instead.
func TestVerifyOnAnEmptyStoreSaysNothingIsInstalled(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	isolateUserDirs(t, "")
	var stdout, stderr bytes.Buffer
	root := cli.NewRoot(build, &stdout, &stderr)
	if err := cli.Execute(context.Background(), build, root, []string{"tools", "verify"}); err != nil {
		t.Fatalf("tools verify on an empty store = %v, want the report and no failure", err)
	}
	want := "installed -- run `codectx tools prefetch`"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("`tools verify` on an empty store did not say nothing is installed\n  want substring: %s\n  got: %s",
			want, stdout.String())
	}
	if !strings.HasPrefix(stdout.String(), "platform") {
		t.Errorf("the report is missing: %q", stdout.String())
	}
}
