package scip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/snapshot"
)

// profileKind is one approved indexer this provider knows how to run: the
// tool name its output must declare and the manifests whose presence makes a
// workspace a candidate. The profile's executable, argv, environment and
// budgets come from the user's `[analyzers.<name>]` table (Section 20.2); the
// name of that table selects the kind.
type profileKind struct {
	tool     string
	triggers []string
}

// profileKinds are the approved indexer profiles of Section 11.4. None has
// been verified against a real installed tool on the development machine
// (ruling R9-2); docs/providers-scip.md says so.
var profileKinds = map[string]profileKind{
	"scip-go":         {tool: "scip-go", triggers: []string{"go.mod"}},
	"scip-typescript": {tool: "scip-typescript", triggers: []string{"package.json", "tsconfig.json"}},
	"scip-java":       {tool: "scip-java", triggers: []string{"pom.xml", "build.gradle", "build.gradle.kts"}},
}

// Profile is an approved analyzer resolved to a known indexer kind.
type Profile struct {
	config.Analyzer
	kind profileKind
}

// profiles keeps the approved analyzers whose name is a known SCIP indexer
// kind, sorted by name. Analyzers with other names belong to other providers
// and are ignored here.
func profiles(all []config.Analyzer) []Profile {
	var out []Profile
	for _, a := range all {
		if kind, ok := profileKinds[a.Name]; ok {
			out = append(out, Profile{Analyzer: a, kind: kind})
		}
	}
	return out
}

// run executes one approved profile against a private materialization of the
// snapshot and returns the path of the validated index it produced. The
// caller owns runDir and removes it on every path; the index is inside it.
//
// The tool sees exactly: the materialized snapshot files as its working
// directory, the typed argv substitutions, the allowlisted environment
// variables and nothing else. There is no shell (ruling R9-3). The captured
// input-hash manifest of the materialized files is written beside the output
// as the record of what the tool analyzed.
func (p *Provider) runProfile(ctx context.Context, prof Profile, view model.SnapshotView, runDir string) (string, error) {
	if p.runner == nil {
		return "", &model.Error{Code: model.CodeProviderUnavailable, Message: "scip profiles need the shared process runner"}
	}
	if err := checkExecutable(prof.Analyzer); err != nil {
		return "", err
	}
	mat, err := snapshot.Materialize(ctx, view, model.FileSelection{}, snapshot.MaterializeOptions{Dir: filepath.Join(runDir, "src"), MaxBytes: p.limits.MaxMaterializeBytes})
	if err != nil {
		return "", err
	}
	defer mat.Close()
	manifestPath := filepath.Join(runDir, "inputs.manifest")
	if err := writeManifest(ctx, view, manifestPath); err != nil {
		return "", err
	}
	output := filepath.Join(runDir, "index.scip")
	subs := map[string]string{"input_dir": mat.Root(), "output_file": output, "work_dir": runDir, "manifest": manifestPath}
	args := make([]string, 0, len(prof.Args))
	for _, a := range prof.Args {
		for name, value := range subs {
			a = strings.ReplaceAll(a, "${"+name+"}", value)
		}
		args = append(args, a)
	}
	var env []string
	for _, name := range prof.EnvAllowlist {
		if value, ok := p.lookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	timeout := prof.Timeout.Std()
	if p.timeout > 0 && p.timeout < timeout {
		timeout = p.timeout
	}
	_, err = p.runner.Run(ctx, process.Spec{
		Path: prof.Executable, Args: args, Dir: mat.Root(), Env: env,
		MaxStdoutBytes: maxToolOutputBytes, MaxStderrBytes: maxToolOutputBytes,
		Timeout: timeout, Grace: toolGrace,
		MemoryReservationBytes: prof.MemoryBudgetBytes, DiskReservationBytes: prof.DiskBudgetBytes,
	})
	if err != nil {
		return "", err
	}
	// The output must be a regular file the tool wrote inside the run
	// directory, within the index bound. A symlink is refused: the decoder
	// would otherwise read whatever it points at as the tool's output.
	info, err := os.Lstat(output)
	if err != nil {
		return "", &model.Error{Code: model.CodeProviderOutputInvalid, Message: prof.kind.tool + " produced no index at its output path"}
	}
	if !info.Mode().IsRegular() {
		return "", &model.Error{Code: model.CodeProviderOutputInvalid, Message: prof.kind.tool + " output is not a regular file"}
	}
	if info.Size() > p.limits.MaxIndexBytes {
		return "", overLimit("index bytes", info.Size(), p.limits.MaxIndexBytes)
	}
	return output, nil
}

const (
	// maxToolOutputBytes bounds each captured stream of an indexer. The
	// streams are diagnostics only and never logged raw (Section 22).
	maxToolOutputBytes = 1 << 20
	toolGrace          = 10 * time.Second
)

// checkExecutable verifies the profile's executable against its recorded
// checksum when one is configured. A repository cannot substitute the tool a
// user approved without the digest changing.
func checkExecutable(a config.Analyzer) error {
	if a.Checksum == "" {
		return nil
	}
	f, err := os.Open(a.Executable)
	if err != nil {
		return trustRequired(fmt.Sprintf("%s cannot be read for its checksum: %v", a.Executable, err))
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return trustRequired(fmt.Sprintf("%s cannot be read for its checksum: %v", a.Executable, err))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.Checksum {
		return trustRequired(a.Executable + " does not match the checksum recorded for profile " + a.Name)
	}
	return nil
}

// writeManifest records the content hash of every materialized file, one
// `<sha256>  <path>` line per file in manifest order, streaming from the
// snapshot view so no file list is held.
func writeManifest(ctx context.Context, view model.SnapshotView, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return internal("scip manifest: " + err.Error())
	}
	err = view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		_, err := fmt.Fprintf(f, "%s  %s\n", fv.ContentHash, fv.Path)
		return err
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return internal("scip manifest: " + err.Error())
	}
	return nil
}

