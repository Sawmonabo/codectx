package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// maxDownloadBytes bounds every upstream read. The largest pinned distribution
// (the Java runtime archive) is about two hundred megabytes; anything far above
// that is a mirror serving something other than the pinned artifact.
const maxDownloadBytes = 4 << 30

var httpClient = &http.Client{Timeout: 30 * time.Minute}

// download streams url into dest, never holding the body in memory, and returns
// the SHA-256 of the bytes written.
func download(ctx context.Context, url, dest string) (string, int64, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		digest, size, err := downloadOnce(ctx, url, dest)
		if err == nil {
			return digest, size, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		logf("download attempt %d/%d failed for %s: %v", attempt, 3, url, err)
		time.Sleep(time.Duration(attempt) * 3 * time.Second)
	}
	return "", 0, lastErr
}

func downloadOnce(ctx context.Context, url, dest string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownloadBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		paced.Remove(dest)
		return "", 0, err
	}
	if n > maxDownloadBytes {
		paced.Remove(dest)
		return "", 0, fmt.Errorf("GET %s: body above %d-byte cap", url, int64(maxDownloadBytes))
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// fetchText reads a small upstream text file (a checksum manifest) with a hard
// byte cap.
func fetchText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// digestSpec describes the digest upstream publishes for an artifact. An empty
// spec means upstream publishes none; that is recorded honestly in the report
// rather than papered over with a self-computed value called "verified".
type digestSpec struct {
	Algo  string // "sha256"
	URL   string // checksum file to fetch
	Match string // artifact file name to select inside a multi-line manifest
}

func (d digestSpec) empty() bool { return d.Algo == "" }

// resolve returns the expected lowercase hex digest for the artifact.
func (d digestSpec) resolve(ctx context.Context) (string, error) {
	if d.URL == "" {
		return "", fmt.Errorf("digest spec carries no value")
	}
	body, err := fetchText(ctx, d.URL)
	if err != nil {
		return "", err
	}
	return parseChecksumFile(body, d.Match)
}

func parseChecksumFile(body, match string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 1 && match == "" {
			return strings.ToLower(fields[0]), nil
		}
		if len(fields) >= 2 {
			name := strings.TrimPrefix(fields[len(fields)-1], "*")
			// Upstream manifests sometimes carry a build-relative path.
			if idx := strings.LastIndex(name, "/"); idx >= 0 {
				name = name[idx+1:]
			}
			if match == "" || name == match {
				return strings.ToLower(fields[0]), nil
			}
		}
	}
	return "", fmt.Errorf("no checksum line for %q", match)
}

// verifyUpstream compares the SHA-256 of path, already computed as it was
// downloaded, with the digest upstream published. A mismatch aborts the run: a payload we cannot tie to the upstream
// artifact must never be re-hosted under the codectx release.
func verifyUpstream(ctx context.Context, path string, spec digestSpec, sha256Hex string) (string, error) {
	if spec.empty() {
		return "", nil
	}
	want, err := spec.resolve(ctx)
	if err != nil {
		return "", err
	}
	if spec.Algo != "sha256" {
		return "", fmt.Errorf("unknown digest algorithm %q", spec.Algo)
	}
	if sha256Hex != want {
		return "", fmt.Errorf("upstream digest mismatch for %s: want %s, got %s", path, want, sha256Hex)
	}
	return spec.Algo + ":" + want, nil
}
