package tbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Builder feeds one generation's units through the store calls a real run
// makes for its search documents.
type Builder struct {
	Store  *sqlite.Store
	Gen    model.GenerationID
	Limits provider.Limits
	Pool   *provider.Pool
}

// NewBuilder takes the sink limits and the shared pool exactly as the
// coordinator and the composition root derive them from cfg.
func NewBuilder(s *sqlite.Store, gen model.GenerationID, cfg config.Config) (*Builder, error) {
	limits := provider.Limits{BatchRecords: cfg.Index.BatchRecords, BatchBytes: cfg.Index.BatchBytes,
		MaxRecordBytes: cfg.Resources.MaxProviderRecordBytes.Value()}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	pool, err := provider.NewPool(cfg.Index.QueueBytes)
	if err != nil {
		return nil, err
	}
	return &Builder{Store: s, Gen: gen, Limits: limits, Pool: pool}, nil
}

// Workers is the coordinator's per-provider build concurrency (workerCount).
func Workers(cfg config.Config) int { return min(config.BuildWorkers(cfg), provider.MaxLiveSinks) }

// UnitTimes is one unit's phases.
type UnitTimes struct {
	Begin, Nodes, Put, Seal time.Duration
}

// Build runs one unit: begin (inputs streamed and hashed), the referenced node
// facts through their own batch sink, the search documents through a batch
// sink over the unit writer (the production batching, sort and pool), the
// seal, and the run's completion. rec.Build.OriginRunID is replaced with the
// run this call opens, and every evidence row is pointed at it.
func (b *Builder) Build(ctx context.Context, rec UnitRecord, docs []model.SearchUnit) (UnitTimes, error) {
	var t UnitTimes
	s := b.Store
	spec := rec.Build.Spec
	t0 := time.Now()
	run, err := s.BeginProviderRun(ctx, b.Gen, spec.ProviderID, spec.ProviderVersion)
	if err != nil {
		return t, err
	}
	build := rec.Build
	build.OriginRunID = run
	w, err := s.BeginUnit(ctx, b.Gen, build, func(yield func(model.UnitInput) error) error {
		for _, in := range rec.Inputs {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return t, err
	}
	t1 := time.Now()
	t.Begin = t1.Sub(t0)
	out := provider.StoreUnit(s, w)
	fail := func(err error) (UnitTimes, error) {
		_ = out.Fail(context.WithoutCancel(ctx))
		return t, err
	}
	var records, bytes uint64
	if len(rec.Nodes) > 0 {
		nodes := make([]model.NodeFact, len(rec.Nodes))
		for i, f := range rec.Nodes {
			ev := make([]model.Evidence, len(f.Evidence))
			for j, e := range f.Evidence {
				e.OriginRunID = run
				ev[j] = e
			}
			f.Evidence = ev
			nodes[i] = f
		}
		sink, err := b.sink(ctx, out)
		if err != nil {
			return fail(err)
		}
		if err := sink.PutNodes(ctx, nodes); err != nil {
			sink.Discard()
			return fail(err)
		}
		err = sink.Flush(ctx)
		records, bytes = records+sink.Records(), bytes+sink.Bytes()
		sink.Discard()
		if err != nil {
			return fail(err)
		}
	}
	t2 := time.Now()
	t.Nodes = t2.Sub(t1)
	sink, err := b.sink(ctx, out)
	if err != nil {
		return fail(err)
	}
	if err := sink.PutSearchUnits(ctx, docs); err != nil {
		sink.Discard()
		return fail(err)
	}
	err = sink.Flush(ctx)
	records, bytes = records+sink.Records(), bytes+sink.Bytes()
	sink.Discard()
	if err != nil {
		return fail(err)
	}
	t3 := time.Now()
	t.Put = t3.Sub(t2)
	if err := out.Seal(ctx); err != nil {
		return fail(err)
	}
	t.Seal = time.Since(t3)
	res := model.ProviderResult{RunID: run, State: model.RunSucceeded, RecordsEmitted: records, BytesProcessed: bytes}
	return t, s.CompleteProviderRun(context.WithoutCancel(ctx), res, "")
}

func (b *Builder) sink(ctx context.Context, out provider.UnitOutput) (*provider.BatchSink, error) {
	return provider.NewBatchSink(ctx, out, b.Limits, b.Pool, func(error) {})
}

// StoreInfo is what the index arm leaves beside the database for the query
// and delta arms.
type StoreInfo struct {
	Repo model.RepositoryID `json:"repo"`
	Root string             `json:"root"`
	Docs string             `json:"docs"`
	// CAS is the content store directory the query arm hydrates from.
	CAS string `json:"cas"`
}

// InfoName is the StoreInfo file in the store directory.
const InfoName = "tbench.json"

// WriteInfo writes the store's StoreInfo.
func WriteInfo(dir string, info StoreInfo) error {
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, InfoName), b, 0o644)
}

// ReadInfo reads the store's StoreInfo.
func ReadInfo(dir string) (StoreInfo, error) {
	var info StoreInfo
	b, err := os.ReadFile(filepath.Join(dir, InfoName))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// ReadSource loads the exporter's snapshot record for docs.
func ReadSource(docs string) (Source, error) {
	var src Source
	b, err := os.ReadFile(filepath.Join(PrereqDir(docs), "source.json"))
	if err != nil {
		return src, err
	}
	return src, json.Unmarshal(b, &src)
}

// Stream walks every exported unit in seal order (units.jsonl) together with
// its documents (docs.jsonl, read in lockstep), holding one unit at a time. A
// unit that published no document is yielded with none.
func Stream(docsPath string, fn func(rec UnitRecord, docs []model.SearchUnit) error) error {
	f, err := os.Open(docsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	var pending *Doc
	next := func() (*Doc, error) {
		if pending != nil {
			d := pending
			pending = nil
			return d, nil
		}
		if !sc.Scan() {
			return nil, sc.Err()
		}
		var d Doc
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return nil, err
		}
		return &d, nil
	}
	err = Lines(filepath.Join(PrereqDir(docsPath), "units.jsonl"), func(line []byte) error {
		var rec UnitRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		docs := make([]model.SearchUnit, 0, rec.Docs)
		for len(docs) < rec.Docs {
			d, err := next()
			if err != nil {
				return err
			}
			if d == nil || d.Unit != rec.Unit {
				return fmt.Errorf("docs.jsonl and units.jsonl disagree at unit %s (%d of %d documents)", rec.Unit, len(docs), rec.Docs)
			}
			docs = append(docs, d.SearchUnit)
		}
		return fn(rec, docs)
	})
	if err != nil {
		return err
	}
	if d, err := next(); err != nil || d != nil {
		if err == nil {
			err = fmt.Errorf("docs.jsonl has documents of unit %s past the last unit", d.Unit)
		}
		return err
	}
	return nil
}

var errStop = errors.New("stop")

// ConfigHash is the analysis config hash the exported units were built under.
func ConfigHash(docsPath string) (string, error) {
	var h string
	err := Lines(filepath.Join(PrereqDir(docsPath), "units.jsonl"), func(line []byte) error {
		var rec UnitRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return err
		}
		h = rec.Build.AnalysisConfigHash
		return errStop
	})
	if err != nil && !errors.Is(err, errStop) {
		return "", err
	}
	if h == "" {
		return "", errors.New("units.jsonl records no unit")
	}
	return h, nil
}
