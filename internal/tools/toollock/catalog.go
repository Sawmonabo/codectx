package main

import (
	"fmt"
	"strings"
)

// platformOrder is the Section 24 target list, in the order every emitted map
// and every log line uses.
var platformOrder = []string{
	"linux_amd64", "linux_arm64",
	"darwin_amd64", "darwin_arm64",
	"windows_amd64", "windows_arm64",
}

// source is one upstream distribution for one platform. Under the hybrid
// hosting rule (see docs/toolchain.md) a platform that has a source is NOT
// re-hosted: the lock points at this URL and pins this file's own SHA-256, so
// Strip is always 0 and every entry path below is relative to the upstream
// archive exactly as the runtime extractor will lay it out.
type source struct {
	URL    string
	Kind   archiveKind
	Strip  int
	Dest   string // payload-relative name for the single-file kinds
	Digest digestSpec
}

// build produces a payload the upstream does not publish as a binary.
type build struct {
	Kind string // "go" or "npm"
	Pkg  string // go: package@version
	NPM  string // npm: shared build id, see npmBuilds
}

// platformPayload is one platform's recipe plus the payload-relative entry the
// launcher executes there. A platform with Src is pinned upstream; a platform
// with Build is produced here and hosted on the tools release, because upstream
// publishes no binary for it at all.
type platformPayload struct {
	Entry string // may contain one "*" glob segment, resolved after extraction
	Src   *source
	Build *build
}

// toolSpec is one lock entry before it is realized.
type toolSpec struct {
	Name      string
	Version   string
	Kind      string // indexer | server | cpg | runtime
	License   string
	Upstream  string
	Runtime   string // "", "node", "jdk"
	Entry     string // tool-level entry: the path that holds on the majority of platforms
	Languages []string
	Notes     string
	Platforms map[string]platformPayload
}

// npmBuild is a build-time `npm ci --omit=dev` of an exactly pinned package
// set. The result is platform-independent JavaScript, so one build is packed
// once per platform key.
type npmBuild struct {
	ID      string
	Deps    map[string]string
	Entries []string // payload-relative files that must exist afterwards
}

const (
	nodeVersion   = "22.23.2"
	jdkVersion    = "21.0.12.1+1"
	jdkTag        = "jdk-21.0.12.1%2B1"
	raVersion     = "2026-08-17.4"
	clangdVersion = "22.1.6"
	jdtlsVersion  = "1.61.0"
	jdtlsStamp    = "202609031315"
	joernVersion  = "4.0.627"
	goplsVersion  = "v0.23.0"
	scipGoVersion = "v0.2.7"
	// scipGoPkg is the module's own declared path. The repository moved under a
	// new owner and the release assets still live at the old one, so the URL a
	// payload is pinned at and the package a payload is built from spell the
	// project differently; both are scip-go v0.2.7.
	scipGoPkg = "github.com/scip-code/scip-go/cmd/scip-go@" + scipGoVersion
	tsVersion = "5.9.3"
)

var npmBuilds = map[string]npmBuild{
	"scip-typescript": {
		ID:      "scip-typescript",
		Deps:    map[string]string{"@sourcegraph/scip-typescript": "0.4.0"},
		Entries: []string{"node_modules/@sourcegraph/scip-typescript/dist/src/main.js"},
	},
	"scip-python": {
		ID:      "scip-python",
		Deps:    map[string]string{"@sourcegraph/scip-python": "0.6.6"},
		Entries: []string{"node_modules/@sourcegraph/scip-python/index.js"},
	},
	"typescript-language-server": {
		ID: "typescript-language-server",
		Deps: map[string]string{
			"typescript-language-server": "6.0.0",
			"typescript":                 tsVersion,
		},
		Entries: []string{
			"node_modules/typescript-language-server/lib/cli.mjs",
			"node_modules/typescript/lib/tsserver.js",
		},
	},
	"pyright": {
		ID:      "pyright",
		Deps:    map[string]string{"pyright": "1.1.414"},
		Entries: []string{"node_modules/pyright/langserver.index.js"},
	},
}

