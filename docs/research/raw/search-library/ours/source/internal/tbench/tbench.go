// Package tbench is benchmark harness code (throwaway worktree): the record
// shapes the exporter writes beside docs.jsonl and the index arm reads back.
package tbench

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Doc is one docs.jsonl line: the exact SearchUnit plus its unit.
type Doc struct {
	model.SearchUnit
	Unit string `json:"unit"`
}

// UnitRecord is the store-side prerequisite of one unit: the build the real
// run began it with, its inputs, and the node facts its documents reference
// (each trimmed to the one evidence row the store requires).
type UnitRecord struct {
	Unit   string            `json:"unit"`
	Build  model.UnitBuild   `json:"build"`
	Inputs []model.UnitInput `json:"inputs"`
	Nodes  []model.NodeFact  `json:"nodes"`
	Docs   int               `json:"docs"`
}

// Source is the snapshot the real run captured, as the store holds it.
type Source struct {
	Repo model.RepositoryID `json:"repo"`
	Root string             `json:"root"`
	Ref  string             `json:"ref"`
	// DataDir is the real run's data directory; its content store is what
	// the query arm hydrates source ranges from.
	DataDir  string              `json:"data_dir"`
	Snapshot model.Snapshot      `json:"snapshot"`
	Files    []model.FileVersion `json:"files"`
	Blobs    []model.BlobRecord  `json:"blobs"`
}

// PrereqDir is where the exporter's side files live for a docs.jsonl.
func PrereqDir(docs string) string { return filepath.Join(filepath.Dir(docs), "prereq") }

// UnitKey is the contract's unit value.
func UnitKey(provider, scope string) string { return provider + "/" + scope }

// Lines streams a JSON-lines file.
func Lines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Usage is the process's CPU and peak RSS so far.
type Usage struct {
	CPU     time.Duration
	PeakRSS int64
}

// Rusage reads getrusage(RUSAGE_SELF).
func Rusage() Usage {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	cpu := time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	return Usage{CPU: cpu, PeakRSS: ru.Maxrss * 1024}
}

// DirBytes is the apparent size of every regular file under dir.
func DirBytes(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// Appender writes JSON lines to a results file.
type Appender struct {
	f *os.File
	w *bufio.Writer
}

// OpenAppend opens path for appending, creating its directory.
func OpenAppend(path string) (*Appender, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Appender{f: f, w: bufio.NewWriter(f)}, nil
}

// Write appends one value as a line.
func (a *Appender) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = a.w.Write(b)
	return err
}

// Close flushes and closes.
func (a *Appender) Close() error {
	if err := a.w.Flush(); err != nil {
		return err
	}
	return a.f.Close()
}

// ResultsDir is the contract's results directory: <bench>/results, <bench> being the
// directory that holds docs.jsonl.
func ResultsDir(docsOrStoreParent string) string { return filepath.Join(docsOrStoreParent, "results") }
