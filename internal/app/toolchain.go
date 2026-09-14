// Package app is the composition root: it wires configuration, the managed
// toolchain, storage and providers into the objects the command tree and the
// index coordinator consume. Nothing here owns behaviour of its own.
package app

import (
	"io"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/toolchain"
)

// OpenToolchain is the composition root of the managed toolchain: it resolves
// the layered configuration for the repository at repo and builds one resolver
// over it, returning the resolver and the store path it reads. Logs go to
// stderr (Section 18.2), never to the result stream a --json consumer reads.
//
// The resolver is the fetching one: a `tools prefetch` exists to install, and
// the read-only reports call res.Status, which installs nothing of its own. A
// composition that must not fetch at all builds its resolver through
// openResolver instead.
func OpenToolchain(repo string, stderr io.Writer) (*toolchain.Resolver, string, error) {
	cfg, err := config.Load(repo)
	if err != nil {
		return nil, "", err
	}
	return openResolver(cfg, stderr, false)
}
