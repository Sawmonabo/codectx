// Command toollock is the release-time generator for the managed analyzer
// toolchain of Section 11.7. It downloads every pinned upstream distribution,
// verifies the digest upstream publishes, runs the build steps a distribution
// needs (npm ci for the Node packages, a cross `go install` for the Go
// programs), normalizes each tree into a deterministic tar.gz, publishes the
// payloads as assets of a tools-v<n> release, and writes
// internal/toolchain/tools.lock.json.
//
// It is never part of the shipped binary: the product only ever consumes the
// lock this program emits.
//
//	go run ./internal/tools/toollock                 # produce, publish, write the lock
//	go run ./internal/tools/toollock -check          # re-download every asset and confirm digests
//	go run ./internal/tools/toollock -tools joern    # limit to one tool
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	flagRepo     = flag.String("repo", "Sawmonabo/codectx", "GitHub repository owning the payload release")
	flagTag      = flag.String("tag", "tools-v1", "payload release tag")
	flagWork     = flag.String("work", ".agents/tmp/toollock", "scratch directory for downloads and trees")
	flagOut      = flag.String("out", "internal/toolchain/tools.lock.json", "lock file to write")
	flagLicenses = flag.String("licenses", "", "optional path to write the redistributed-payload license table")
	flagTools    = flag.String("tools", "", "comma-separated subset of tool names (default: every tool)")
	flagCheck    = flag.Bool("check", false, "re-download every published asset and confirm the lock digests")
	flagNoUpload = flag.Bool("no-upload", false, "produce payloads without publishing them")
	flagKeep     = flag.Bool("keep", false, "keep extracted trees and payloads instead of deleting them")
)

func logf(format string, args ...any) {
	log.Printf(format, args...)
}

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if *flagCheck {
		err = runCheck(ctx)
	} else {
		err = runGenerate(ctx)
	}
	if err != nil {
		log.Fatalf("toollock: %v", err)
	}
}

