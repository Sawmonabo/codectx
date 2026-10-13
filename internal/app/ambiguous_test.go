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

// TestAnAmbiguousNameCountsAndNamesEveryCandidate indexes more packages than
// one page holds, each declaring a function of the same name, and resolves
// that name. The rejection must carry the exact number of candidates and,
// first, the command that lists every one of them, so the remediation's byte
// bound can never cut either; it must state how many candidates it names, name
// exactly those -- the first by node id -- and no other.
//
// Mutations that fail it: count by fetching a page bound and one more, as a
// bounded lookup would (the count reads 201, not 205); place the `codectx
// symbol` pointer after the names, where the bound cuts it; let the bound cut
// the list mid-name, or claim more or fewer names than the text holds.
func TestAnAmbiguousNameCountsAndNamesEveryCandidate(t *testing.T) {
	const packages = model.MaxPageItems + 5
	ctx := context.Background()
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	userCfg := filepath.Join(home, "config", "codectx")
	for _, dir := range []string{userCfg, repo} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
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
	lead := strconv.Itoa(packages) + " nodes carry this name; `codectx symbol \"Duplicated\"` lists every match"
	if !strings.HasPrefix(typed.Remediation, lead) {
		t.Fatalf("the rejection does not open with the count and the listing command %q: %s", lead, typed.Remediation)
	}
	if len(typed.Remediation) > model.MaxDetailBytes {
		t.Fatalf("the remediation is %d bytes, past the %d-byte bound", len(typed.Remediation), model.MaxDetailBytes)
	}
	named, err := strconv.Atoi(typed.Details["candidates_named"])
	if err != nil || named < 1 || named >= packages {
		t.Fatalf("candidates_named = %q, want a count of the names shown, at least one and fewer than %d",
			typed.Details["candidates_named"], packages)
	}
	if want := "the first " + strconv.Itoa(named) + " by node id being: "; !strings.Contains(typed.Remediation, want) {
		t.Fatalf("the rejection does not state that it names the first %d: %s", named, typed.Remediation)
	}
	for i, id := range want {
		shown := strings.Contains(typed.Remediation, string(id)[:12])
		if i < named && !shown {
			t.Fatalf("candidate %d (%s) is one of the %d the rejection says it names, and it is named nowhere: %s",
				i, id, named, typed.Remediation)
		}
		if i >= named && shown {
			t.Fatalf("candidate %d (%s) is named past the %d the rejection says it names: %s", i, id, named, typed.Remediation)
		}
	}
}