func nodeSource(file string) *source {
	kind := archiveTarGz
	if len(file) > 4 && file[len(file)-4:] == ".zip" {
		kind = archiveZip
	}
	return &source{
		URL:    "https://nodejs.org/dist/v" + nodeVersion + "/" + file,
		Kind:   kind,
		Strip:  0,
		Digest: digestSpec{Algo: "sha256", URL: "https://nodejs.org/dist/v" + nodeVersion + "/SHASUMS256.txt", Match: file},
	}
}

// jdkSource pins a Temurin build at the adoptium/temurin21-binaries release
// asset rather than at the api.adoptium.net redirector: the release URL is
// stable, and each asset carries a published `.sha256.txt` sidecar.
func jdkSource(file string) *source {
	kind := archiveTarGz
	if len(file) > 4 && file[len(file)-4:] == ".zip" {
		kind = archiveZip
	}
	return &source{
		URL:    "https://github.com/adoptium/temurin21-binaries/releases/download/" + jdkTag + "/" + file,
		Kind:   kind,
		Strip:  0,
		Digest: digestSpec{Algo: "sha256", URL: "https://github.com/adoptium/temurin21-binaries/releases/download/" + jdkTag + "/" + file + ".sha256.txt", Match: file},
	}
}

func ghSource(repo, tag, file string, kind archiveKind, strip int, dest string, digest digestSpec) *source {
	return &source{
		URL:    fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, file),
		Kind:   kind,
		Strip:  strip,
		Dest:   dest,
		Digest: digest,
	}
}

func sidecarSHA256(repo, tag, file string) digestSpec {
	return digestSpec{
		Algo: "sha256",
		URL:  fmt.Sprintf("https://github.com/%s/releases/download/%s/%s.sha256", repo, tag, file),
	}
}

func sidecarSHA512(repo, tag, file string) digestSpec {
	return digestSpec{
		Algo: "sha512",
		URL:  fmt.Sprintf("https://github.com/%s/releases/download/%s/%s.sha512", repo, tag, file),
	}
}

// npmAll returns the same platform-independent npm build under every platform
// key. The six published assets are byte-identical and therefore share one
// SHA-256; that is expected, not a bug.
func npmAll(id, entry string) map[string]platformPayload {
	m := make(map[string]platformPayload, len(platformOrder))
	for _, p := range platformOrder {
		m[p] = platformPayload{Entry: entry, Build: &build{Kind: "npm", NPM: id}}
	}
	return m
}

func sameAll(entry string, src *source) map[string]platformPayload {
	m := make(map[string]platformPayload, len(platformOrder))
	for _, p := range platformOrder {
		m[p] = platformPayload{Entry: entry, Src: src}
	}
	return m
}

func goAll(pkg, unixEntry, winEntry string) map[string]platformPayload {
	m := make(map[string]platformPayload, len(platformOrder))
	for _, p := range platformOrder {
		entry := unixEntry
		if p == "windows_amd64" || p == "windows_arm64" {
			entry = winEntry
		}
		m[p] = platformPayload{Entry: entry, Build: &build{Kind: "go", Pkg: pkg}}
	}
	return m
}

