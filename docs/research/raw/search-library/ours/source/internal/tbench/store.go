package tbench

import (
	"context"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// DatabaseName is the composition root's database file name (app.databaseName).
const DatabaseName = "codectx.db"

// Config is the default configuration as Load resolves it for a workspace
// whose data directory is dataDir: built-in defaults, the user file and the
// project file merged, then the data directory pinned.
func Config(root, dataDir string) (config.Config, error) {
	cfg, err := config.Load(root)
	if err != nil {
		return cfg, err
	}
	cfg.Storage.DataDir = dataDir
	return cfg, nil
}

// OpenStore opens the database exactly as app.openStack does: readOnly is the
// query composition (codectx search), otherwise the indexing composition.
func OpenStore(ctx context.Context, cfg config.Config, readOnly bool) (*sqlite.Store, error) {
	return sqlite.Open(ctx, filepath.Join(cfg.Storage.DataDir, DatabaseName), sqlite.Options{
		BusyTimeout:        cfg.Storage.BusyTimeout.Std(),
		ReadConnections:    config.ReadConnections(cfg),
		PostingConnections: config.PostingConnections(cfg),
		WriterCacheKiB:     cfg.Storage.WriterCacheKiB,
		ReaderCacheKiB:     cfg.Storage.ReaderCacheKiB,
		BatchRecords:       cfg.Index.BatchRecords,
		BatchBytes:         cfg.Index.BatchBytes,
		MaxJSONBytes:       cfg.Context.MaxManifestBytes.Value(),
		MaxEvidencePerFact: int(cfg.Index.MaxEvidencePerFact.ValueOr(model.MaxEvidencePerFact)),
		Synchronous:        cfg.Storage.Synchronous,
		ReadOnly:           readOnly,
	})
}
