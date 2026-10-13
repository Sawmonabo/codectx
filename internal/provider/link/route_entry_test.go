package link_test

import "testing"

// TestPyprojectScriptsAreEndpoints is a pyproject.toml declaring a console
// script, a GUI script and an entry-points group, each `module:attr` in the
// src layout: each is an endpoint of the file the module is, routed to the
// declaration. Mutations caught: the entry point found by the file's name
// rather than the declared module (an endpoint in tools/cli.py, which also
// declares main), the attribute ignored (routed to the module), and the
// src layout not read (no endpoint).
func TestPyprojectScriptsAreEndpoints(t *testing.T) {
	checkMounted(t, map[string]string{
		"pyproject.toml": `[project]
name = "demo"
version = "1.0"

[project.scripts]
demo = "demo.cli:main"

[project.gui-scripts]
demo-gui = "demo.gui:App.run"

[project.entry-points."demo.plugins"]
extra = "demo.cli:plugin"
`,
		"src/demo/cli.py": `def main():
    pass


def plugin():
    pass
`,
		"src/demo/gui.py": `class App:
    def run(self):
        pass
`,
		"tools/cli.py": `def main():
    pass
`,
	}, []string{
		"src/demo/cli.py console_scripts:demo -> main",
		"src/demo/cli.py demo.plugins:extra -> plugin",
		"src/demo/gui.py gui_scripts:demo-gui -> run",
	})
}

// TestPackageJSONBinAndMainAreEndpoints is a package.json whose `bin` maps
// commands to files and whose `main` names a file without its extension:
// each is an endpoint of that file, routed to its module, `main` found as
// Node's require finds a file. Mutations caught: `main` matched only as
// written (no endpoint for lib/index.js), and a bin path not cleaned (no
// endpoint for bin/cli.js written ./bin/cli.js).
func TestPackageJSONBinAndMainAreEndpoints(t *testing.T) {
	checkMounted(t, map[string]string{
		"package.json": `{"name": "@acme/tool", "bin": {"tool": "./bin/cli.js"}, "main": "lib/index"}
`,
		"bin/cli.js": `function run() {}
run();
`,
		"lib/index.js": `export function api() {}
`,
	}, []string{
		"bin/cli.js bin:tool -> cli.js",
		"lib/index.js main -> index.js",
	})
}

// TestCargoBinIsAnEndpoint is a Cargo.toml `[[bin]]` with no path, which
// Cargo reads from src/bin/<name>.rs: its endpoint routes to that file's
// `fn main`. Mutations caught: the default path not applied (no endpoint),
// and the bin routed to the file's module rather than its `main`.
func TestCargoBinIsAnEndpoint(t *testing.T) {
	checkMounted(t, map[string]string{
		"Cargo.toml": `[package]
name = "demo"
version = "0.1.0"

[[bin]]
name = "tool"
`,
		"src/lib.rs": `pub fn lib() {}
`,
		"src/bin/tool.rs": `fn main() {}
`,
	}, []string{"src/bin/tool.rs bin:tool -> main"})
}
