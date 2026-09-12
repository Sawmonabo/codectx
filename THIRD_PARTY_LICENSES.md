# Third-party licenses

codectx is licensed under the Apache License 2.0 (see `LICENSE`). The binary
links the components below. The list is the module set reported by
`go list -deps ./cmd/codectx`, not the full `go list -m all` module graph, so it
excludes modules that only take part in dependency resolution or in a
dependency's own tooling. Each license was read from the module cache at the
pinned version; none is copyleft or carries a commercial-use restriction.

Regenerate the module set with:

```bash
go list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' ./cmd/codectx | sort -u
GOOS=windows go list -deps -f '{{with .Module}}{{.Path}} {{.Version}}{{end}}' ./cmd/codectx | sort -u
```

| Module | Version | License | Notes |
|---|---|---|---|
| github.com/spf13/cobra | v1.10.2 | Apache-2.0 (`LICENSE.txt`) | Command parsing. |
| github.com/spf13/pflag | v1.0.9 | BSD-3-Clause (`LICENSE`) | Flag parsing; required by cobra. Copyright 2012 Alex Ogier and The Go Authors. |
| github.com/inconshreveable/mousetrap | v1.1.0 | Apache-2.0 (`LICENSE`) | Windows-only; linked into the Windows targets only. Copyright 2022 Alan Shreve. |

The Go standard library and toolchain are covered by the Go project's
BSD-3-Clause license and are not redistributed as source by this repository.

Bundled native assets, such as pinned Tree-sitter grammars and query packs, are
not part of the build yet. They must be inventoried here with their own
licenses when they are added, because a module manifest does not describe a
vendored grammar file.
