# Go tools and linting

The repository has three application modules: `orlop`, `platform-api`, and
`controllers`. Each requires Go 1.26.10. The separate `hack/tools` module also requires
Go 1.26.10 and pins golangci-lint and kube-api-linter with Go `tool` directives.
No root application module or Go workspace is needed.

## Prerequisites

- Go 1.26.10 installed on `PATH`, which includes the fix for CVE-2026-111943.
  Root Makefile targets check the installed version and set `GOTOOLCHAIN=local`;
  they fail if the required compiler is missing instead of downloading another
  toolchain. For direct Go commands or module-specific Makefile targets, use
  the same installed compiler and set `GOTOOLCHAIN=local`.
- GNU Make, Git, and a C compiler for Go plugins (for example, Xcode Command Line
  Tools on macOS or GCC on Linux). Linting sets `CGO_ENABLED=1`.
- Network access to download pinned modules.

Production Containerfiles use the Red Hat Go toolchain bundled with the UBI 9
builder and set `GOTOOLCHAIN=local`. Builder and CI images must provide Go
1.26.10 before compiling or running Makefile targets.

Linter and generator executables do not need to be installed on `PATH`.
The shared `hack/lint.mk` invokes
`go tool -modfile=<absolute path to hack/tools/go.mod> golangci-lint` from the
application directory. The linter therefore analyzes that application's module
while its own dependencies come from the tools module.

Each `lint` and `lint-fix` invocation builds the plugin into
`hack/tools/bin/kube-api-linter.so`. The build uses the same tools module,
toolchain, CGO setting, and inherited `GOFLAGS` as the linter. Go's build cache
avoids unnecessary compilation and accounts for dependency and toolchain changes.
Generated tool artifacts are ignored by Git. The API modules load the plugin;
controllers uses only the general linters.

On macOS, both builds add `-Wl,-no_fixup_chains` to the C linker flags. This
avoids the macOS 27 loader's "chained fixups, seg_count does not match number of
segments" error observed with Go 1.26.4 plugins.

## Running lint

Run these commands from the repository root:

```sh
make lint                 # Full lint in all three modules
make -C controllers lint  # Full lint in one module
make lint-fix             # Autofix all three modules
make lint-fmt             # Format imports in all three modules
```

All lint targets process the full modules, subject to their configured exclusions.
Root lint targets visit every module and fail if any module fails.

Each module owns its `.golangci.yml`. The API modules retain their API-specific
rules and exemptions; controllers uses the same general rules and import groups.
The staticcheck compatibility disable is retained for developers using Go 1.27.
Controllers exemptions document the diagnostic, affected paths, and reason next
to each rule. Larger behavior changes belong in separate cleanup work.

## Generating APIs

```sh
make -C orlop generate
make -C platform-api generate
```

The module-specific generators continue to run with `go run ./cmd/orlop-gen`.
Orlop also declares `openapi-gen` and `controller-gen` as tools in `orlop/go.mod`
and invokes them with `go tool`.

The current baseline emits schema warnings about the `NetworkType` default
marker in platform-api. It also has generated-file drift in public API types
and OpenAPI descriptions. Generation with the tool directives reproduces the
same output as the previous commands; review this existing drift separately
from tool updates.

## Updating tools

Use explicit versions when updating a tool, then inspect the module and checksum
diffs. For example, from the repository root (replace each version placeholder):

```sh
go -C hack/tools get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint@<version>
go -C hack/tools get -tool sigs.k8s.io/kube-api-linter/pkg/plugin@<version>
go -C hack/tools mod tidy

go -C orlop get -tool k8s.io/kube-openapi/cmd/openapi-gen@<version>
go -C orlop get -tool sigs.k8s.io/controller-tools/cmd/controller-gen@<version>
go -C orlop mod tidy
```

Generator dependencies share the orlop application module, so review their effect
on application dependencies. Check that the Go requirements remain intentional,
run generation in both API modules, and run lint and `make test` after updates.
Database integration tests require their separately configured services; see the
module documentation for those prerequisites.
