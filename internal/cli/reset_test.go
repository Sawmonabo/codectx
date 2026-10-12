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
)

// resetDataDir is the data directory a workspace of repo resolves to, made
// without composing the workspace. A composition in this process would register
// its scratch pools under that directory, and a reset would then queue its
// removals there instead of freeing them in place as the command's own
// process, which composes nothing, does.
func resetDataDir(t *testing.T, repo string) string {
	t.Helper()
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}
	if err := os.MkdirAll(cfg.Storage.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg.Storage.DataDir
}

// TestResetReportsWhatWentAndWarnsOfTheStall holds the command to what an
// operator agrees to when they run it: the envelope names every entry the data
// directory held and the bytes its removal gave back, and states that freeing a
// large amount of space can stall some virtual hosts, because that is the cost
// of the request.
//
// Mutation: drop resetNotice from the envelope (or name nothing in Removed)
// and the operator is not told what went or what it may cost the host.
func TestResetReportsWhatWentAndWarnsOfTheStall(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	isolateUserDirs(t, "")
	dir := t.TempDir()

	run := func(args ...string) (string, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		root := cli.NewRoot(build, &stdout, &stderr)
		err := cli.Execute(context.Background(), build, root, args)
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty", stderr.String())
		}
		return stdout.String(), err
	}
	if _, err := run("init", dir, "--json"); err != nil {
		t.Fatalf("init: %v", err)
	}
	data := resetDataDir(t, dir)
	const held = 64 << 10
	stale := filepath.Join(data, "left-by-an-older-run")
	if err := os.WriteFile(stale, make([]byte, held), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run("reset", dir, "--json")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	var env struct {
		OK       bool                 `json:"ok"`
		Command  string               `json:"command"`
		Warnings []string             `json:"warnings"`
		Data     model.WorkspaceReset `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("reset did not emit one envelope: %v (%q)", err, out)
	}
	if !env.OK || env.Command != "reset" {
		t.Fatalf("envelope ok=%v command=%q", env.OK, env.Command)
	}
	if !strings.Contains(strings.Join(env.Warnings, " "), "stall") {
		t.Fatalf("reset did not state that freeing large space can stall some hosts: %v", env.Warnings)
	}
	var named *model.RemovedEntry
	for i, e := range env.Data.Removed {
		if filepath.Base(e.Path) == filepath.Base(stale) {
			named = &env.Data.Removed[i]
		}
	}
	if named == nil || named.Bytes != held {
		t.Fatalf("reset reported %+v; want %s named with the %d bytes it gave back", env.Data.Removed, stale, held)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("the entry the reset reported removing is still there: %v", err)
	}
}

// The requirement: a repository named both positionally and with --repo is
// refused before anything is removed. The two could name different
// workspaces, and picking either one empties a data directory the operator
// may not have meant.
//
// Mutation: resolve the target from --repo alone (drop repoTarget) and the
// reset empties the --repo workspace's data directory.
func TestResetNamingTheRepositoryTwiceRemovesNothing(t *testing.T) {
	build := model.BuildInfo{Version: "1.2.3", Commit: "abc1234", Toolchain: "go1.27.1", SchemaVersion: "1"}
	isolateUserDirs(t, "")
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := cli.Execute(context.Background(), build, cli.NewRoot(build, &stdout, &stderr), []string{"init", dir}); err != nil {
		t.Fatalf("init: %v", err)
	}
	data := resetDataDir(t, dir)
	kept := filepath.Join(data, "kept")
	if err := os.WriteFile(kept, []byte("derived"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	err := cli.Execute(context.Background(), build, cli.NewRoot(build, &stdout, &stderr),
		[]string{"reset", t.TempDir(), "--repo", dir, "--json"})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Fatalf("reset naming the repository twice = %v, want %s", err, model.CodeArgumentInvalid)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("a refused reset removed %s: %v", kept, err)
	}
}
