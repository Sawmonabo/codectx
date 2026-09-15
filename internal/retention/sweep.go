package retention

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// component is the slog component every entry of this package carries.
const component = "retention"

const (
	// serverWorkDirName is the language-server work-directory root under the
	// data directory. It mirrors internal/provider/lsp's workDirName, which is
	// unexported there; the layout is
	// <data_dir>/lsp/<server name>/<tool fingerprint digest>.
	serverWorkDirName = "lsp"

	// serverConfigStagingPrefix is the prefix of the staging directories
	// jdtls's configuration seeding creates with os.MkdirTemp inside a work
	// directory (provider/lsp/jdtls.go:65). Its deferred removal covers every
	// Go return path but not a kill, so an abandoned one is the residue this
	// pass reclaims.
	serverConfigStagingPrefix = "config-"

	// serverConfigStagingGrace is how long an abandoned staging directory is
	// left alone before it is reclaimed. The language-server manager is on the
	// query path and is NOT excluded by the two locks Collect's caller holds,
	// so a staging directory younger than this window may be the one a live
	// seedPlatformConfig is filling right now. snapshot.Sweep and
	// (*Spools).Sweep both guard their temporaries the same way.
	serverConfigStagingGrace = 15 * time.Minute

	// The work-directory scans below are deliberately unbounded in entry count.
	// os.ReadDir has already materialised the whole listing by the time a slice
	// could bound it, so the former entries[:4096] spent the memory and then
	// SILENTLY dropped the remainder -- every pass reclaiming the same first
	// 4096 names and never reaching the rest. The listing is one directory of
	// names, not a traversal, and reclaiming is idempotent.
)

// ToolPins reports the tool payloads the lock currently pins, as a complete map
// from lock-entry name to that entry's resolved fingerprint digest
// (toolchain.Tool.FingerprintDigest, the last path component of a language
// server's work directory).
//
// It is declared here rather than in the frozen Options because L0 froze no
// digest source: ToolCollector carries GC alone, and no method on
// *toolchain.Resolver returns name -> digest today (PinnedFingerprint takes one
// name; Status carries no digest). This is the L3b -> INT seam named in the
// report: INT adds that one method to *toolchain.Resolver and hands the same
// value in as the collector's ToolCollector, which is why the capability is
// discovered by assertion on the collector's existing tool dependency instead
// of through a second Options field this lane may not add.
//
// CONTRACT: a name the map does not carry is left entirely alone, and only the
// digests under a name it does carry are reclaimed. That is deliberately the
// weaker of the two possible rules, because a name is legitimately absent from
// what the lock can pin: (*Resolver).pinned refuses a tool the user has
// overridden ("whose identity the lock does not pin", resolver.go:311) and one
// with no payload for this platform, yet both still run and both still own a
// work directory. Reading absence as "nothing pins this" would delete the
// directory of a running overridden server. An empty map is skipped outright
// for the same reason one step further: it is indistinguishable from a lock
// that could not be read.
type ToolPins interface {
	PinnedFingerprints() map[string]string
}

// sweep gives five landed, caller-less helpers their first caller, in
// dependency order -- ExpireSessions, then PruneSessions (the first reader of
// the `storage.closed_session_retention` key, which had none), then
// (*Spools).Sweep, snapshot.Sweep and (*Resolver).GC -- and then reclaims the
// one tree nothing reclaims, <data_dir>/lsp.
//
// Order is a contract, not a listing order. Expiring a live session past its
// deadline is what makes it a closed session, so pruning before expiring would
// leave this pass's newly closed sessions for the next one; and both must
// precede the spool and blob work, because the lease a pruned session held is
// what makes a spool or a blob unreferenced in this pass rather than the next.
//
// Every step runs even when an earlier one failed, and the failures are joined:
// a tool store busy with an install must not cost this pass its session
// pruning. It never deletes retained source -- that is the grace protocol's
// (grace.go), not this pass's.
func (c *Collector) sweep(ctx context.Context) (Report, error) {
	now := c.opts.Now()
	limit := c.opts.Config.BatchLimit
	var report Report
	var errs []error

	expired, err := c.opts.Sessions.ExpireSessions(ctx, now, limit)
	report.SessionsExpired = expired
	errs = append(errs, err)

	pruned, err := c.opts.Sessions.PruneSessions(ctx, now, c.opts.Config.ClosedSessionRetention, limit)
	report.SessionsPruned = pruned
	errs = append(errs, err)

	// (*Spools).Sweep returns the live byte total it reconciled the budget
	// against, not a count of files removed, which is why the field it lands in
	// is named for bytes.
	liveSpoolBytes, err := c.opts.Spools.Sweep(ctx, now)
	report.SpoolBytesSwept = liveSpoolBytes
	errs = append(errs, err)

	errs = append(errs, c.opts.Snapshot.SweepSnapshots(c.opts.Config.DataDir))

	collected, err := c.opts.Tools.GC(ctx)
	report.ToolsCollected = collected
	errs = append(errs, err)

	errs = append(errs, c.sweepServerWorkDirs(ctx, now))
	return report, errors.Join(errs...)
}

