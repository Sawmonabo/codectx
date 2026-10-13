package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestAnAmbiguousNameCountsAndNamesEveryCandidate indexes forty packages that
// each declare a function of the same name and resolves that name. The
// rejection must carry the exact number of candidates, forty, and name every
// one of them: the operator picks from that list, and a list cut at a
// presentation bound with a fetched-bound-plus-one total misstates both how
// many there are and which ones exist.
//
// Mutation that fails it: fetch a fixed number of candidates and one more,
// as a bounded lookup would (the count reads as that bound plus one and the
// candidates past it are named nowhere).
func TestAnAmbiguousNameCountsAndNamesEveryCandidate(t *testing.T) {
	const packages = 40
	ctx := context.Background()
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	userCfg := filepath.Join(home, "config", "codectx")
	if err := os.MkdirAll(userCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[storage]\ndata_dir = \"" + filepath.Join(home, "data") + "\"\n" +
		"[tools]\noffline = true\n" +
		"[providers.scip]\nenabled = false\n[providers.lsp]\nenabled = false\n"
	writeFile(t, filepath.Join(userCfg, "config.toml"), []byte(cfg))
	writeFile(t, filepath.Join(repo, "go.mod"), []byte("module example.com/dup\n\ngo 1.27\n"))
	for i := range packages {
		pkg := "p" + strconv.Itoa(i)
		if err := os.MkdirAll(filepath.Join(repo, pkg), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(repo, pkg, "dup.go"), []byte("package "+pkg+"\n\nfunc Duplicated() {}\n"))
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))

	ws, err := OpenWorkspace(ctx, repo, OpenOptions{Operation: "index"})
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	result, err := ws.Services().Index(ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	gen := result.Binding.GenerationID

	// Every node the name selects, read here by the test's own keyset walk.
	reader, err := ws.s.store.PinGeneration(ctx, ws.s.repo, gen, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var want []model.NodeID
	after := model.NodeID("")
	for {
		page, err := reader.Nodes(ctx, sqlite.NodeFilter{Name: "Duplicated"}, after, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, n := range page {
			want = append(want, n.Node.ID)
		}
		after = page[len(page)-1].Node.ID
	}
	reader.Close()
	if len(want) != packages {
		t.Fatalf("the generation holds %d nodes named Duplicated, want one per package (%d)", len(want), packages)
	}

	_, err = ws.ResolveNodes(ctx, gen, []string{"Duplicated"})
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Fatalf("resolving an ambiguous name = %v, want CTX_ARGUMENT_INVALID", err)
	}
	if got := typed.Details["candidates"]; got != strconv.Itoa(packages) {
		t.Fatalf("the rejection counts %s candidates, want exactly %d", got, packages)
	}
	for _, id := range want {
		if !strings.Contains(typed.Remediation, string(id)[:12]) {
			t.Fatalf("candidate %s is named nowhere in the rejection: %s", id, typed.Remediation)
		}
	}
}
