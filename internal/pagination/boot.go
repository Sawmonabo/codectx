package pagination

import "sync"

// The boot identity scopes every piece of continuation state to the boot of
// the host that wrote it. Continuation state is written and never synced to
// stable storage -- it lives one cursor lease and the sweep reclaims it, as a
// database's temporary files do, and those are removed at restart rather than
// trusted after one -- so a power loss can leave any of its files short or
// zero-filled. Stamping the boot in every header and treating another boot's
// state as expired (Spools.live) makes all of it unreadable after the restart
// a power loss implies: a cursor that names it answers CTX_CURSOR_INVALID and
// the sweep removes it, and no torn file is ever read as rows.
//
// A process crash needs none of this: what was written reached the kernel,
// and the next process on the same boot reads it whole.

var (
	bootOnce  sync.Once
	bootValue string
)

// thisBoot is the identity of the running boot, or empty where the platform
// offers none that stays fixed for a boot (bootIdentity).
func thisBoot() string {
	bootOnce.Do(func() { bootValue = bootIdentity() })
	return bootValue
}

// BootKnown reports whether continuation state is scoped to the boot on this
// host. Where it is not, a writer whose state must survive a power loss in
// order to be read correctly keeps syncing it.
func BootKnown() bool { return thisBoot() != "" }