// sweepServerWorkDirs reclaims <data_dir>/lsp: every <digest> under a pinned
// name that is not the pinned digest, and the abandoned config-* staging
// directories under the pinned one. A name the pinned set does not carry is
// left alone -- see the ToolPins contract.
// internal/provider/lsp states in its own comments that nothing reclaims this
// tree and that the reclaim belongs to whoever owns data-directory retention;
// this is that owner.
//
// Removal is safe here only because of the lock order stated in retention.go
// plus the digest test: the work directory the running server holds is the
// pinned one, and the pinned one is never removed.
func (c *Collector) sweepServerWorkDirs(ctx context.Context, now time.Time) error {
	var pinned map[string]string
	if pins, ok := c.opts.Tools.(ToolPins); ok {
		pinned = pins.PinnedFingerprints()
	}
	if len(pinned) == 0 {
		// Both shapes of "nothing is known": a tool collector that implements
		// no oracle, and one whose oracle came back empty -- indistinguishable
		// from a lock that could not be read. Neither is an error, because a
		// reclaim that cannot run must not cost this pass its session pruning
		// or, through Collect's gate, the blob grace protocol. It is warned
		// rather than skipped silently: unreclaimed disk grows without bound
		// and an operator has to be able to see why.
		slog.Warn("language-server work directories were not reclaimed: no pinned payload is known",
			"component", component)
		return nil
	}
	root := filepath.Join(c.opts.Config.DataDir, serverWorkDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return sweepError("the language-server work directory root cannot be listed", err)
	}
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if !e.IsDir() {
			// Nothing this product writes here is a plain file; leaving it is
			// the conservative half of the one invariant this pass protects.
			continue
		}
		name := e.Name()
		digest, isPinned := pinned[name]
		if !isPinned {
			// An overridden or unsupported server is absent from the pinned set
			// and still live; see the ToolPins contract. Its tree is the INT
			// ask, not this pass's to guess at.
			continue
		}
		errs = append(errs, c.sweepServerVersions(ctx, root, name, digest, now))
	}
	return errors.Join(errs...)
}

// sweepServerVersions handles one pinned server: every payload directory but
// the pinned one goes, and the pinned one keeps only its live staging.
func (c *Collector) sweepServerVersions(ctx context.Context, root, name, pinnedDigest string, now time.Time) error {
	dir := filepath.Join(root, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return sweepError("a language-server work directory cannot be listed", err)
	}
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if !e.IsDir() {
			continue
		}
		if e.Name() == pinnedDigest {
			errs = append(errs, c.sweepConfigStaging(filepath.Join(dir, e.Name()), now))
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			errs = append(errs, sweepError("a superseded language-server work directory was not removed", err))
			continue
		}
		slog.Info("a superseded language-server work directory was reclaimed",
			"component", component, "server", name)
	}
	return errors.Join(errs...)
}

// sweepConfigStaging removes the abandoned config-* staging directories inside
// a live work directory. A staging directory younger than the grace window is
// left alone: the language-server manager runs on the query path, outside the
// locks Collect's caller holds, so a fresh one may be the copy a start is
// filling right now. The seeded "config" directory itself is never touched --
// removing it would leave the payload unable to start with no way back.
func (c *Collector) sweepConfigStaging(workDir string, now time.Time) error {
	entries, err := os.ReadDir(workDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return sweepError("a language-server work directory cannot be listed", err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), serverConfigStagingPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			errs = append(errs, sweepError("an abandoned language-server staging directory cannot be inspected", err))
			continue
		}
		if now.Sub(info.ModTime()) < serverConfigStagingGrace {
			continue
		}
		if err := os.RemoveAll(filepath.Join(workDir, e.Name())); err != nil {
			errs = append(errs, sweepError("an abandoned language-server staging directory was not removed", err))
			continue
		}
		slog.Info("an abandoned language-server staging directory was reclaimed", "component", component)
	}
	return errors.Join(errs...)
}

// sweepError is this pass's typed failure. It names what could not be done and
// carries only the bare cause, never the path the filesystem error wraps: the
// data directory is a private absolute root, which Section 21 keeps out of
// ordinary reporting. A disk-full failure keeps its own family so an operator
// is told to free space rather than to report a defect.
//
// The bare-cause unwrapping is the third copy in the tree (snapshot.bareCause,
// toolchain's ioError); both are unexported, so this lane cannot reuse one.
// Consolidating them into one exported helper is the ask recorded for INT.
func sweepError(what string, err error) error {
	var pe *fs.PathError
	cause := err
	if errors.As(err, &pe) {
		cause = pe.Err
	}
	if errors.Is(err, syscall.ENOSPC) {
		return &model.Error{Code: model.CodeDiskFull,
			Message:     "retention: " + what + ": the data directory's disk is full",
			Remediation: "free disk space or move storage.data_dir to a larger volume"}
	}
	return &model.Error{Code: model.CodeInternal,
		Message:     "retention: " + what + ": " + cause.Error(),
		Remediation: "check the data directory's permissions and free space, then run collection again"}
}
