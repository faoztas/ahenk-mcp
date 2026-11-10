# Third-party licensing

Ahenk MCP source is licensed under Apache-2.0. The Go toolchain and linked modules remain under their upstream licenses. The project source tree does not vendor those modules, and the release does not include provider CLI executables.

`make dist` builds the Intel macOS executable and creates `dist/ahenk-darwin-amd64.tar.gz`. The archive includes the project's complete `LICENSE`, this inventory, and original upstream legal files under `licenses/`. The packager identifies dependencies using the binary's target package graph and copies legal files from each package's directory ancestry, including the Go toolchain and its bundled packages. Use `go version -m ahenk` to inspect the executable's module versions.

| Component | Upstream terms represented in the archive |
| --- | --- |
| Go runtime and standard library | Go's BSD-style license and patent grant. The archive also preserves legal files for included bundled packages. |
| `github.com/modelcontextprotocol/go-sdk` | The pinned SDK's license records an Apache-2.0 transition, retained MIT terms for applicable contributions, and CC-BY-4.0 for non-specification documentation. The archive preserves the upstream license in full. |
| `github.com/dustin/go-humanize`, `github.com/google/jsonschema-go`, `github.com/mattn/go-isatty`, `github.com/ncruces/go-strftime`, `github.com/segmentio/asm`, `github.com/segmentio/encoding` | MIT |
| `github.com/google/uuid`, `github.com/remyoudompheng/bigfft`, `github.com/yosida95/uritemplate/v3`, `golang.org/x/oauth2`, `golang.org/x/sync`, `golang.org/x/sys`, `golang.org/x/time` | BSD-style licenses and upstream patent grants where supplied |
| `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory`, `modernc.org/sqlite` | BSD-style licenses plus the original SQLite dedication and included third-party terms and notices. The archive retains the modules' full upstream attribution files. |

The source tree uses no project-level `NOTICE`. The pinned distribution dependencies provide attribution through their license and supplemental notice files, which are retained individually in the binary archive. When dependencies change, review their terms, inspect the updated archive, and update this inventory before redistribution.