func selectedTools() (map[string]bool, bool) {
	if strings.TrimSpace(*flagTools) == "" {
		return nil, false
	}
	m := map[string]bool{}
	for _, n := range strings.Split(*flagTools, ",") {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m, true
}

func runGenerate(ctx context.Context) error {
	only, filtered := selectedTools()
	work, err := filepath.Abs(*flagWork)
	if err != nil {
		return err
	}
	for _, d := range []string{"dl", "tree", "out", "state", "npm", "gopath"} {
		if err := os.MkdirAll(filepath.Join(work, d), dirMode); err != nil {
			return err
		}
	}

	if !*flagNoUpload {
		if err := ensureRelease(ctx, *flagRepo, *flagTag); err != nil {
			return err
		}
	}

	specs := catalog()
	for _, spec := range specs {
		if filtered && !only[spec.Name] {
			continue
		}
		statePath := filepath.Join(work, "state", spec.Name+".json")
		if _, err := os.Stat(statePath); err == nil {
			logf("%s: already recorded, skipping", spec.Name)
			continue
		}
		entry, err := realizeTool(ctx, work, spec)
		if err != nil {
			return fmt.Errorf("%s: %w", spec.Name, err)
		}
		b, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(statePath, b, fileMode); err != nil {
			return err
		}
	}

	lock, err := assembleLock(work, specs)
	if err != nil {
		return err
	}
	if err := writeLock(lock, *flagOut); err != nil {
		return err
	}
	logf("wrote %s with %d tools", *flagOut, len(lock.Tools))
	if *flagLicenses != "" {
		if err := writeLicenseTable(lock, *flagLicenses); err != nil {
			return err
		}
		logf("wrote %s", *flagLicenses)
	}
	return nil
}

// assembleLock reads back every completed tool record so a resumed run produces
// the same lock as an uninterrupted one.
func assembleLock(work string, specs []toolSpec) (Lock, error) {
	lock := Lock{LockVersion: 1, Tools: map[string]Entry{}}
	for _, spec := range specs {
		b, err := os.ReadFile(filepath.Join(work, "state", spec.Name+".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return lock, err
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil {
			return lock, err
		}
		lock.Tools[e.Name] = e
	}
	return lock, nil
}

// realizeTool produces, publishes and records every platform payload of one
// tool. Each platform is downloaded, normalized, uploaded and then deleted
// before the next one starts, so the peak disk cost is one payload rather than
// the whole matrix, and no payload is ever held in memory.
func realizeTool(ctx context.Context, work string, spec toolSpec) (Entry, error) {
	entry := Entry{
		Name: spec.Name, Version: spec.Version, Kind: spec.Kind,
		License: spec.License, Upstream: spec.Upstream, Runtime: spec.Runtime,
		Entry: spec.Entry, Languages: spec.Languages, Notes: spec.Notes,
		Platforms: map[string]Payload{},
	}

	npmDirs := map[string]string{}
	entryDigests := map[string]bool{}

	for _, plat := range platformOrder {
		pp, ok := spec.Platforms[plat]
		if !ok {
			continue
		}
		// Per-platform state: a multi-gigabyte tool must not lose five finished
		// uploads because the sixth was interrupted.
		platState := filepath.Join(work, "state", spec.Name, plat+".json")
		if b, err := os.ReadFile(platState); err == nil {
			var p Payload
			if err := json.Unmarshal(b, &p); err != nil {
				return entry, err
			}
			entry.Platforms[plat] = p
			entryDigests[p.EntrySHA256] = true
			logf("%s/%s: already published, skipping", spec.Name, plat)
			continue
		}

		start := time.Now()
		treeDir := filepath.Join(work, "tree", spec.Name+"-"+plat)
		_ = os.RemoveAll(treeDir)

		var (
			provenance, upstreamDigest string
			payloadURL, payloadSum     string
			payloadSize                int64
			outPath                    string
		)
		switch {
		case pp.Src != nil:
			// Hybrid hosting: upstream publishes a real binary for this
			// platform, so the lock points at the upstream asset and pins that
			// file's own digest. Nothing is re-packed and nothing is uploaded.
			// The download below exists only to compute the digest, to check it
			// against what upstream publishes, and to resolve and hash the entry
			// inside the archive the runtime will extract.
			provenance = "upstream"
			name := baseName(pp.Src.URL)
			dl := filepath.Join(work, "dl", name)
			sum, size, err := download(ctx, pp.Src.URL, dl)
			if err != nil {
				return entry, err
			}
			logf("%s/%s: downloaded %s (%d bytes, sha256 %s)", spec.Name, plat, name, size, sum)
			upstreamDigest, err = verifyUpstream(ctx, dl, pp.Src.Digest, sum)
			if err != nil {
				return entry, err
			}
			if upstreamDigest == "" {
				logf("%s/%s: upstream publishes no digest for %s; the lock pins the sha256 observed here", spec.Name, plat, name)
			} else {
				logf("%s/%s: upstream digest %s verified", spec.Name, plat, upstreamDigest)
			}
			if err := extract(dl, treeDir, pp.Src.Kind, pp.Src.Strip, pp.Src.Dest); err != nil {
				return entry, err
			}
			// An upstream payload is pinned as published, so its expansion must
			// clear the runtime's bounds before the lock records it.
			if err := checkTreeBounds(spec.Name, plat, treeDir, size); err != nil {
				return entry, err
			}
			payloadURL, payloadSum, payloadSize = pp.Src.URL, sum, size
			if !*flagKeep {
				_ = os.Remove(dl)
			}
		case pp.Build != nil && pp.Build.Kind == "npm":
			provenance = "built"
			spec2, ok := npmBuilds[pp.Build.NPM]
			if !ok {
				return entry, fmt.Errorf("unknown npm build %q", pp.Build.NPM)
			}
			dir, built := npmDirs[spec2.ID]
			if !built {
				dir = filepath.Join(work, "npm", spec2.ID)
				_ = os.RemoveAll(dir)
				if err := buildNPM(ctx, spec2, dir); err != nil {
					return entry, err
				}
				npmDirs[spec2.ID] = dir
				logf("%s: npm build complete", spec.Name)
			}
			treeDir = dir
		case pp.Build != nil && pp.Build.Kind == "go":
			provenance = "built"
			if err := buildGo(ctx, pp.Build.Pkg, plat, work, treeDir, pp.Entry); err != nil {
				return entry, err
			}
			logf("%s/%s: cross-built %s", spec.Name, plat, pp.Build.Pkg)
		default:
			return entry, fmt.Errorf("%s: no source or build for %s", spec.Name, plat)
		}

		entryRel, err := resolveEntry(treeDir, pp.Entry)
		if err != nil {
			return entry, err
		}
		entrySum, err := hashFile(filepath.Join(treeDir, filepath.FromSlash(entryRel)))
		if err != nil {
			return entry, err
		}
		entryDigests[entrySum] = true

		if pp.Src == nil {
			// Nothing upstream to point at: pack the built tree and host it.
			assetName := payloadName(spec.Name, spec.Version, plat)
			outPath = filepath.Join(work, "out", assetName)
			payloadSum, payloadSize, err = packDeterministic(treeDir, outPath)
			if err != nil {
				return entry, err
			}
			payloadURL = assetURL(*flagRepo, *flagTag, assetName)
			logf("%s/%s: packed %s (%d bytes, sha256 %s) in %s", spec.Name, plat, assetName, payloadSize, payloadSum, time.Since(start).Round(time.Second))
			// A hosted payload's compressed size is only known once it is
			// packed, so its bounds check happens here rather than at extraction.
			if err := checkTreeBounds(spec.Name, plat, treeDir, payloadSize); err != nil {
				return entry, err
			}
			switch {
			case *flagNoUpload:
			default:
				// Packing is deterministic, so an asset already published at the
				// right size is already the right bytes; re-uploading it would
				// only spend bandwidth.
				if got, err := remoteAssetSize(ctx, *flagRepo, *flagTag, assetName); err == nil && got == payloadSize {
					logf("%s/%s: %s already published at %d bytes, not re-uploading", spec.Name, plat, assetName, got)
					break
				}
				if err := uploadAsset(ctx, *flagRepo, *flagTag, outPath, payloadSize); err != nil {
					return entry, err
				}
			}
		}

		payload := Payload{
			URL:            payloadURL,
			SHA256:         payloadSum,
			Size:           payloadSize,
			Entry:          entryRel,
			EntrySHA256:    entrySum,
			Provenance:     provenance,
			UpstreamDigest: upstreamDigest,
		}
		entry.Platforms[plat] = payload
		if b, err := json.MarshalIndent(payload, "", "  "); err == nil {
			if err := os.MkdirAll(filepath.Dir(platState), dirMode); err != nil {
				return entry, err
			}
			if err := os.WriteFile(platState, b, fileMode); err != nil {
				return entry, err
			}
		}

		if !*flagKeep {
			if outPath != "" {
				_ = os.Remove(outPath)
			}
			if pp.Build == nil || pp.Build.Kind != "npm" {
				_ = os.RemoveAll(treeDir)
			}
		}
	}

	if len(entry.Platforms) == 0 {
		return entry, fmt.Errorf("no platforms realized")
	}
	// The tool-level entry digest is only meaningful when one file is the entry
	// on every platform in the map. For a native per-platform launcher it is
	// left empty and the per-platform digest governs.
	for _, plat := range platformOrder {
		if p, ok := entry.Platforms[plat]; ok {
			// The catalog's tool-level entry may carry a glob for an
			// upstream-versioned directory or jar; the lock must not.
			entry.Entry = p.Entry
			break
		}
	}
	if len(entryDigests) == 1 && sameEntryPath(entry) {
		for d := range entryDigests {
			entry.EntrySHA256 = d
		}
	}
	if !*flagKeep {
		for _, d := range npmDirs {
			_ = os.RemoveAll(d)
		}
	}
	return entry, nil
}

func sameEntryPath(e Entry) bool {
	seen := ""
	for _, p := range e.Platforms {
		if seen == "" {
			seen = p.Entry
			continue
		}
		if p.Entry != seen {
			return false
		}
	}
	return true
}

// resolveEntry turns a catalog entry path, which may carry one glob segment for
// an upstream-versioned jar name, into the exact payload-relative path.
func resolveEntry(root, pattern string) (string, error) {
	if _, err := confinedRel(pattern); err != nil {
		return "", err
	}
	if !strings.Contains(pattern, "*") {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(pattern))); err != nil {
			return "", fmt.Errorf("entry %s: %w", pattern, err)
		}
		return pattern, nil
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("entry pattern %s matched %d files, want exactly 1", pattern, len(matches))
	}
	rel, err := filepath.Rel(root, matches[0])
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func writeLicenseTable(l Lock, path string) error {
	var b strings.Builder
	b.WriteString("| Payload | Version | License | Upstream |\n|---|---|---|---|\n")
	for _, n := range l.Names() {
		e := l.Tools[n]
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", e.Name, e.Version, e.License, e.Upstream)
	}
	return os.WriteFile(path, []byte(b.String()), fileMode)
}
