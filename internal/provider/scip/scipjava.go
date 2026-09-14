package scip

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// scipJavaConfigName is the build description the Java profile hands the
// indexer. It is a generated input written into the private materialization
// for the same reason normalizeCompileCommands rewrites the compilation
// database there: it must name absolute paths inside a directory that did not
// exist when the snapshot was taken, so the snapshot cannot carry it.
const scipJavaConfigName = "scip-java.json"

// scipJavaTargetRootName is the indexer's own scratch root, kept inside the
// run directory. Left to its default, scip-java writes its javac option file
// and its per-file SCIP output into `<sourceroot>/target`, i.e. into the
// materialization; --targetroot moves all of it into the run's private scratch
// so the copy the indexer reads is the copy the input manifest describes.
const scipJavaTargetRootName = "scip-java-targetroot"

// scipJavaConfig is the `scip-java.json` schema this profile emits. Only the
// fields the profile sets exist here: a struct that cannot express a field is
// a field no run can acquire by accident.
type scipJavaConfig struct {
	Projects []scipJavaProject `json:"projects"`
}

type scipJavaProject struct {
	SourceRoot        string   `json:"sourceroot"`
	SourceDirectories []string `json:"sourceDirectories"`
	Dependencies      []string `json:"dependencies"`
	Classpath         []string `json:"classpath"`
	JavacOptions      []string `json:"javacOptions"`
}

// requireJavaSources refuses a Java run whose materialization holds no `.java`
// file at all, before the indexer is started.
//
// A root pom.xml with no compilable source is not exotic: it is every
// aggregator POM of a multi-module repository, and a root manifest is exactly
// what triggers this profile. javac refuses, scip-java exits 1, and the run
// fails as `CTX_PROVIDER_UNAVAILABLE: scip-java-v0.13.1 exited with status 1`
// (measured) -- a process failure with no stderr and no remediation, for a
// condition this provider can name precisely and the other five profiles
// already do name: the Go path reports the same empty index as
// CTX_PROVIDER_OUTPUT_INVALID. The refusal is the same typed one, raised
// before a JVM is started rather than after.
//
// The walk stops at the first match and is bounded by the materialization,
// which MaxMaterializeBytes already bounds.
func requireJavaSources(matRoot string) error {
	found := false
	err := filepath.WalkDir(matRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".java") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return internal("scip java source scan: " + err.Error())
	}
	if found {
		return nil
	}
	return &model.Error{Code: model.CodeProviderOutputInvalid,
		Message:     "the snapshot declares a Java project but holds no .java source for the indexer to describe",
		Remediation: "an aggregator pom.xml whose modules hold the sources needs no index of its own; if sources were expected here, restore them and re-run"}
}

// writeScipJavaConfig writes the Java profile's build description into the
// private materialization, replacing one the repository happened to carry.
//
// Why this exists. Without it scip-java drives the project's own build tool,
// which it looks for on PATH: a machine without Maven or Gradle gets
// `CTX_PROVIDER_UNAVAILABLE: scip-java exited with status 1` from a product
// that owns a JDK and pins an indexer (measured). With a configuration file
// the indexer compiles the sources itself with the managed JDK's own javac, so
// the Java profile needs no host build tool at all and resolves nothing over
// the network.
//
// The description is deliberately minimal: the materialization root is both
// the source root and the only source directory, so every `.java` file of the
// pinned snapshot is compiled wherever it sits, and `dependencies` and
// `classpath` stay empty, which is the same repository-local navigation the
// Python profile already publishes. A project whose types come from external
// jars still indexes its own sources; the symbols it cannot resolve are absent
// rather than wrong, and the false-readiness gate refuses an index that
// described nothing at all.
//
// A repository's own scip-java.json is not honoured: the argument array, the
// environment and the build description are product code (Section 20.2), and a
// file inside the repository choosing what the indexer compiles is a
// configuration surface this provider does not offer.
func writeScipJavaConfig(matRoot string) error {
	cfg := scipJavaConfig{Projects: []scipJavaProject{{
		SourceRoot:        matRoot,
		SourceDirectories: []string{matRoot},
		Dependencies:      []string{},
		Classpath:         []string{},
		JavacOptions:      []string{},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		return internal("scip java configuration: " + err.Error())
	}
	if err := os.WriteFile(filepath.Join(matRoot, scipJavaConfigName), data, 0o600); err != nil {
		return &model.Error{Code: model.CodeInternal,
			Message: "the private scip-java configuration could not be written: " + err.Error()}
	}
	return nil
}
