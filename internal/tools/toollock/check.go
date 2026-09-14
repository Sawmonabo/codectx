package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// runCheck re-downloads every payload the lock names and confirms that the
// published bytes still hash to the pinned digest, that the size agrees, and
// that the entry inside the payload hashes to the pinned entry digest. It is
// the release gate for "the lock describes what is actually published".
//
// The lock mixes upstream URLs with the assets this project hosts, and -check
// makes no distinction: every URL is fetched again and digested.
//
// No payload is ever held in memory. A gzip payload -- tar.gz or a bare gzip
// member -- is hashed as it streams. A zip needs its central directory, which
// lives at the end, so it streams to a scratch file that is hashed on the way
// past and deleted immediately afterwards.
func runCheck(ctx context.Context) error {
	only, filtered := selectedTools()
	// A zip payload spools past a scratch file (see checkZip), so the work
	// directory must exist even on a tree that has never run a generation.
	if err := os.MkdirAll(*flagWork, dirMode); err != nil {
		return err
	}
	lock, err := readLock(*flagOut)
	if err != nil {
		return err
	}
	if err := lock.validate(); err != nil {
		return fmt.Errorf("lock is invalid before any download: %w", err)
	}
	var checked, bytesRead int64
	for _, name := range sortedToolNames(lock) {
		if filtered && !only[name] {
			continue
		}
		e := lock.Tools[name]
		for _, plat := range platformOrder {
			p, ok := e.Platforms[plat]
			if !ok {
				continue
			}
			start := time.Now()
			if err := checkPayload(ctx, e, p); err != nil {
				return fmt.Errorf("%s/%s: %w", name, plat, err)
			}
			checked++
			bytesRead += p.Size
			logf("ok %s/%s %s sha256=%s size=%d entry=%s in %s",
				name, plat, filepath.Base(p.URL), p.SHA256, p.Size, p.Entry, time.Since(start).Round(time.Second))
		}
	}
	logf("verified %d payloads, %d bytes", checked, bytesRead)
	return nil
}

func checkPayload(ctx context.Context, e Entry, p Payload) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p.URL, resp.Status)
	}

	payloadHash := sha256.New()
	counter := &countingWriter{}
	body := io.TeeReader(io.LimitReader(resp.Body, maxDownloadBytes+1), io.MultiWriter(payloadHash, counter))

	var entryDigest string
	magic, err := peek(body, 4)
	if err != nil {
		return err
	}
	stream := io.MultiReader(bytes.NewReader(magic), body)
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		entryDigest, err = checkGzip(stream, p)
	case string(magic) == "PK\x03\x04":
		entryDigest, err = checkZip(stream, p)
	default:
		// A payload that is one uncompressed file: the payload and its entry
		// are the same bytes.
		if _, err := io.Copy(io.Discard, stream); err != nil {
			return err
		}
		entryDigest = hex.EncodeToString(payloadHash.Sum(nil))
	}
	if err != nil {
		return err
	}
	// Drain whatever follows so the payload digest covers every published byte,
	// not only the part the archive reader consumed.
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}

	if got := counter.n; got != p.Size {
		return fmt.Errorf("size %d, lock says %d", got, p.Size)
	}
	if got := hex.EncodeToString(payloadHash.Sum(nil)); got != p.SHA256 {
		return fmt.Errorf("payload sha256 %s, lock says %s", got, p.SHA256)
	}
	if entryDigest == "" {
		return fmt.Errorf("entry %q absent from the payload", p.Entry)
	}
	if entryDigest != p.EntrySHA256 {
		return fmt.Errorf("entry sha256 %s, lock says %s", entryDigest, p.EntrySHA256)
	}
	if e.EntrySHA256 != "" && e.EntrySHA256 != p.EntrySHA256 {
		return fmt.Errorf("tool-level entry_sha256 %s disagrees with this platform's %s", e.EntrySHA256, p.EntrySHA256)
	}
	return nil
}

// checkGzip hashes the pinned entry of a gzip payload. A tar.gz is walked entry
// by entry; a bare gzip member is one file, so the decompressed stream is the
// entry. The two are told apart by the tar magic at offset 257 of the first
// block rather than by the URL, which is the same rule the runtime extractor
// applies to the bytes it actually received.
func checkGzip(r io.Reader, p Payload) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	head, err := peek(gz, 512)
	if err != nil {
		return "", err
	}
	stream := io.MultiReader(bytes.NewReader(head), gz)
	h := sha256.New()
	if len(head) < 512 || string(head[257:262]) != "ustar" {
		if _, err := io.Copy(h, stream); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	found := false
	tr := tar.NewReader(stream)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Name == p.Entry {
			if _, err := io.Copy(h, tr); err != nil {
				return "", err
			}
			found = true
			continue
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return "", err
		}
	}
	if !found {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkZip spools the payload to a scratch file because a zip's index is at the
// end of the archive. The bytes are already being hashed by the caller's tee as
// they stream past; nothing is buffered in memory and the file is removed as
// soon as the entry has been read.
func checkZip(r io.Reader, p Payload) (string, error) {
	tmp, err := os.CreateTemp(*flagWork, "check-*.zip")
	if err != nil {
		return "", err
	}
	defer func() {
		tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name != p.Entry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	return "", nil
}

// peek reads up to n bytes without consuming them from the caller's view; the
// caller re-prepends what came back.
func peek(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return buf[:got], nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:got], nil
}
