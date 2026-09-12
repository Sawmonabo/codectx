package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Sawmonabo/codectx/internal/cli"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestVersionEnvelope protects the machine-stable CLI boundary of Section 18.2:
// a --json request emits exactly one envelope on stdout, logs and human text
// never share that channel, and an argument rejection is reported through the
// same envelope with exit code 2.
func TestVersionEnvelope(t *testing.T) {
	build := model.BuildInfo{
		Version:       "1.2.3",
		Commit:        "abc1234",
		Toolchain:     "go1.27.1",
		SchemaVersion: "1",
	}

	tests := []struct {
		name     string
		args     []string
		exitCode int
		ok       bool
		command  string
		errCode  string
	}{
		{name: "version json", args: []string{"version", "--json"}, exitCode: 0, ok: true, command: "version"},
		{name: "unknown flag", args: []string{"version", "--bogus", "--json"}, exitCode: 2, ok: false, command: "version", errCode: "CTX_ARGUMENT_INVALID"},
		{name: "unknown command", args: []string{"bogus", "--json"}, exitCode: 2, ok: false, command: "codectx", errCode: "CTX_ARGUMENT_INVALID"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
				var data struct {
					Version       string `json:"version"`
					Commit        string `json:"commit"`
					Toolchain     string `json:"toolchain"`
					SchemaVersion string `json:"schema_version"`
				}
				if err := json.Unmarshal(env.Data, &data); err != nil {
					t.Fatalf("data: %v", err)
				}
				if data.Version != build.Version || data.Commit != build.Commit ||
					data.Toolchain != build.Toolchain || data.SchemaVersion != build.SchemaVersion {
					t.Errorf("data = %+v, want %+v", data, build)
				}
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
