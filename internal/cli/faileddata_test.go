package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAFailureAfterDataKeepsTheDataInItsOneEnvelope runs a command that
// produced a report and then failed -- a reset whose removal happened and
// whose index did not -- with --json. The output must be exactly one envelope,
// not ok, carrying both the failure and the report, and the process must still
// see the typed failure.
//
// Mutation that fails it: Execute writes the bare failure envelope for a
// failedWithData (the removal report is lost), or the command writes its own
// envelope beside Execute's (two envelopes on stdout).
func TestAFailureAfterDataKeepsTheDataInItsOneEnvelope(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	var stdout, stderr bytes.Buffer
	root := NewRoot(build, &stdout, &stderr)
	report := model.WorkspaceReset{DataDir: "/data", Removed: []model.RemovedEntry{{Path: "/data/codectx.db", Bytes: 7}}, FreedBytes: 7}
	failure := &model.Error{Code: model.CodeWorkspaceBusy, Message: "the index after the reset could not start"}
	cmd := &cobra.Command{Use: "probe-failed-data", SilenceErrors: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error { return &failedWithData{err: failure, data: report} }}
	root.AddCommand(cmd)

	err := Execute(context.Background(), build, root, []string{"probe-failed-data", "--json"})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy {
		t.Fatalf("Execute = %v, want the typed failure", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout holds %d envelopes, want one:\n%s", len(lines), stdout.String())
	}
	var env struct {
		OK    bool                 `json:"ok"`
		Data  model.WorkspaceReset `json:"data"`
		Error *model.Error         `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.OK || env.Error == nil || env.Error.Code != model.CodeWorkspaceBusy {
		t.Fatalf("envelope ok=%v error=%+v, want a failure naming the index's error", env.OK, env.Error)
	}
	if len(env.Data.Removed) != 1 || env.Data.Removed[0].Path != "/data/codectx.db" || env.Data.FreedBytes != 7 {
		t.Fatalf("envelope data = %+v, want the removal report", env.Data)
	}
}
