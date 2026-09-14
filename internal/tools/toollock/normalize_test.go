package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNormalizer protects the two properties the published lock rests on.
//
// Determinism: the payload digest in the lock is the only thing that makes a
// downloaded archive trustworthy. If packing the same tree twice produced
// different bytes, a regenerated release would silently invalidate every lock a
// shipped binary carries and no payload could be reproduced from source.
//
// Confinement: a payload entry that escapes the payload root would let an
// extracted tool overwrite files outside its store directory. It must be
// refused while packing, before any such archive can be published.
func TestNormalizer(t *testing.T) {
	t.Run("identical trees pack to identical bytes", func(t *testing.T) {
		cases := []struct {
			name  string
			build func(t *testing.T, root string)
		}{
			{
				name: "files, nested dirs and an executable",
				build: func(t *testing.T, root string) {
					mkdir(t, filepath.Join(root, "bin"))
					mkdir(t, filepath.Join(root, "lib", "inner"))
					write(t, filepath.Join(root, "bin", "tool"), "#!/bin/sh\nexec true\n", 0o755)
					write(t, filepath.Join(root, "lib", "a.jar"), "jar-bytes", 0o644)
					write(t, filepath.Join(root, "lib", "inner", "b.txt"), "b", 0o600)
					write(t, filepath.Join(root, "LICENSE"), "license", 0o644)
				},
			},
			{
				name: "a symlink inside the payload",
				build: func(t *testing.T, root string) {
					mkdir(t, filepath.Join(root, "bin"))
					write(t, filepath.Join(root, "bin", "real"), "x", 0o755)
					symlink(t, "real", filepath.Join(root, "bin", "alias"))
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				first := t.TempDir()
				second := t.TempDir()
				tc.build(t, first)
				tc.build(t, second)
				// Different mtimes and different creation order must not reach
				// the archive.
				touchAll(t, second, time.Now().Add(-72*time.Hour))

				a := pack(t, first)
				b := pack(t, second)
				if !bytes.Equal(a, b) {
					t.Fatalf("identical trees produced different archives: %d vs %d bytes", len(a), len(b))
				}
			})
		}
	})

	t.Run("an entry escaping the payload root is rejected", func(t *testing.T) {
		cases := []struct {
			name  string
			build func(t *testing.T, root string)
		}{
			{
				name: "symlink to a parent directory",
				build: func(t *testing.T, root string) {
					symlink(t, "../outside", filepath.Join(root, "escape"))
				},
			},
			{
				name: "symlink climbing out of a subdirectory",
				build: func(t *testing.T, root string) {
					mkdir(t, filepath.Join(root, "bin"))
					symlink(t, "../../etc/passwd", filepath.Join(root, "bin", "escape"))
				},
			},
			{
				name: "absolute symlink target",
				build: func(t *testing.T, root string) {
					symlink(t, "/etc/passwd", filepath.Join(root, "escape"))
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				root := t.TempDir()
				tc.build(t, root)
				out := filepath.Join(t.TempDir(), "payload.tar.gz")
				_, _, err := packDeterministic(root, out)
				if err == nil {
					t.Fatal("packed an archive whose entry escapes the payload root")
				}
				if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "absolute") {
					t.Fatalf("unexpected rejection reason: %v", err)
				}
			})
		}
	})
}

func pack(t *testing.T, root string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "payload.tar.gz")
	digest, size, err := packDeterministic(root, out)
	if err != nil {
		t.Fatalf("packDeterministic: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(b)) != size {
		t.Fatalf("reported size %d, file is %d bytes", size, len(b))
	}
	if digest == "" {
		t.Fatal("no digest reported")
	}
	return b
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, p, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, p string) {
	t.Helper()
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
}

func touchAll(t *testing.T, root string, when time.Time) {
	t.Helper()
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, when, when)
	})
	if err != nil {
		t.Fatal(err)
	}
}
