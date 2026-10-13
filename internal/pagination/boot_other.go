//go:build !linux && !darwin

package pagination

// bootIdentity is empty where the platform offers no identity that stays
// fixed for one boot: a boot time that moves when the clock is stepped would
// expire live state. Continuation state there is not boot-scoped
// (BootKnown).
func bootIdentity() string { return "" }