// checkTool verifies that the produced index names the profile's tool and a
// version satisfying the constraint. Output from another tool or version is
// not what the user approved.
func checkTool(prof Profile, m metadata) error {
	if m.toolName != prof.kind.tool {
		return trustRequired(fmt.Sprintf("index was produced by %q, profile %s approves %q", m.toolName, prof.Name, prof.kind.tool))
	}
	ok, err := satisfies(m.toolVersion, prof.VersionConstraint)
	if err != nil {
		return err
	}
	if !ok {
		return trustRequired(fmt.Sprintf("%s version %q does not satisfy the approved constraint %q", prof.kind.tool, m.toolVersion, prof.VersionConstraint))
	}
	return nil
}

// satisfies evaluates a version constraint: an exact version ("0.1.24",
// "v0.1.24") or space-separated comparators (">=0.1.0 <0.2.0"). Versions
// compare by their dotted numeric components; a leading "v" and any
// pre-release or build suffix are ignored.
func satisfies(version, constraint string) (bool, error) {
	got, ok := parseVersion(version)
	if !ok {
		return false, trustRequired(fmt.Sprintf("tool version %q is not a dotted version", version))
	}
	for _, term := range strings.Fields(constraint) {
		op := "="
		for _, candidate := range []string{">=", "<=", ">", "<", "="} {
			if strings.HasPrefix(term, candidate) {
				op, term = candidate, term[len(candidate):]
				break
			}
		}
		want, ok := parseVersion(term)
		if !ok {
			return false, &model.Error{Code: model.CodeConfigInvalid, Message: "version constraint " + strconv.Quote(constraint) + " is not understood"}
		}
		c := compareVersions(got, want)
		switch op {
		case "=":
			ok = c == 0
		case ">=":
			ok = c >= 0
		case "<=":
			ok = c <= 0
		case ">":
			ok = c > 0
		case "<":
			ok = c < 0
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func parseVersion(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func compareVersions(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// executableInstalled reports whether the profile's executable exists as a
// regular executable file. Detection never runs it.
func executableInstalled(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

func trustRequired(msg string) *model.Error {
	return (&model.Error{Code: model.CodeTrustRequired, Message: msg}).
		WithRemediation("Approve the tool with an absolute path, checksum and version constraint in the user configuration.")
}
