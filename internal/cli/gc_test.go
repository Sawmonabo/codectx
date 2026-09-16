package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/cli"
	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/scratch"
)

// TestGCEmptiesThePoolsAndSaysWhatTheyHeld protects the only way an operator
// can get this product's disk back.
//
// The product never frees its working files on its own: it writes over them
// next time, because creating and freeing them per use is what stalls the host
// this design exists for. So scratch_bytes only ever grows towards the
// workspace's working set, and the resources block tells the operator it
// "shrinks only when an operator asks for the pools to be emptied". Until this
// command existed there was no such request -- the figure was monotonic for the
// life of the data directory and the only recovery was removing it by hand.
//
// The command has to say what it is about to free, by what each surface was
// taken for, before it frees it: "12 GB of scratch" is a number to be alarmed
// by and "11 GB of it is sort runs" is a number to decide on. And it has to
// state that a large collection can stall some virtual hosts, because that is
// the cost the operator is agreeing to.
//
// Mutation: report the pools without emptying them (drop the Empty call) and
// the surface is still on the disk afterwards.
func TestGCEmptiesThePoolsAndSaysWhatTheyHeld(t *testing.T) {
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

	ws, err := app.OpenWorkspaceForReport(context.Background(), dir)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	data := ws.DataDir()
	if err := ws.Close(); err != nil {
		t.Fatalf("close workspace: %v", err)
	}

	// One pooled surface with bytes in it, of a purpose the report must name.
	const held = 256 << 10
	lease, f, err := scratch.For(data).TakeFile(scratch.SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if _, err := f.Write(make([]byte, held)); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := lease.Path()
	lease.Release()

	out, err := run("gc", "--repo", dir, "--json")
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	var env struct {
		OK       bool                    `json:"ok"`
		Command  string                  `json:"command"`
		Warnings []string                `json:"warnings"`
		Data     model.ScratchCollection `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("gc did not emit one envelope: %v (%q)", err, out)
	}
	if !env.OK || env.Command != "gc" {
		t.Fatalf("envelope ok=%v command=%q", env.OK, env.Command)
	}
	if len(env.Warnings) == 0 || !strings.Contains(strings.Join(env.Warnings, " "), "stall") {
		t.Fatalf("gc did not state that freeing large space can stall some hosts: %v", env.Warnings)
	}
	var reported uint64
	for _, p := range env.Data.Pools {
		reported += p.HeldByPurpose[string(scratch.SortRun)]
	}
	if reported < held {
		t.Fatalf("gc reported %d bytes of %s across %d pools, want at least the %d pooled: an operator cannot judge space the report does not name",
			reported, scratch.SortRun, len(env.Data.Pools), held)
	}
	if env.Data.FreedBytes < held {
		t.Fatalf("gc reported %d bytes freed of %d held: the request to give the space back gave back less than it named",
			env.Data.FreedBytes, env.Data.HeldBytes)
	}
	if _, err := scratch.For(data).Bytes(); err != nil {
		t.Fatalf("reading the pool after the collection: %v", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatalf("the pooled surface %s survived the collection: the command reported space it did not give back", path)
	}
}

// The requirement: the collection has to name what it left behind. A pool
// instance a running process owns is skipped -- emptying it would take the
// working files out from under that run -- and the whole of it is skipped,
// which on a machine running an index beside the request is most of the disk
// the report just said was held. Unnamed, "12 GB held, 200 MB freed" reads as
// the command having quietly failed, and an operator with no reason for the
// difference has nothing to act on.
//
// Mutation: drop the LeftAlone entry (leave the instance skipped silently in
// Arena.Empty) and the report shows the shortfall with nothing to explain it.
func TestGCNamesTheInstancesALiveRunOwns(t *testing.T) {
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
	ws, err := app.OpenWorkspaceForReport(context.Background(), dir)
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	data := ws.DataDir()
	if err := ws.Close(); err != nil {
		t.Fatalf("close workspace: %v", err)
	}

	// An instance of the pool that another run owns: its claim is held, and
	// it holds a surface with bytes in it. A whole-file lock conflicts
	// between descriptors, so a second open here is another owner as far as
	// the claim is concerned.
	const held = 128 << 10
	owned := filepath.Join(scratch.Dir(data), "9")
	if err := os.MkdirAll(filepath.Join(owned, string(scratch.SortRun)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, string(scratch.SortRun), "0"), make([]byte, held), 0o600); err != nil {
		t.Fatal(err)
	}
	claim, err := os.OpenFile(filepath.Join(owned, "owner.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	if taken, err := fslock.TryLock(claim); err != nil || !taken {
		t.Fatalf("the claim on the instance a live run owns could not be held: %v", err)
	}
	defer fslock.Unlock(claim)

	out, err := run("gc", "--repo", dir, "--json")
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	var env struct {
		Data model.ScratchCollection `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("gc did not emit one envelope: %v (%q)", err, out)
	}
	var named []model.UntouchedInstance
	for _, p := range env.Data.Pools {
		named = append(named, p.LeftAlone...)
	}
	if len(named) != 1 || named[0].Instance != "9" {
		t.Fatalf("gc reported %d bytes held and %d freed and named %v as left alone; "+
			"the instance a live run owns is the whole of the difference and the operator is told nothing about it",
			env.Data.HeldBytes, env.Data.FreedBytes, named)
	}
	if named[0].HeldBytes < held {
		t.Fatalf("the instance left alone is reported holding %d bytes of %d", named[0].HeldBytes, held)
	}
	if named[0].Reason == "" {
		t.Fatal("the instance left alone carries no reason, so an operator cannot tell a skipped pool from a failed collection")
	}
	if _, err := os.Stat(filepath.Join(owned, string(scratch.SortRun), "0")); err != nil {
		t.Fatalf("the surface of a live run's instance did not survive the collection: %v", err)
	}
}