// catalog is the complete Section 11.7 tool table. A platform absent from a
// tool's map is unavailable there and is reported as such by the runtime; it is
// never a failure.
func catalog() []toolSpec {
	joern := func(file string) *source {
		return ghSource("joernio/joern", "v"+joernVersion, file, archiveZip, 0, "",
			sidecarSHA512("joernio/joern", "v"+joernVersion, file))
	}
	ra := func(file string, kind archiveKind) *source {
		return ghSource("rust-lang/rust-analyzer", raVersion, file, kind, 0, strings.TrimSuffix(file, ".gz"), digestSpec{})
	}
	clangd := func(file string) *source {
		return ghSource("clangd/clangd", clangdVersion, file, archiveZip, 0, "", digestSpec{})
	}
	scipGo := func(file string) *source {
		return ghSource("sourcegraph/scip-go", scipGoVersion, file, archiveTarGz, 0, "",
			sidecarSHA256("sourcegraph/scip-go", scipGoVersion, file))
	}

	return []toolSpec{
		{
			Name: "node", Version: nodeVersion, Kind: "runtime",
			License:  "MIT AND ICU AND OpenSSL AND others (Node.js LICENSE)",
			Upstream: "https://nodejs.org/dist/v" + nodeVersion + "/",
			Entry:    "bin/node",
			Notes:    "Node.js 22 LTS (Jod); runtime for the node-hosted indexers and servers.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "*/bin/node", Src: nodeSource("node-v" + nodeVersion + "-linux-x64.tar.gz")},
				"linux_arm64":   {Entry: "*/bin/node", Src: nodeSource("node-v" + nodeVersion + "-linux-arm64.tar.gz")},
				"darwin_amd64":  {Entry: "*/bin/node", Src: nodeSource("node-v" + nodeVersion + "-darwin-x64.tar.gz")},
				"darwin_arm64":  {Entry: "*/bin/node", Src: nodeSource("node-v" + nodeVersion + "-darwin-arm64.tar.gz")},
				"windows_amd64": {Entry: "*/node.exe", Src: nodeSource("node-v" + nodeVersion + "-win-x64.zip")},
				"windows_arm64": {Entry: "*/node.exe", Src: nodeSource("node-v" + nodeVersion + "-win-arm64.zip")},
			},
		},
		{
			Name: "jdk", Version: jdkVersion, Kind: "runtime",
			License:  "GPL-2.0-only WITH Classpath-exception-2.0",
			Upstream: "https://adoptium.net/temurin/releases/?version=21",
			Entry:    "bin/java",
			Notes:    "Eclipse Temurin 21 LTS JDK; runtime for scip-java, jdtls and the dependence engine.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "*/bin/java", Src: jdkSource("OpenJDK21U-jdk_x64_linux_hotspot_21.0.12.1_1.tar.gz")},
				"linux_arm64":   {Entry: "*/bin/java", Src: jdkSource("OpenJDK21U-jdk_aarch64_linux_hotspot_21.0.12.1_1.tar.gz")},
				"darwin_amd64":  {Entry: "*/Contents/Home/bin/java", Src: jdkSource("OpenJDK21U-jdk_x64_mac_hotspot_21.0.12.1_1.tar.gz")},
				"darwin_arm64":  {Entry: "*/Contents/Home/bin/java", Src: jdkSource("OpenJDK21U-jdk_aarch64_mac_hotspot_21.0.12.1_1.tar.gz")},
				"windows_amd64": {Entry: "*/bin/java.exe", Src: jdkSource("OpenJDK21U-jdk_x64_windows_hotspot_21.0.12.1_1.zip")},
				"windows_arm64": {Entry: "*/bin/java.exe", Src: jdkSource("OpenJDK21U-jdk_aarch64_windows_hotspot_21.0.12.1_1.zip")},
			},
		},
		{
			Name: "scip-go", Version: "0.2.7", Kind: "indexer",
			License: "Apache-2.0", Upstream: "https://github.com/sourcegraph/scip-go",
			Entry: "scip-go", Languages: []string{"go"},
			Notes: "Upstream publishes linux amd64/arm64 and darwin arm64; the other three are cross-built from the pinned module at release time and hosted, which is the hybrid rule applied per platform rather than per tool.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "scip-go", Src: scipGo("scip-go-linux-amd64.tar.gz")},
				"linux_arm64":   {Entry: "scip-go", Src: scipGo("scip-go-linux-arm64.tar.gz")},
				"darwin_arm64":  {Entry: "scip-go", Src: scipGo("scip-go-darwin-arm64.tar.gz")},
				"darwin_amd64":  {Entry: "scip-go", Build: &build{Kind: "go", Pkg: scipGoPkg}},
				"windows_amd64": {Entry: "scip-go.exe", Build: &build{Kind: "go", Pkg: scipGoPkg}},
				"windows_arm64": {Entry: "scip-go.exe", Build: &build{Kind: "go", Pkg: scipGoPkg}},
			},
		},
		{
			Name: "scip-typescript", Version: "0.4.0", Kind: "indexer",
			License: "Apache-2.0", Upstream: "https://www.npmjs.com/package/@sourcegraph/scip-typescript",
			Runtime: "node", Entry: "node_modules/@sourcegraph/scip-typescript/dist/src/main.js",
			Languages: []string{"typescript", "tsx", "javascript"},
			Platforms: npmAll("scip-typescript", "node_modules/@sourcegraph/scip-typescript/dist/src/main.js"),
		},
		{
			Name: "scip-python", Version: "0.6.6", Kind: "indexer",
			License: "MIT", Upstream: "https://www.npmjs.com/package/@sourcegraph/scip-python",
			Runtime: "node", Entry: "node_modules/@sourcegraph/scip-python/index.js",
			Languages: []string{"python"},
			Notes:     "MIT, not Apache-2.0 like the other scip-* indexers: the package vendors pyright.",
			Platforms: npmAll("scip-python", "node_modules/@sourcegraph/scip-python/index.js"),
		},
		{
			Name: "scip-java", Version: "0.13.1", Kind: "indexer",
			License: "Apache-2.0", Upstream: "https://github.com/sourcegraph/scip-java",
			Runtime: "jdk", Entry: "scip-java-v0.13.1",
			Languages: []string{"java"},
			Notes:     "One platform-independent launcher jar, published as a single unarchived file; the upstream sh preamble is tolerated by `java -jar`, so Windows is covered under the managed JDK.",
			Platforms: sameAll("scip-java-v0.13.1", ghSource("sourcegraph/scip-java", "v0.13.1", "scip-java-v0.13.1",
				archiveRaw, 0, "scip-java-v0.13.1", sidecarSHA256("sourcegraph/scip-java", "v0.13.1", "scip-java-v0.13.1"))),
		},
		{
			Name: "rust-analyzer", Version: raVersion, Kind: "indexer",
			License: "MIT OR Apache-2.0", Upstream: "https://github.com/rust-lang/rust-analyzer",
			Entry: "rust-analyzer-x86_64-unknown-linux-gnu", Languages: []string{"rust"},
			Notes: "Also the Rust language server. Upstream publishes no digest sidecar for these assets.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "rust-analyzer-x86_64-unknown-linux-gnu", Src: ra("rust-analyzer-x86_64-unknown-linux-gnu.gz", archiveGz)},
				"linux_arm64":   {Entry: "rust-analyzer-aarch64-unknown-linux-gnu", Src: ra("rust-analyzer-aarch64-unknown-linux-gnu.gz", archiveGz)},
				"darwin_amd64":  {Entry: "rust-analyzer-x86_64-apple-darwin", Src: ra("rust-analyzer-x86_64-apple-darwin.gz", archiveGz)},
				"darwin_arm64":  {Entry: "rust-analyzer-aarch64-apple-darwin", Src: ra("rust-analyzer-aarch64-apple-darwin.gz", archiveGz)},
				"windows_amd64": {Entry: "rust-analyzer.exe", Src: ra("rust-analyzer-x86_64-pc-windows-msvc.zip", archiveZip)},
				"windows_arm64": {Entry: "rust-analyzer.exe", Src: ra("rust-analyzer-aarch64-pc-windows-msvc.zip", archiveZip)},
			},
		},
		{
			Name: "scip-clang", Version: "0.4.0", Kind: "indexer",
			License: "Apache-2.0", Upstream: "https://github.com/sourcegraph/scip-clang",
			Entry: "scip-clang-x86_64-linux", Languages: []string{"c", "cpp"},
			Notes: "Upstream publishes linux x86_64 and darwin arm64 only; Windows and the other two use clangd for precise C/C++.",
			Platforms: map[string]platformPayload{
				"linux_amd64":  {Entry: "scip-clang-x86_64-linux", Src: ghSource("sourcegraph/scip-clang", "v0.4.0", "scip-clang-x86_64-linux", archiveRaw, 0, "scip-clang-x86_64-linux", digestSpec{})},
				"darwin_arm64": {Entry: "scip-clang-arm64-darwin", Src: ghSource("sourcegraph/scip-clang", "v0.4.0", "scip-clang-arm64-darwin", archiveRaw, 0, "scip-clang-arm64-darwin", digestSpec{})},
			},
		},
		{
			Name: "gopls", Version: "0.23.0", Kind: "server",
			License: "BSD-3-Clause", Upstream: "https://pkg.go.dev/golang.org/x/tools/gopls",
			Entry: "gopls", Languages: []string{"go"},
			Notes:     "No upstream binaries exist; every platform is cross-built from the pinned module at release time.",
			Platforms: goAll("golang.org/x/tools/gopls@"+goplsVersion, "gopls", "gopls.exe"),
		},
		{
			Name: "typescript-language-server", Version: "6.0.0", Kind: "server",
			License:  "Apache-2.0 (server) AND Apache-2.0 (TypeScript " + tsVersion + ")",
			Upstream: "https://www.npmjs.com/package/typescript-language-server",
			Runtime:  "node", Entry: "node_modules/typescript-language-server/lib/cli.mjs",
			Languages: []string{"typescript", "tsx", "javascript"},
			Notes:     "Pinned with TypeScript " + tsVersion + ": the server drives `tsserver.js`, which the 7.x package no longer ships.",
			Platforms: npmAll("typescript-language-server", "node_modules/typescript-language-server/lib/cli.mjs"),
		},
		{
			Name: "pyright", Version: "1.1.414", Kind: "server",
			License: "MIT", Upstream: "https://www.npmjs.com/package/pyright",
			Runtime: "node", Entry: "node_modules/pyright/langserver.index.js",
			Languages: []string{"python"},
			Platforms: npmAll("pyright", "node_modules/pyright/langserver.index.js"),
		},
		{
			Name: "clangd", Version: clangdVersion, Kind: "server",
			License: "Apache-2.0 WITH LLVM-exception", Upstream: "https://github.com/clangd/clangd",
			Entry: "*/bin/clangd", Languages: []string{"c", "cpp"},
			Notes: "Upstream publishes x86_64 linux, macOS and Windows builds only; no digest sidecar.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "*/bin/clangd", Src: clangd("clangd-linux-" + clangdVersion + ".zip")},
				"darwin_amd64":  {Entry: "*/bin/clangd", Src: clangd("clangd-mac-" + clangdVersion + ".zip")},
				"windows_amd64": {Entry: "*/bin/clangd.exe", Src: clangd("clangd-windows-" + clangdVersion + ".zip")},
			},
		},
		{
			Name: "jdtls", Version: jdtlsVersion, Kind: "server",
			License: "EPL-2.0", Upstream: "https://download.eclipse.org/jdtls/milestones/" + jdtlsVersion + "/",
			Runtime: "jdk", Entry: "plugins/org.eclipse.equinox.launcher_*.jar",
			Languages: []string{"java"},
			Notes:     "One platform-independent tarball carrying config_linux, config_mac and config_win; launched as `java -jar <equinox launcher>`.",
			Platforms: sameAll("plugins/org.eclipse.equinox.launcher_*.jar", &source{
				URL:    "https://download.eclipse.org/jdtls/milestones/" + jdtlsVersion + "/jdt-language-server-" + jdtlsVersion + "-" + jdtlsStamp + ".tar.gz",
				Kind:   archiveTarGz,
				Strip:  0,
				Digest: digestSpec{Algo: "sha256", URL: "https://download.eclipse.org/jdtls/milestones/" + jdtlsVersion + "/jdt-language-server-" + jdtlsVersion + "-" + jdtlsStamp + ".tar.gz.sha256"},
			}),
		},
		{
			Name: "joern", Version: joernVersion, Kind: "cpg",
			License: "Apache-2.0", Upstream: "https://github.com/joernio/joern",
			Runtime: "jdk", Entry: "*/joern-parse",
			Languages: []string{"go", "typescript", "tsx", "javascript", "python", "java", "c", "cpp", "rust"},
			Notes:     "Backend of the `dependence` provider. The sibling `joern-export` lives beside the entry in the same payload root.",
			Platforms: map[string]platformPayload{
				"linux_amd64":   {Entry: "*/joern-parse", Src: joern("joern-cli-linux-x86_64.zip")},
				"linux_arm64":   {Entry: "*/joern-parse", Src: joern("joern-cli-linux-arm64.zip")},
				"darwin_amd64":  {Entry: "*/joern-parse", Src: joern("joern-cli-macos-x86_64.zip")},
				"darwin_arm64":  {Entry: "*/joern-parse", Src: joern("joern-cli-macos-arm64.zip")},
				"windows_amd64": {Entry: "*/joern-parse.bat", Src: joern("joern-cli-windows-x86_64.zip")},
				"windows_arm64": {Entry: "*/joern-parse.bat", Src: joern("joern-cli-windows-arm64.zip")},
			},
		},
	}
}
