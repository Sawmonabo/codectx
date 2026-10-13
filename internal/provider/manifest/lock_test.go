package manifest_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// lockLines renders every `depends_on` occurrence a lock file published,
// as "from -> to key=value ...", the detail keys sorted and "ranged" when
// the occurrence has a range: the lock's configuration node to each pinned
// package, and each pinned package to what it depends on.
func lockLines(t *testing.T, cap *capture) []string {
	t.Helper()
	var out []string
	for _, r := range cap.relations {
		from, to := cap.nodes[r.Relation.From], cap.nodes[r.Relation.To]
		if r.Relation.Kind != model.RelDependsOn || from.Kind != model.NodeConfiguration && from.Kind != model.NodeDependency {
			continue
		}
		for _, ev := range r.Evidence {
			line := from.QualifiedName + " -> " + to.QualifiedName
			var detail map[string]string
			if err := json.Unmarshal([]byte(ev.Detail), &detail); err != nil {
				t.Fatalf("evidence detail %q: %v", ev.Detail, err)
			}
			for _, k := range sortedMapKeys(detail) {
				line += " " + k + "=" + detail[k]
			}
			if ev.Range != nil {
				line += " ranged"
			}
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// runLock runs the manifest provider over each listed file of files, in
// order, into one capture.
func runLock(t *testing.T, files map[string]string, paths ...string) (*capture, map[string]model.CapabilityState) {
	t.Helper()
	mf, err := manifest.New(manifest.Options{MaxParseFileBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	cap := &capture{}
	states := map[string]model.CapabilityState{}
	h := providertest.New(t, files)
	for _, p := range paths {
		runUnit(t, h, mf, p, cap, states)
	}
	for _, p := range paths {
		if cs := states[manifest.ID+" "+manifest.CapabilityManifests+" "+filesystem.ScopeKey(p)]; cs.State != model.CapabilityFresh {
			t.Fatalf("%s published %s/%s, want fresh", p, cs.State, cs.DiagnosticCode)
		}
	}
	return cap, states
}

func wantLines(t *testing.T, name string, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("%s facts differ:\n--- got\n%s\n--- want\n%s", name, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLockFilesPublishResolvedVersionsAndTheirGraph runs each lock format
// over a real-shaped excerpt and pins every fact it publishes. Mutations
// caught, per case:
//   - package-lock v3: resolving a dependency to the top-level copy instead
//     of Node's nearest node_modules (a -> b would be 2.1.0, not 1.5.0), and
//     keeping one version per name (b's two pins would collapse, set_size 1);
//   - package-lock v1: reading the nested tree without its path (the nested
//     b would resolve as the top-level one);
//   - Cargo.lock: ignoring the "name version" form (app -> rand, which the
//     lock pins twice, would lose its resolved_version);
//   - yarn.lock v1: splitting a scoped descriptor at its first `@` (the name
//     would be empty) or not joining a dependency to the block listing its
//     descriptor (no resolved_version);
//   - Pipfile.lock: keeping the `==` (resolved_version ==2.31.0) or dropping
//     the section (no group);
//   - poetry.lock: taking a wheel's hash for the package's integrity, and
//     dropping a table-valued dependency's markers;
//   - pdm.lock: publishing a dependency under its name as written
//     (Typing_Extensions) instead of its normalized one;
//   - yarn.lock (YAML format): reading a workspace as a registry package (no
//     local), or matching a dependency only by its range as written;
//   - pnpm-lock.yaml v9: reading dependencies from `packages` alone (the
//     `snapshots` graph is lost) or keeping the peer suffix in the key (the
//     react-dom snapshot would be a second package);
//   - pnpm-lock.yaml v5: splitting a `/name/version` key at its `@` (the
//     scoped name would be cut) or keeping the `_peer` suffix;
//   - bun.lock: failing on its trailing commas, resolving a nested copy
//     as the top-level one (other -> js-tokens would be 4.0.0), or skipping
//     the `workspaces` object past its own end (the skip would consume
//     `packages` with it: no facts at all);
//   - uv.lock: dropping optional-dependencies or the marker, or counting
//     the wheels' hashes as the integrity;
//   - pylock.toml: losing the `[[packages.wheels]]` sub-tables to the next
//     segment (attrs's hash count would be 0);
//   - gradle.lockfile: reading `empty=` as a module, or dropping the
//     configurations;
//   - npm-shrinkwrap.json: not reading the file as a package lock (no facts),
//     or naming an aliased package by its node_modules folder (the lock and
//     app would reach npm:js-yaml, not npm:@zkochan/js-yaml);
//   - package-lock v1 alias: keeping `npm:@zkochan/js-yaml@0.0.6` as the
//     version and js-yaml as the name;
//   - bun.lock alias: naming the package by its install path instead of the
//     tuple's `name@version`;
//   - yarn.lock alias: naming the block by the alias of its descriptor, or
//     resolving a dependency to the alias name;
//   - pnpm-lock.yaml v9 and v6 alias: reading a dependency path value as a
//     version (other -> js-yaml would publish resolved_version
//     @zkochan/js-yaml@0.0.6 on npm:js-yaml), or reading tslib's plain
//     version as a path;
//   - Cargo.lock v1: not reading `[metadata]` (rand loses its integrity), or
//     taking `<none>` as a checksum (rand_core would gain one);
//   - poetry.lock 1.1: not reading `[metadata.files]` (no hashes), or giving
//     each pinned version every file of its name (six 1.16.0 would count 3
//     hashes and, with two source distributions, no integrity);
//   - poetry.lock 1.0: not reading `[metadata.hashes]` (no hashes);
//   - Pipfile.lock, pdm.lock and pylock.<name>.toml VCS: reading git alone
//     (mylib would have no source), or dropping the VCS from the source;
//   - buildscript-gradle.lockfile: not reading the file as a Gradle lock
//     (no facts).
func TestLockFilesPublishResolvedVersionsAndTheirGraph(t *testing.T) {
	const npmA = "https://registry.npmjs.org/a/-/a-1.2.0.tgz"
	const npmB1 = "https://registry.npmjs.org/b/-/b-1.5.0.tgz"
	const npmB2 = "https://registry.npmjs.org/b/-/b-2.1.0.tgz"
	const crates = "registry+https://github.com/rust-lang/crates.io-index"
	cases := []struct {
		name, path, body string
		want             []string
	}{
		{
			name: "package-lock v3",
			path: "web/package-lock.json",
			body: `{
  "name": "app",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "requires": true,
  "packages": {
    "": {
      "name": "app",
      "version": "1.0.0",
      "dependencies": {
        "a": "^1.0.0",
        "b": "^2.0.0"
      }
    },
    "node_modules/a": {
      "version": "1.2.0",
      "resolved": "` + npmA + `",
      "integrity": "sha512-AAA",
      "dependencies": {
        "b": "^1.0.0"
      }
    },
    "node_modules/a/node_modules/b": {
      "version": "1.5.0",
      "resolved": "` + npmB1 + `",
      "integrity": "sha512-BBB"
    },
    "node_modules/b": {
      "version": "2.1.0",
      "resolved": "` + npmB2 + `",
      "integrity": "sha512-CCC"
    }
  }
}
`,
			want: []string{
				"npm:lock:web/package-lock.json -> npm:app kind=locked local=true resolved_version=1.0.0 set_size=1 ranged",
				"npm:lock:web/package-lock.json -> npm:a integrity=sha512-AAA kind=locked resolved_version=1.2.0 set_size=1 source=" + npmA + " ranged",
				"npm:lock:web/package-lock.json -> npm:b integrity=sha512-BBB kind=locked resolved_version=1.5.0 set_size=2 source=" + npmB1 + " ranged",
				"npm:lock:web/package-lock.json -> npm:b integrity=sha512-CCC kind=locked resolved_version=2.1.0 set_size=2 source=" + npmB2 + " ranged",
				"npm:app -> npm:a kind=runtime requirement=^1.0.0 resolved_version=1.2.0 version=1.0.0 ranged",
				"npm:app -> npm:b kind=runtime requirement=^2.0.0 resolved_version=2.1.0 version=1.0.0 ranged",
				"npm:a -> npm:b kind=runtime requirement=^1.0.0 resolved_version=1.5.0 version=1.2.0 ranged",
			},
		},
		{
			name: "package-lock v1",
			path: "package-lock.json",
			body: `{
  "name": "app",
  "version": "1.0.0",
  "lockfileVersion": 1,
  "requires": true,
  "dependencies": {
    "a": {
      "version": "1.2.0",
      "resolved": "` + npmA + `",
      "integrity": "sha512-AAA",
      "requires": {
        "b": "^1.0.0"
      },
      "dependencies": {
        "b": {
          "version": "1.5.0",
          "resolved": "` + npmB1 + `",
          "integrity": "sha512-BBB"
        }
      }
    },
    "b": {
      "version": "2.1.0",
      "resolved": "` + npmB2 + `",
      "integrity": "sha512-CCC"
    }
  }
}
`,
			want: []string{
				"npm:lock:package-lock.json -> npm:a integrity=sha512-AAA kind=locked resolved_version=1.2.0 set_size=1 source=" + npmA + " ranged",
				"npm:lock:package-lock.json -> npm:b integrity=sha512-BBB kind=locked resolved_version=1.5.0 set_size=2 source=" + npmB1 + " ranged",
				"npm:lock:package-lock.json -> npm:b integrity=sha512-CCC kind=locked resolved_version=2.1.0 set_size=2 source=" + npmB2 + " ranged",
				"npm:a -> npm:b kind=runtime requirement=^1.0.0 resolved_version=1.5.0 version=1.2.0 ranged",
			},
		},
		{
			name: "Cargo.lock",
			path: "Cargo.lock",
			body: `# This file is automatically @generated by Cargo.
# It is not intended for manual editing.
version = 4

[[package]]
name = "app"
version = "0.1.0"
dependencies = [
 "rand 0.8.5",
 "serde",
]

[[package]]
name = "rand"
version = "0.7.3"
source = "` + crates + `"
checksum = "aaa"

[[package]]
name = "rand"
version = "0.8.5"
source = "` + crates + `"
checksum = "bbb"

[[package]]
name = "serde"
version = "1.0.200"
source = "` + crates + `"
checksum = "ccc"
`,
			want: []string{
				"cargo:lock:Cargo.lock -> cargo:app kind=locked local=true resolved_version=0.1.0 set_size=1 ranged",
				"cargo:lock:Cargo.lock -> cargo:rand integrity=aaa kind=locked resolved_version=0.7.3 set_size=2 source=" + crates + " ranged",
				"cargo:lock:Cargo.lock -> cargo:rand integrity=bbb kind=locked resolved_version=0.8.5 set_size=2 source=" + crates + " ranged",
				"cargo:lock:Cargo.lock -> cargo:serde integrity=ccc kind=locked resolved_version=1.0.200 set_size=1 source=" + crates + " ranged",
				"cargo:app -> cargo:rand kind=runtime resolved_version=0.8.5 version=0.1.0 ranged",
				"cargo:app -> cargo:serde kind=runtime resolved_version=1.0.200 version=0.1.0 ranged",
			},
		},
		{
			name: "yarn.lock v1",
			path: "yarn.lock",
			body: `# THIS IS AN AUTOGENERATED FILE. DO NOT EDIT THIS FILE DIRECTLY.
# yarn lockfile v1


"@babel/code-frame@^7.0.0", "@babel/code-frame@^7.10.4":
  version "7.12.13"
  resolved "https://registry.yarnpkg.com/@babel/code-frame/-/code-frame-7.12.13.tgz#dcfc826b"
  integrity sha512-HV1Cm0Q3
  dependencies:
    "@babel/highlight" "^7.12.13"

"@babel/highlight@^7.12.13":
  version "7.13.10"
  resolved "https://registry.yarnpkg.com/@babel/highlight/-/highlight-7.13.10.tgz#a8b2a661"
  integrity sha512-5aPpe5XQ
`,
			want: []string{
				"npm:lock:yarn.lock -> npm:@babel/code-frame integrity=sha512-HV1Cm0Q3 kind=locked resolved_version=7.12.13 set_size=1 " +
					"source=https://registry.yarnpkg.com/@babel/code-frame/-/code-frame-7.12.13.tgz#dcfc826b ranged",
				"npm:lock:yarn.lock -> npm:@babel/highlight integrity=sha512-5aPpe5XQ kind=locked resolved_version=7.13.10 set_size=1 " +
					"source=https://registry.yarnpkg.com/@babel/highlight/-/highlight-7.13.10.tgz#a8b2a661 ranged",
				"npm:@babel/code-frame -> npm:@babel/highlight kind=runtime requirement=^7.12.13 resolved_version=7.13.10 version=7.12.13 ranged",
			},
		},
		{
			name: "Pipfile.lock",
			path: "Pipfile.lock",
			body: `{
    "_meta": {
        "hash": {
            "sha256": "abc"
        },
        "pipfile-spec": 6,
        "requires": {
            "python_version": "3.11"
        },
        "sources": [
            {
                "name": "pypi",
                "url": "https://pypi.org/simple",
                "verify_ssl": true
            }
        ]
    },
    "default": {
        "requests": {
            "hashes": [
                "sha256:aaa",
                "sha256:bbb"
            ],
            "index": "pypi",
            "markers": "python_version >= '3.7'",
            "version": "==2.31.0"
        }
    },
    "develop": {
        "pytest": {
            "hashes": [
                "sha256:ccc"
            ],
            "index": "pypi",
            "version": "==7.4.0"
        }
    }
}
`,
			want: []string{
				"pypi:lock:Pipfile.lock -> pypi:requests group=default hashes=2 kind=locked resolved_version=2.31.0 set_size=1 source=pypi ranged",
				"pypi:lock:Pipfile.lock -> pypi:pytest group=develop hashes=1 kind=locked resolved_version=7.4.0 set_size=1 source=pypi ranged",
			},
		},
		{
			name: "poetry.lock",
			path: "poetry.lock",
			body: `# This file is automatically @generated by Poetry 1.8.2 and should not be changed by hand.

[[package]]
name = "certifi"
version = "2024.2.2"
description = "Python package for providing Mozilla's CA Bundle."
optional = false
python-versions = ">=3.6"
files = [
    {file = "certifi-2024.2.2-py3-none-any.whl", hash = "sha256:aaa"},
    {file = "certifi-2024.2.2.tar.gz", hash = "sha256:bbb"},
]

[[package]]
name = "requests"
version = "2.31.0"
description = "Python HTTP for Humans."
optional = false
python-versions = ">=3.7"
files = [
    {file = "requests-2.31.0-py3-none-any.whl", hash = "sha256:ccc"},
    {file = "requests-2.31.0.tar.gz", hash = "sha256:ddd"},
]

[package.dependencies]
certifi = ">=2017.4.17"
urllib3 = {version = ">=1.21.1,<3", markers = "python_version >= \"3.8\""}

[metadata]
lock-version = "2.0"
python-versions = "^3.11"
content-hash = "abc"
`,
			want: []string{
				"pypi:lock:poetry.lock -> pypi:certifi hashes=2 integrity=sha256:bbb kind=locked resolved_version=2024.2.2 set_size=1 ranged",
				"pypi:lock:poetry.lock -> pypi:requests hashes=2 integrity=sha256:ddd kind=locked resolved_version=2.31.0 set_size=1 ranged",
				"pypi:requests -> pypi:certifi kind=runtime requirement=>=2017.4.17 resolved_version=2024.2.2 version=2.31.0 ranged",
				`pypi:requests -> pypi:urllib3 kind=runtime requirement=>=1.21.1,<3 ; python_version >= "3.8" version=2.31.0 ranged`,
			},
		},
		{
			name: "pdm.lock",
			path: "pdm.lock",
			body: `# This file is @generated by PDM.
# It is not intended for manual editing.

[metadata]
groups = ["default"]
strategy = ["inherit_metadata"]
lock_version = "4.5.0"
content_hash = "sha256:abc"

[[package]]
name = "requests"
version = "2.31.0"
requires_python = ">=3.7"
summary = "Python HTTP for Humans."
groups = ["default"]
dependencies = [
    "Typing_Extensions>=4.0; python_version < \"3.11\"",
    "certifi>=2017.4.17",
]
files = [
    {file = "requests-2.31.0-py3-none-any.whl", hash = "sha256:aaa"},
    {file = "requests-2.31.0.tar.gz", hash = "sha256:bbb"},
]
`,
			want: []string{
				"pypi:lock:pdm.lock -> pypi:requests group=default hashes=2 integrity=sha256:bbb kind=locked resolved_version=2.31.0 set_size=1 ranged",
				"pypi:requests -> pypi:certifi kind=runtime requirement=>=2017.4.17 version=2.31.0 ranged",
				`pypi:requests -> pypi:typing-extensions kind=runtime requirement=>=4.0; python_version < "3.11" version=2.31.0 ranged`,
			},
		},
		{
			name: "yarn.lock berry",
			path: "yarn.lock",
			body: `# This file is generated by running "yarn install" inside your project.
# Manual changes might be lost - proceed with caution!

__metadata:
  version: 8
  cacheKey: 10c0

"js-tokens@npm:^3.0.0 || ^4.0.0":
  version: 4.0.0
  resolution: "js-tokens@npm:4.0.0"
  checksum: 10c0/e248708d
  languageName: node
  linkType: hard

"loose-envify@npm:^1.1.0":
  version: 1.4.0
  resolution: "loose-envify@npm:1.4.0"
  dependencies:
    js-tokens: "npm:^3.0.0 || ^4.0.0"
  bin:
    loose-envify: cli.js
  checksum: 10c0/655d1102
  languageName: node
  linkType: hard

"my-app@workspace:.":
  version: 0.0.0-use.local
  resolution: "my-app@workspace:."
  dependencies:
    loose-envify: "npm:^1.1.0"
  languageName: unknown
  linkType: soft
`,
			want: []string{
				"npm:lock:yarn.lock -> npm:js-tokens integrity=10c0/e248708d kind=locked resolved_version=4.0.0 set_size=1 source=js-tokens@npm:4.0.0 ranged",
				"npm:lock:yarn.lock -> npm:loose-envify integrity=10c0/655d1102 kind=locked resolved_version=1.4.0 set_size=1 source=loose-envify@npm:1.4.0 ranged",
				"npm:lock:yarn.lock -> npm:my-app kind=locked local=true resolved_version=0.0.0-use.local set_size=1 source=my-app@workspace:. ranged",
				"npm:loose-envify -> npm:js-tokens kind=runtime requirement=npm:^3.0.0 || ^4.0.0 resolved_version=4.0.0 version=1.4.0 ranged",
				"npm:my-app -> npm:loose-envify kind=runtime requirement=npm:^1.1.0 resolved_version=1.4.0 version=0.0.0-use.local ranged",
			},
		},
		{
			name: "pnpm-lock.yaml v9",
			path: "pnpm-lock.yaml",
			body: `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      react-dom:
        specifier: ^18.2.0
        version: 18.2.0(react@18.2.0)

packages:

  js-tokens@4.0.0:
    resolution: {integrity: sha512-RdJUflcE}

  loose-envify@1.4.0:
    resolution: {integrity: sha512-lyuxPGr}
    hasBin: true

  react-dom@18.2.0:
    resolution: {integrity: sha512-6IMTriUm}
    peerDependencies:
      react: ^18.2.0

  react@18.2.0:
    resolution: {integrity: sha512-3IjMdb2L}
    engines: {node: '>=0.10.0'}

snapshots:

  js-tokens@4.0.0: {}

  loose-envify@1.4.0:
    dependencies:
      js-tokens: 4.0.0

  react-dom@18.2.0(react@18.2.0):
    dependencies:
      loose-envify: 1.4.0
      react: 18.2.0
      scheduler: 0.23.0

  react@18.2.0:
    dependencies:
      loose-envify: 1.4.0
`,
			want: []string{
				"npm:lock:pnpm-lock.yaml -> npm:js-tokens integrity=sha512-RdJUflcE kind=locked resolved_version=4.0.0 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:loose-envify integrity=sha512-lyuxPGr kind=locked resolved_version=1.4.0 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:react-dom integrity=sha512-6IMTriUm kind=locked resolved_version=18.2.0 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:react integrity=sha512-3IjMdb2L kind=locked resolved_version=18.2.0 set_size=1 ranged",
				"npm:loose-envify -> npm:js-tokens kind=runtime resolved_version=4.0.0 version=1.4.0 ranged",
				"npm:react-dom -> npm:loose-envify kind=runtime resolved_version=1.4.0 version=18.2.0 ranged",
				"npm:react-dom -> npm:react kind=runtime resolved_version=18.2.0 version=18.2.0 ranged",
				"npm:react-dom -> npm:scheduler kind=runtime resolved_version=0.23.0 version=18.2.0 ranged",
				"npm:react -> npm:loose-envify kind=runtime resolved_version=1.4.0 version=18.2.0 ranged",
			},
		},
		{
			name: "pnpm-lock.yaml v5",
			path: "pnpm-lock.yaml",
			body: `lockfileVersion: 5.4

packages:

  /@babel/core/7.17.9:
    resolution: {integrity: sha512-5ug}
    dependencies:
      '@babel/helper-compilation-targets': 7.17.7_@babel+core@7.17.9
    dev: true

  /@babel/helper-compilation-targets/7.17.7_@babel+core@7.17.9:
    resolution: {integrity: sha512-UFz}
    dev: true
`,
			want: []string{
				"npm:lock:pnpm-lock.yaml -> npm:@babel/core integrity=sha512-5ug kind=locked resolved_version=7.17.9 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:@babel/helper-compilation-targets integrity=sha512-UFz kind=locked resolved_version=7.17.7 set_size=1 ranged",
				"npm:@babel/core -> npm:@babel/helper-compilation-targets kind=runtime resolved_version=7.17.7 version=7.17.9 ranged",
			},
		},
		{
			name: "bun.lock",
			path: "bun.lock",
			body: `{
  "lockfileVersion": 1,
  "workspaces": {
    "": {
      "name": "app",
      "dependencies": {
        "loose-envify": "^1.1.0",
      },
    },
  },
  "packages": {
    "js-tokens": ["js-tokens@4.0.0", "", {}, "sha512-RdJ"],

    "loose-envify": ["loose-envify@1.4.0", "", { "dependencies": { "js-tokens": "^3.0.0 || ^4.0.0" }, "bin": { "loose-envify": "cli.js" } }, "sha512-lyu"],

    "other": ["other@1.0.0", "", { "dependencies": { "js-tokens": "^3.0.0" } }, "sha512-oth"],

    "other/js-tokens": ["js-tokens@3.0.2", "", {}, "sha512-old"],
  }
}
`,
			want: []string{
				"npm:lock:bun.lock -> npm:js-tokens integrity=sha512-RdJ kind=locked resolved_version=4.0.0 set_size=2 ranged",
				"npm:lock:bun.lock -> npm:js-tokens integrity=sha512-old kind=locked resolved_version=3.0.2 set_size=2 ranged",
				"npm:lock:bun.lock -> npm:loose-envify integrity=sha512-lyu kind=locked resolved_version=1.4.0 set_size=1 ranged",
				"npm:lock:bun.lock -> npm:other integrity=sha512-oth kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:loose-envify -> npm:js-tokens kind=runtime requirement=^3.0.0 || ^4.0.0 resolved_version=4.0.0 version=1.4.0 ranged",
				"npm:other -> npm:js-tokens kind=runtime requirement=^3.0.0 resolved_version=3.0.2 version=1.0.0 ranged",
			},
		},
		{
			name: "uv.lock",
			path: "uv.lock",
			body: `version = 1
revision = 2
requires-python = ">=3.12"

[[package]]
name = "app"
version = "0.1.0"
source = { editable = "." }
dependencies = [
    { name = "requests" },
]

[package.optional-dependencies]
socks = [
    { name = "pysocks" },
]

[[package]]
name = "pysocks"
version = "1.7.1"
source = { registry = "https://pypi.org/simple" }
sdist = { url = "https://files.pythonhosted.org/packages/PySocks-1.7.1.tar.gz", hash = "sha256:aaa", size = 284872 }
wheels = [
    { url = "https://files.pythonhosted.org/packages/PySocks-1.7.1-py3-none-any.whl", hash = "sha256:bbb", size = 16725 },
]

[[package]]
name = "requests"
version = "2.32.3"
source = { registry = "https://pypi.org/simple" }
dependencies = [
    { name = "urllib3", marker = "python_full_version >= '3.12'" },
]
sdist = { url = "https://files.pythonhosted.org/packages/requests-2.32.3.tar.gz", hash = "sha256:ccc", size = 131218 }
wheels = [
    { url = "https://files.pythonhosted.org/packages/requests-2.32.3-py3-none-any.whl", hash = "sha256:ddd", size = 64928 },
]

[[package]]
name = "urllib3"
version = "2.2.3"
source = { registry = "https://pypi.org/simple" }
wheels = [
    { url = "https://files.pythonhosted.org/packages/urllib3-2.2.3-py3-none-any.whl", hash = "sha256:eee", size = 126338 },
]
`,
			want: []string{
				"pypi:lock:uv.lock -> pypi:app kind=locked local=true resolved_version=0.1.0 set_size=1 source=editable=. ranged",
				"pypi:lock:uv.lock -> pypi:pysocks hashes=2 integrity=sha256:aaa kind=locked resolved_version=1.7.1 set_size=1 source=registry=https://pypi.org/simple ranged",
				"pypi:lock:uv.lock -> pypi:requests hashes=2 integrity=sha256:ccc kind=locked resolved_version=2.32.3 set_size=1 source=registry=https://pypi.org/simple ranged",
				"pypi:lock:uv.lock -> pypi:urllib3 hashes=1 kind=locked resolved_version=2.2.3 set_size=1 source=registry=https://pypi.org/simple ranged",
				"pypi:app -> pypi:pysocks kind=optional requirement=[socks] resolved_version=1.7.1 version=0.1.0 ranged",
				"pypi:app -> pypi:requests kind=runtime resolved_version=2.32.3 version=0.1.0 ranged",
				"pypi:requests -> pypi:urllib3 kind=runtime requirement=; python_full_version >= '3.12' resolved_version=2.2.3 version=2.32.3 ranged",
			},
		},
		{
			name: "pylock.toml",
			path: "pylock.toml",
			body: `lock-version = "1.0"
environments = ["sys_platform == 'linux'"]
requires-python = ">=3.12"
created-by = "pip"

[[packages]]
name = "attrs"
version = "25.1.0"
requires-python = ">=3.8"
index = "https://pypi.org/simple"

[[packages.wheels]]
name = "attrs-25.1.0-py3-none-any.whl"
upload-time = 2025-01-25T11:30:10.164985+00:00
url = "https://files.pythonhosted.org/packages/attrs-25.1.0-py3-none-any.whl"
size = 63152
hashes = {sha256 = "aaa"}

[[packages]]
name = "cattrs"
version = "24.1.2"
requires-python = ">=3.8"
dependencies = [
    {name = "attrs"},
]
index = "https://pypi.org/simple"

[packages.sdist]
name = "cattrs-24.1.2.tar.gz"
url = "https://files.pythonhosted.org/packages/cattrs-24.1.2.tar.gz"
size = 426462
hashes = {sha256 = "bbb"}

[[packages.wheels]]
name = "cattrs-24.1.2-py3-none-any.whl"
url = "https://files.pythonhosted.org/packages/cattrs-24.1.2-py3-none-any.whl"
size = 66446
hashes = {sha256 = "ccc"}
`,
			want: []string{
				"pypi:lock:pylock.toml -> pypi:attrs hashes=1 kind=locked resolved_version=25.1.0 set_size=1 source=https://pypi.org/simple ranged",
				"pypi:lock:pylock.toml -> pypi:cattrs hashes=2 integrity=sha256:bbb kind=locked resolved_version=24.1.2 set_size=1 source=https://pypi.org/simple ranged",
				"pypi:cattrs -> pypi:attrs kind=runtime resolved_version=25.1.0 version=24.1.2 ranged",
			},
		},
		{
			name: "gradle.lockfile",
			path: "app/gradle.lockfile",
			body: `# This is a Gradle generated file for dependency locking.
# Manual edits can break the build and are not advised.
# This file is expected to be part of source control.
com.google.guava:guava:33.0.0-jre=compileClasspath,runtimeClasspath
org.slf4j:slf4j-api:2.0.9=runtimeClasspath
empty=annotationProcessor
`,
			want: []string{
				"maven:lock:app/gradle.lockfile -> maven:com.google.guava:guava group=compileClasspath,runtimeClasspath kind=locked resolved_version=33.0.0-jre set_size=1 ranged",
				"maven:lock:app/gradle.lockfile -> maven:org.slf4j:slf4j-api group=runtimeClasspath kind=locked resolved_version=2.0.9 set_size=1 ranged",
			},
		},
		{
			name: "npm-shrinkwrap.json",
			path: "npm-shrinkwrap.json",
			body: `{
  "name": "app",
  "version": "1.0.0",
  "lockfileVersion": 3,
  "packages": {
    "": {
      "name": "app",
      "version": "1.0.0",
      "dependencies": {
        "js-yaml": "npm:@zkochan/js-yaml@0.0.6"
      }
    },
    "node_modules/js-yaml": {
      "name": "@zkochan/js-yaml",
      "version": "0.0.6",
      "integrity": "sha512-zk"
    }
  }
}
`,
			want: []string{
				"npm:lock:npm-shrinkwrap.json -> npm:@zkochan/js-yaml alias=js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 ranged",
				"npm:lock:npm-shrinkwrap.json -> npm:app kind=locked local=true resolved_version=1.0.0 set_size=1 ranged",
				"npm:app -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime requirement=npm:@zkochan/js-yaml@0.0.6 resolved_version=0.0.6 version=1.0.0 ranged",
			},
		},
		{
			name: "package-lock v1 alias",
			path: "package-lock.json",
			body: `{
  "name": "app",
  "lockfileVersion": 1,
  "dependencies": {
    "js-yaml": {
      "version": "npm:@zkochan/js-yaml@0.0.6",
      "integrity": "sha512-zk"
    },
    "other": {
      "version": "1.0.0",
      "requires": {
        "js-yaml": "npm:@zkochan/js-yaml@0.0.6"
      }
    }
  }
}
`,
			want: []string{
				"npm:lock:package-lock.json -> npm:@zkochan/js-yaml alias=js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 ranged",
				"npm:lock:package-lock.json -> npm:other kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:other -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime requirement=npm:@zkochan/js-yaml@0.0.6 resolved_version=0.0.6 version=1.0.0 ranged",
			},
		},
		{
			name: "bun.lock alias",
			path: "web/bun.lock",
			body: `{
  "lockfileVersion": 1,
  "packages": {
    "js-yaml": ["@zkochan/js-yaml@0.0.6", "", {}, "sha512-zk"],
    "other": ["other@1.0.0", "", { "dependencies": { "js-yaml": "npm:@zkochan/js-yaml@0.0.6" } }, "sha512-oth"],
  }
}
`,
			want: []string{
				"npm:lock:web/bun.lock -> npm:@zkochan/js-yaml alias=js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 ranged",
				"npm:lock:web/bun.lock -> npm:other integrity=sha512-oth kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:other -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime requirement=npm:@zkochan/js-yaml@0.0.6 resolved_version=0.0.6 version=1.0.0 ranged",
			},
		},
		{
			name: "yarn.lock alias",
			path: "yarn.lock",
			body: `# yarn lockfile v1


"js-yaml@npm:@zkochan/js-yaml@0.0.6":
  version "0.0.6"
  resolved "https://registry.yarnpkg.com/@zkochan/js-yaml/-/js-yaml-0.0.6.tgz#abc"
  integrity sha512-zk

other@^1.0.0:
  version "1.0.0"
  dependencies:
    js-yaml "npm:@zkochan/js-yaml@0.0.6"
`,
			want: []string{
				"npm:lock:yarn.lock -> npm:@zkochan/js-yaml alias=js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 source=https://registry.yarnpkg.com/@zkochan/js-yaml/-/js-yaml-0.0.6.tgz#abc ranged",
				"npm:lock:yarn.lock -> npm:other kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:other -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime requirement=npm:@zkochan/js-yaml@0.0.6 resolved_version=0.0.6 version=1.0.0 ranged",
			},
		},
		{
			name: "pnpm-lock.yaml v9 alias",
			path: "pnpm-lock.yaml",
			body: `lockfileVersion: '9.0'

packages:

  '@zkochan/js-yaml@0.0.6':
    resolution: {integrity: sha512-zk}

  other@1.0.0:
    resolution: {integrity: sha512-oth}

snapshots:

  '@zkochan/js-yaml@0.0.6': {}

  other@1.0.0:
    dependencies:
      js-yaml: '@zkochan/js-yaml@0.0.6'
`,
			want: []string{
				"npm:lock:pnpm-lock.yaml -> npm:@zkochan/js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:other integrity=sha512-oth kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:other -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime resolved_version=0.0.6 version=1.0.0 ranged",
			},
		},
		{
			name: "pnpm-lock.yaml v6 alias",
			path: "pnpm-lock.yaml",
			body: `lockfileVersion: '6.0'

packages:

  /@zkochan/js-yaml@0.0.6:
    resolution: {integrity: sha512-zk}
    dev: false

  /other@1.0.0:
    resolution: {integrity: sha512-oth}
    dependencies:
      js-yaml: /@zkochan/js-yaml@0.0.6
      tslib: 2.6.2
    dev: false
`,
			want: []string{
				"npm:lock:pnpm-lock.yaml -> npm:@zkochan/js-yaml integrity=sha512-zk kind=locked resolved_version=0.0.6 set_size=1 ranged",
				"npm:lock:pnpm-lock.yaml -> npm:other integrity=sha512-oth kind=locked resolved_version=1.0.0 set_size=1 ranged",
				"npm:other -> npm:@zkochan/js-yaml alias=js-yaml kind=runtime resolved_version=0.0.6 version=1.0.0 ranged",
				"npm:other -> npm:tslib kind=runtime resolved_version=2.6.2 version=1.0.0 ranged",
			},
		},
		{
			name: "Cargo.lock v1",
			path: "Cargo.lock",
			body: `[[package]]
name = "app"
version = "0.1.0"
dependencies = [
 "rand 0.8.5 (` + crates + `)",
]

[[package]]
name = "rand"
version = "0.8.5"
source = "` + crates + `"

[[package]]
name = "rand_core"
version = "0.6.4"
source = "` + crates + `"

[metadata]
"checksum rand 0.8.5 (` + crates + `)" = "34af8d1a"
"checksum rand_core 0.6.4 (` + crates + `)" = "<none>"
`,
			want: []string{
				"cargo:lock:Cargo.lock -> cargo:app kind=locked local=true resolved_version=0.1.0 set_size=1 ranged",
				"cargo:lock:Cargo.lock -> cargo:rand integrity=34af8d1a kind=locked resolved_version=0.8.5 set_size=1 source=" + crates + " ranged",
				"cargo:lock:Cargo.lock -> cargo:rand_core kind=locked resolved_version=0.6.4 set_size=1 source=" + crates + " ranged",
				"cargo:app -> cargo:rand kind=runtime resolved_version=0.8.5 version=0.1.0 ranged",
			},
		},
		{
			name: "poetry.lock 1.1",
			path: "poetry.lock",
			body: `[[package]]
name = "attrs"
version = "21.4.0"
description = "Classes Without Boilerplate"
category = "main"
optional = false
python-versions = ">=2.7"

[[package]]
name = "six"
version = "1.16.0"
category = "main"
optional = false
python-versions = ">=2.7"

[[package]]
name = "six"
version = "1.15.0"
category = "dev"
optional = false
python-versions = ">=2.7"

[metadata]
lock-version = "1.1"
python-versions = "^3.8"
content-hash = "abc"

[metadata.files]
attrs = [
    {file = "attrs-21.4.0-py2.py3-none-any.whl", hash = "sha256:aaa"},
    {file = "attrs-21.4.0.tar.gz", hash = "sha256:bbb"},
]
six = [
    {file = "six-1.16.0-py2.py3-none-any.whl", hash = "sha256:s16w"},
    {file = "six-1.16.0.tar.gz", hash = "sha256:s16"},
    {file = "six-1.15.0.tar.gz", hash = "sha256:s15"},
]
`,
			want: []string{
				"pypi:lock:poetry.lock -> pypi:attrs group=main hashes=2 integrity=sha256:bbb kind=locked resolved_version=21.4.0 set_size=1 ranged",
				"pypi:lock:poetry.lock -> pypi:six group=dev hashes=1 integrity=sha256:s15 kind=locked resolved_version=1.15.0 set_size=2 ranged",
				"pypi:lock:poetry.lock -> pypi:six group=main hashes=2 integrity=sha256:s16 kind=locked resolved_version=1.16.0 set_size=2 ranged",
			},
		},
		{
			name: "poetry.lock 1.0",
			path: "poetry.lock",
			body: `[[package]]
name = "attrs"
version = "19.3.0"
category = "main"
optional = false
python-versions = "*"

[metadata]
content-hash = "abc"
python-versions = "*"

[metadata.hashes]
attrs = ["sha256:aaa", "sha256:bbb"]
`,
			want: []string{
				"pypi:lock:poetry.lock -> pypi:attrs group=main hashes=2 kind=locked resolved_version=19.3.0 set_size=1 ranged",
			},
		},
		{
			name: "Pipfile.lock hg",
			path: "Pipfile.lock",
			body: `{
    "_meta": {"pipfile-spec": 6},
    "default": {
        "mylib": {"hg": "https://hg.example.org/mylib", "ref": "abc123"}
    },
    "develop": {}
}
`,
			want: []string{
				"pypi:lock:Pipfile.lock -> pypi:mylib group=default kind=locked resolved_version=abc123 set_size=1 source=hg+https://hg.example.org/mylib ranged",
			},
		},
		{
			name: "pdm.lock svn",
			path: "pdm.lock",
			body: `[metadata]
lock_version = "4.5.0"

[[package]]
name = "mylib"
version = "1.0"
svn = "https://svn.example.org/mylib"
revision = "42"
`,
			want: []string{
				"pypi:lock:pdm.lock -> pypi:mylib kind=locked resolved_version=1.0 set_size=1 source=svn+https://svn.example.org/mylib@42 ranged",
			},
		},
		{
			name: "pylock.<name>.toml vcs",
			path: "pylock.dev.toml",
			body: `lock-version = "1.0"
created-by = "tool"

[[packages]]
name = "mylib"
version = "1.0"
vcs = {type = "hg", url = "https://hg.example.org/mylib", commit-id = "deadbeef"}
`,
			want: []string{
				"pypi:lock:pylock.dev.toml -> pypi:mylib kind=locked resolved_version=1.0 set_size=1 source=hg+https://hg.example.org/mylib@deadbeef ranged",
			},
		},
		{
			name: "buildscript-gradle.lockfile",
			path: "buildscript-gradle.lockfile",
			body: `# This is a Gradle generated file for dependency locking.
com.example:plugin:1.0=classpath
empty=
`,
			want: []string{
				"maven:lock:buildscript-gradle.lockfile -> maven:com.example:plugin group=classpath kind=locked resolved_version=1.0 set_size=1 ranged",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap, _ := runLock(t, map[string]string{tc.path: tc.body}, tc.path)
			wantLines(t, tc.name, lockLines(t, cap), tc.want)
		})
	}
}

// TestLockJoinsTheManifestDependencyByName pins the join: the dependency
// node a lock file pins is the very node the manifest's requirement of the
// same name reaches, for every ecosystem's naming rule. Mutation caught: a
// lock reader that minted its own identity (another qualified-name form) or
// skipped the ecosystem's name normalization (PEP 503 for Python) would
// leave the manifest's node and the lock's apart.
func TestLockJoinsTheManifestDependencyByName(t *testing.T) {
	files := map[string]string{
		"pyproject.toml": "[project]\nname = \"svc\"\nversion = \"1.0\"\ndependencies = [\"Typing.Extensions>=4\"]\n",
		"pdm.lock": "[metadata]\nlock_version = \"4.5.0\"\n\n[[package]]\nname = \"Typing_Extensions\"\nversion = \"4.12.2\"\n" +
			"files = [\n    {file = \"typing_extensions-4.12.2.tar.gz\", hash = \"sha256:aaa\"},\n]\n",
		"package.json":      "{\"name\": \"web\", \"dependencies\": {\"@scope/ui\": \"^2.0.0\"}}\n",
		"package-lock.json": "{\"lockfileVersion\": 3, \"packages\": {\"node_modules/@scope/ui\": {\"version\": \"2.3.0\"}}}\n",
	}
	cap, _ := runLock(t, files, "pyproject.toml", "pdm.lock", "package.json", "package-lock.json")
	reached := map[model.NodeKind]map[string]model.NodeID{}
	for _, r := range cap.relations {
		from, to := cap.nodes[r.Relation.From], cap.nodes[r.Relation.To]
		if r.Relation.Kind != model.RelDependsOn || to.Kind != model.NodeDependency || from.Kind == model.NodeDependency {
			continue
		}
		if reached[from.Kind] == nil {
			reached[from.Kind] = map[string]model.NodeID{}
		}
		reached[from.Kind][to.QualifiedName] = to.ID
	}
	for _, name := range []string{"pypi:typing-extensions", "npm:@scope/ui"} {
		declared, locked := reached[model.NodePackage][name], reached[model.NodeConfiguration][name]
		if declared == "" || declared != locked {
			t.Fatalf("%s: manifest reaches %q, lock pins %q; want one node\n%s", name, declared, locked, fmt.Sprint(reached))
		}
	}
}
