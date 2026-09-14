// Package app is the composition root: it wires configuration, the managed
// toolchain, storage and providers into the objects the command tree and the
// index coordinator consume. Nothing here owns behaviour of its own.
package app

import (
	"io"
	"log/slog"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// OpenToolchain is the composition root of the managed toolchain: it resolves
// the layered configuration for the repository at repo and builds one resolver
// over it, returning the resolver and the store path it reads. Logs go to
// stderr (Section 18.2), never to the result stream a --json consumer reads.
func OpenToolchain(repo string, stderr io.Writer) (*toolchain.Resolver, string, error) {
	cfg, err := config.Load(repo)
	if err != nil {
		return nil, "", err
	}
	// The two directories are distinct and are passed as such: config's data
	// directory is per workspace and the store under it is <data_dir>/tools,
	// while tools.cache_dir names the store itself -- which is how one store is
	// shared by every checkout on the machine. Folding the second into the first
	// would append "tools" to a path the user already pointed at the store.
	overrides := make(map[string]toolchain.Override, len(cfg.Tools.Override))
	for name, ov := range cfg.Tools.Override {
		overrides[name] = toolchain.Override(ov)
	}
	res, err := toolchain.New(toolchain.Options{
		DataDir:       cfg.Storage.DataDir,
		StoreDir:      cfg.Tools.CacheDir,
		Offline:       cfg.Tools.Offline,
		Mirror:        cfg.Tools.Mirror,
		MaxFetchBytes: cfg.Tools.MaxFetchBytes,
		FetchTimeout:  cfg.Tools.FetchTimeout.Std(),
		Overrides:     overrides,
		// Section 11.7 requires one record per completed fetch in ordinary
		// operation; Section 18.2 puts logs on stderr, never on the result
		// stream a --json consumer reads.
		Log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err != nil {
		return nil, "", err
	}
	// The reported path is the resolver's own, so the report can never name a
	// store other than the one it read.
	return res, res.StoreDir(), nil
}
