package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/cli"
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
// rejected for the wrong reason and prove nothing about the flags. INT:
// uncomment with the row that uses it.
// const hexID = "00000000000000000000000000000000000000000000000000000000000000ab"

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
		exitCode    int
		ok          bool
		command     string
		errCode     string
		checkData   func(t *testing.T, data json.RawMessage)
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
		// what this row pins: the row names no repository, so a check that ran
		// after the open would fail on the workspace instead of on the flags.
		// INT: uncomment this row together with the root.go registration line.
		// The row is green with that line in place and mutation-proved in
		// T16-L5-report.md; without it the tree routes `context` nowhere, so the
		// envelope reports the root command instead of the rejection.
		// {name: "context acknowledge naming both acknowledgment kinds", args: []string{"context", "acknowledge", hexID, hexID, "--actor", "agent-a", "--receipt", "tok", "--file-review", "--json"}, exitCode: 2, ok: false, command: "context acknowledge", errCode: "CTX_ARGUMENT_INVALID"},
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
