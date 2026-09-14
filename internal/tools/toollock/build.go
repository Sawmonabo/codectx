package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// runCmd executes a build-time helper. Build-time is the only place a package
// manager or compiler ever runs: the shipped product executes nothing but the
// pinned payloads.
func runCmd(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, truncate(string(out), 4000))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}

// buildNPM materializes an exactly pinned Node package set with
// `npm ci --omit=dev`. The lockfile is generated first from the pinned direct
// versions, so the transitive set is resolved once at release time and never at
// runtime. Install scripts are disabled: a payload must not carry anything a
// postinstall hook compiled on the build host.
func buildNPM(ctx context.Context, spec npmBuild, dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	pkg := map[string]any{
		"name":         "codectx-" + spec.ID + "-payload",
		"version":      "0.0.0",
		"private":      true,
		"dependencies": spec.Deps,
	}
	b, err := json.MarshalIndent(pkg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), append(b, '\n'), fileMode); err != nil {
		return err
	}
	if err := runCmd(ctx, dir, nil, "npm", "install", "--package-lock-only", "--ignore-scripts", "--omit=optional", "--no-audit", "--no-fund"); err != nil {
		return err
	}
	if err := runCmd(ctx, dir, nil, "npm", "ci", "--omit=dev", "--omit=optional", "--ignore-scripts", "--no-audit", "--no-fund"); err != nil {
		return err
	}
	for _, e := range spec.Entries {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(e))); err != nil {
			return fmt.Errorf("npm build %s: expected %s: %w", spec.ID, e, err)
		}
	}
	return nil
}

// buildGo cross-builds a pinned Go program for one target. `go install
// pkg@version` is module-graph independent, so the product's own go.mod is
// never touched.
func buildGo(ctx context.Context, pkg, platform, workRoot, outDir, outName string) error {
	os_, arch, err := splitPlatform(platform)
	if err != nil {
		return err
	}
	gopath := filepath.Join(workRoot, "gopath")
	env := []string{
		"GOOS=" + os_,
		"GOARCH=" + arch,
		"CGO_ENABLED=0",
		"GOFLAGS=-trimpath",
		"GOPATH=" + gopath,
		"GOBIN=",
	}
	// Keep the read-only module cache where the host already has it; a copy
	// under the scratch GOPATH would be several gigabytes and is not removable
	// without first clearing its read-only bits.
	if cache := goEnv("GOMODCACHE"); cache != "" {
		env = append(env, "GOMODCACHE="+cache)
	}
	if err := runCmd(ctx, workRoot, env, "go", "install", pkg); err != nil {
		return err
	}
	base := pkgBase(pkg)
	if os_ == "windows" {
		base += ".exe"
	}
	candidates := []string{
		filepath.Join(gopath, "bin", os_+"_"+arch, base),
		filepath.Join(gopath, "bin", base),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			if err := os.MkdirAll(outDir, dirMode); err != nil {
				return err
			}
			return copyInto(c, filepath.Join(outDir, filepath.FromSlash(outName)), execMode)
		}
	}
	return fmt.Errorf("go install %s: no binary at %v", pkg, candidates)
}

func pkgBase(pkg string) string {
	p := pkg
	if i := strings.LastIndex(p, "@"); i > 0 {
		p = p[:i]
	}
	return path.Base(p)
}

func splitPlatform(p string) (string, string, error) {
	parts := strings.SplitN(p, "_", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("bad platform key %q", p)
	}
	return parts[0], parts[1], nil
}

// goEnv reads one value from the host `go env`, so a scratch GOPATH does not
// force a second copy of the module cache.
func goEnv(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
