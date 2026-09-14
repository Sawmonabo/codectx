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
		exitCode   int
		ok         bool
		command    string
		errCode    string
		checkData  func(t *testing.T, data json.RawMessage)
	}{
		{name: "version json", args: []string{"version", "--json"}, exitCode: 0, ok: true, command: "version", checkData: checkBuildData(build)},
		{name: "unknown flag", args: []string{"version", "--bogus", "--json"}, exitCode: 2, ok: false, command: "version", errCode: "CTX_ARGUMENT_INVALID"},
		{name: "unknown command", args: []string{"bogus", "--json"}, exitCode: 2, ok: false, command: "codectx", errCode: "CTX_ARGUMENT_INVALID"},
		{name: "tools status on an empty store", args: []string{"tools", "status", "--json"}, exitCode: 0, ok: true, command: "status", checkData: checkEmptyStoreReport},
		{name: "tools prefetch while offline", args: []string{"tools", "prefetch", "--all", "--json"}, userConfig: "[tools]\noffline = true\n", exitCode: 5, ok: false, command: "prefetch", errCode: "CTX_TOOL_OFFLINE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateUserDirs(t, tc.userConfig)
			var stdout, stderr bytes.Buffer
			root := cli.NewRoot(build, &stdout, &stderr)
			err := cli.Execute(context.Background(), build, root, tc.args)

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
