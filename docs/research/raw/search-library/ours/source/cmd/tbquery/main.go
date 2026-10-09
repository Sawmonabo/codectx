// Command tbquery (benchmark harness) answers each query through the search
// package's lexical first-page path, as `codectx search` composes it:
//
//	tbquery <store dir> <queries.txt> <runs>
//
// Each query runs in a fresh process: one cold run, then <runs> warm runs.
// Results go to <dir of docs.jsonl>/results/ours-query.jsonl.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/tbench"
)

const pageSize = 200

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

var kinds, prefixes multi

// filter is the request's filters exactly as model.SearchRequest carries them.
func filter() search.BenchFilter {
	var f search.BenchFilter
	for _, k := range kinds {
		f.Kinds = append(f.Kinds, model.NodeKind(k))
	}
	f.Paths = prefixes
	return f
}

// forwarded is the filter flags as a child's arguments.
func forwarded() []string {
	var args []string
	for _, k := range kinds {
		args = append(args, "--kind", k)
	}
	for _, p := range prefixes {
		args = append(args, "--path-prefix", p)
	}
	return args
}

func main() {
	one := flag.String("one", "", "internal: answer this one query in this process")
	flag.Var(&kinds, "kind", "keep only hits of this node kind, as `codectx search --kind` (repeatable)")
	flag.Var(&prefixes, "path-prefix", "keep only hits under this root-relative path, as SearchRequest.paths (repeatable; whole path segments)")
	flag.Parse()
	if flag.NArg() != 3 {
		fmt.Fprintln(os.Stderr, "usage: tbquery [--kind K]... [--path-prefix P]... <store dir> <queries.txt> <runs>")
		os.Exit(2)
	}
	runs, err := strconv.Atoi(flag.Arg(2))
	if err != nil || runs < 0 {
		fmt.Fprintln(os.Stderr, "tbquery: runs must be a non-negative integer")
		os.Exit(2)
	}
	if *one != "" {
		err = child(flag.Arg(0), *one, runs)
	} else {
		err = parent(flag.Arg(0), flag.Arg(1), runs)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tbquery:", err)
		os.Exit(1)
	}
}

func parent(dir, queries string, runs int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.Open(queries)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		q := sc.Text()
		if q == "" {
			continue
		}
		args := append(append([]string{"-one", q}, forwarded()...), dir, queries, strconv.Itoa(runs))
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("query %q: %w", q, err)
		}
	}
	return sc.Err()
}

type row struct {
	Arm        string    `json:"arm"`
	Phase      string    `json:"phase"`
	Query      string    `json:"query"`
	Run        int       `json:"run"`
	WallMS     float64   `json:"wall_ms"`
	Hits       int64     `json:"hits"`
	Page       []string  `json:"page"`
	ServedHits int64     `json:"served_hits"`
	ServedPage []string  `json:"served_page"`
	OpenMS     float64   `json:"open_ms,omitempty"`
	Filter     rowFilter `json:"filter"`
}

// rowFilter is the filter a result line was answered under; empty lists mean
// no filter.
type rowFilter struct {
	Kinds      []string `json:"kinds"`
	PathPrefix []string `json:"path_prefixes"`
}

func msf(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func child(dir, query string, runs int) error {
	ctx := context.Background()
	t0 := time.Now()
	dir, _ = filepath.Abs(dir)
	info, err := tbench.ReadInfo(dir)
	if err != nil {
		return err
	}
	cfg, err := tbench.Config(info.Root, dir)
	if err != nil {
		return err
	}
	// The query composition (app.openStack in modeQuery).
	cas, err := snapshot.OpenCASForReading(info.CAS)
	if err != nil {
		return err
	}
	store, err := tbench.OpenStore(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer store.Close()
	signer, err := pagination.OpenSignerForReading(dir)
	if err != nil {
		return err
	}
	var spoolShare int64
	if total := cfg.Resources.MaxTempBytes; total > 0 {
		spoolShare = max(total/8, 1)
	}
	spools, err := pagination.NewSpools(filepath.Join(dir, "work", "spools"), spoolShare, store)
	if err != nil {
		return err
	}
	leases := pagination.NewLeases(store, cfg.Storage.QueryCursorTTL.Std())
	svc, err := search.New(search.Options{Store: store, Repo: info.Repo, Signer: signer, Spools: spools, Leases: leases,
		Content: cas, Resources: cfg.Resources, CursorTTL: cfg.Storage.QueryCursorTTL.Std(), Now: time.Now})
	if err != nil {
		return err
	}
	open := time.Since(t0)

	results, err := tbench.OpenAppend(filepath.Join(tbench.ResultsDir(filepath.Dir(info.Docs)), "ours-query.jsonl"))
	if err != nil {
		return err
	}
	defer results.Close()
	var answers []search.BenchAnswer
	var walls []time.Duration
	for range runs + 1 {
		start := time.Now()
		a, err := svc.BenchLexical(ctx, query, pageSize, filter())
		walls = append(walls, time.Since(start))
		if err != nil {
			return err
		}
		svc.BenchRelease(a)
		answers = append(answers, a)
	}
	// The document-level page, outside every timing.
	docs, page, err := svc.BenchDocPage(ctx, query, pageSize, filter())
	if err != nil {
		return err
	}
	for i, a := range answers {
		if a.Docs != docs {
			return fmt.Errorf("query %q: run %d matched %d documents, the document pass %d", query, i, a.Docs, docs)
		}
		r := row{Arm: "ours", Phase: "warm", Query: query, Run: i, WallMS: msf(walls[i]), Hits: a.Docs,
			Page: append([]string{}, page...), ServedHits: a.Served, ServedPage: append([]string{}, a.Keys...),
			Filter: rowFilter{Kinds: append([]string{}, kinds...), PathPrefix: append([]string{}, prefixes...)}}
		if i == 0 {
			r.Phase, r.OpenMS = "cold", msf(open)
		}
		if err := results.Write(r); err != nil {
			return err
		}
	}
	unhydrated := 0
	for _, h := range answers[0].Hits {
		if h.Range == nil {
			unhydrated++
		}
	}
	b, _ := json.Marshal(map[string]any{"query": query, "kinds": kinds, "path_prefixes": prefixes, "hits": docs, "served_hits": answers[0].Served,
		"served_page": len(answers[0].Hits), "page_without_range": unhydrated,
		"cold_ms": msf(walls[0]), "warm_ms": warmSummary(walls[1:]), "open_ms": msf(open)})
	fmt.Println(string(b))
	return nil
}

func warmSummary(w []time.Duration) []float64 {
	out := make([]float64, len(w))
	for i, d := range w {
		out[i] = msf(d)
	}
	return out
}
