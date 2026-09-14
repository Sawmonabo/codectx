package main

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ensureRelease creates the tools release if it does not exist yet. Payload
// assets live in their own release so a product release never has to carry
// several gigabytes of analyzer payloads.
func ensureRelease(ctx context.Context, repo, tag string) error {
	if err := exec.CommandContext(ctx, "gh", "release", "view", tag, "--repo", repo).Run(); err == nil {
		return nil
	}
	notes := "Managed analyzer toolchain payloads for codectx (Section 11.7).\n\n" +
		"Every asset is a normalized, deterministic `<tool>-<version>-<os>-<arch>.tar.gz`.\n" +
		"The embedded `internal/toolchain/tools.lock.json` pins each asset by SHA-256 and size;\n" +
		"the product verifies both before anything is extracted or executed.\n" +
		"Produced by `go run ./internal/tools/toollock`; re-verify with `go run ./internal/tools/toollock -check`."
	out, err := exec.CommandContext(ctx, "gh", "release", "create", tag,
		"--repo", repo, "--title", tag, "--notes", notes).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh release create %s: %w\n%s", tag, err, out)
	}
	logf("created release %s: %s", tag, strings.TrimSpace(string(out)))
	return nil
}

// uploadAsset publishes one payload, retrying a transient upload. The remote
// size is read back afterwards: a truncated upload that still exits zero would
// otherwise be published under a digest nothing on the wire matches.
func uploadAsset(ctx context.Context, repo, tag, path string, want int64) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		start := time.Now()
		out, err := exec.CommandContext(ctx, "gh", "release", "upload", tag, path,
			"--repo", repo, "--clobber").CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("gh release upload: %w\n%s", err, truncate(string(out), 2000))
			logf("upload attempt %d/3 failed for %s: %v", attempt, path, lastErr)
			continue
		}
		got, err := remoteAssetSize(ctx, repo, tag, baseName(path))
		if err != nil {
			lastErr = err
			continue
		}
		if got != want {
			lastErr = fmt.Errorf("published %s is %d bytes, expected %d", baseName(path), got, want)
			logf("%v; retrying", lastErr)
			continue
		}
		logf("uploaded %s (%d bytes) in %s", baseName(path), want, time.Since(start).Round(time.Second))
		return nil
	}
	return lastErr
}

func remoteAssetSize(ctx context.Context, repo, tag, name string) (int64, error) {
	out, err := exec.CommandContext(ctx, "gh", "release", "view", tag, "--repo", repo,
		"--json", "assets", "--jq", fmt.Sprintf(`.assets[] | select(.name=="%s") | .size`, name)).Output()
	if err != nil {
		return 0, fmt.Errorf("gh release view assets: %w", err)
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("asset %s absent from release %s", name, tag)
	}
	return strconv.ParseInt(s, 10, 64)
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

func assetURL(repo, tag, name string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, name)
}

// payloadName is the Section 24 asset name: <tool>-<version>-<os>-<arch>.tar.gz.
// GitHub rewrites any character outside [A-Za-z0-9._-] in an asset name, so a
// version carrying one (Temurin's "21.0.12.1+1") is sanitized here rather than
// silently renamed on the server, which would break the read-back and the URL
// the lock pins.
func payloadName(tool, version, platform string) string {
	return fmt.Sprintf("%s-%s-%s.tar.gz", tool, sanitizeAssetPart(version), strings.ReplaceAll(platform, "_", "-"))
}

func sanitizeAssetPart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
