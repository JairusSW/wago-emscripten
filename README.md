<div align="center">
  <h1><code>emscripten</code></h1>
  <p>Standalone Emscripten, Go <code>js/wasm</code>, and Ruby JS-ABI programs for <a href="https://github.com/wago-org/wago">Wago</a>.</p>
</div>

<p align="center">
  <a href="https://github.com/JairusSW/wago-emscripten/actions/workflows/ci.yml"><img src="https://github.com/JairusSW/wago-emscripten/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/go-%3E%3D1.22-00ADD8.svg" alt="Go >= 1.22"></a>
  <a href="https://github.com/wago-org/wago"><img src="https://img.shields.io/badge/wago-%3E%3D0.1.0-6E56CF.svg" alt="Wago >= 0.1.0"></a>
</p>

`emscripten` is an optional compatibility plugin for command-style WebAssembly
that was emitted for JavaScript but can run without a browser, Node.js, or a
JavaScript engine. It recognizes supported ABIs from module structure and
imports; it does not maintain an application allowlist.

The plugin transforms compatible modules into standalone Wago programs, then
provides the small host surface they still need: startup, typed callbacks, heap
growth, clocks, randomness, console I/O, and a bounded in-memory filesystem.
It also supports the Go `js/wasm` and Ruby JS-ABI paths exercised by esbuild and
Ruby.

> `emscripten` is experimental (`v0.4.0`). Supported standalone programs execute
> real workloads in CI; modules that require their generated JavaScript remain
> unchanged and fail normal import resolution.

## Install

Add the plugin to a Wago project:

```sh
wago init
wago add github.com/JairusSW/wago-emscripten
wago run program.wasm -- first second
```

The installer resolves both WASI Preview 1 compatibility providers, presents
the plugin's exact authorities for review, and records the complete selection
in `wago-lock.json`. Generated runtimes link the explicit
`github.com/JairusSW/wago-emscripten/register` catalog; importing the package
does not mutate a global registry.

For a shared user installation:

```sh
wago add --global github.com/JairusSW/wago-emscripten
wago run --global program.wasm -- first second
```

## What runs

The compatibility layer currently provides:

- Emscripten standalone startup through `main` or `__main_argc_argv`, including
  constructors, `argv`, environment data, internalized memory, and heap growth.
- Typed `invoke_*` callbacks, legalized i64 helpers, large memory copies, UTC
  time conversion, and Emscripten-mode `setjmp`/`longjmp`.
- WASI-backed realtime and monotonic clocks, cryptographic randomness, and
  descriptor I/O shared with Emscripten's `env` syscalls.
- Per-instance regular files and directories with open, read, write, seek,
  positioned I/O, descriptor duplication, flags, metadata, rename, unlink,
  truncate, allocation, sync, and shared file-backed `mmap` writeback.
- The Go `js/wasm` ABI used by esbuild and the Ruby JS-ABI/canonical ABI used by
  the Wago Ruby build.

Tests validate output and API results after substantial work. Successful
instantiation by itself is not counted as support.

| Binary | Work verified |
| --- | --- |
| Official `@sqlite.org/sqlite-wasm` | Creates a database and index, inserts 5,000 rows with a recursive CTE, and checks an aggregate query. |
| Emscripten C compute fixture | Grows memory, sorts 2.5 million integers with `qsort`, and checks the result. |
| Emscripten C time fixture | Converts and formats a fixed Unix timestamp and checks the exact UTC output. |
| Emscripten C filesystem fixture | Writes and rereads 2,000 records while exercising directories, metadata, positioned I/O, allocation, rename, `mmap` writeback, and cleanup. |
| Emscripten C system fixture | Reads configured environment and argv, consumes 4 KiB of randomness, and checks both clocks. |
| Emscripten C++ fixture | Sorts and accumulates 100,000 values using STL containers, RAII, virtual dispatch, and formatted output. |
| Emscripten setjmp fixture | Unwinds 200 recursive frames through invoke trampolines and verifies the `longjmp` result. |
| Wago `lua.wasm` | Opens Lua libraries, sorts 5,000 values, allocates strings, and reads the result through the C API. |
| Wago `sqlite3.wasm` | Creates and indexes a 5,000-row database and checks an aggregate query. |
| Wago `ruby.wasm` | Evaluates a 2,000-element Enumerable workload and reads its result through the canonical ABI. |
| Wago `esbuild.wasm` | Parses and minifies a generated 1,000-function JavaScript module through Go `js/wasm`. |
| Wago `regexmatch.wasm` | Runs 3,000 regex iterations and checks the workload checksum. |
| Wago `wasm3.wasm` | Loads a Wasm module into the interpreter and verifies recursive `fib(25) = 75025`. |

## Configuration

Configuration is strict JSON. Unknown fields, malformed environment entries,
trailing JSON, and limits outside the documented ranges are rejected before
the provider starts.

```sh
wago plugin config github.com/JairusSW/wago-emscripten \
  '{"env":["LANG=C.UTF-8","MODE=batch"],"maxFilesystemBytes":33554432,"maxOpenFiles":1024}'
```

| Field | Values | Default |
| --- | --- | --- |
| `stdin` | `"inherit"` or `"eof"` | `"inherit"` |
| `stdout`, `stderr` | `"inherit"` or `"discard"` | `"inherit"` |
| `env` | Up to 256 `KEY=VALUE` strings | Empty |
| `maxFilesystemBytes` | 64 KiB to 256 MiB | 32 MiB |
| `maxOpenFiles` | 3 to 65,536, including stdio | 1,024 |

The filesystem is ephemeral, isolated per guest instance, and never exposes
host paths.

## Authorities

The provider requests six required Wago authorities:

| Authority | Why it is needed |
| --- | --- |
| `host.import.define` | Define only the reviewed `env`, `go`, `rb-js-abi-host`, and `canonical_abi` imports. |
| `host.arguments.read` | Construct standalone and Go `js/wasm` argument vectors. |
| `host.caller.identify` | Keep compatibility state and files isolated by guest instance. |
| `host.caller.invoke` | Re-enter typed Emscripten and Go callbacks on the active guest. |
| `instance.close.observe` | Release all per-instance state when a guest closes. |
| `module.source.transform` | Internalize memory and add typed callbacks, heap growth, and standalone launchers. |

The merged Wago active-caller support scopes callback re-entry to the current
synchronous guest call. The plugin does not receive arbitrary runtime,
instance-management, host-filesystem, network, or subprocess authority.

## Compatibility boundary

This is not a browser or a general JavaScript runtime. DOM and Node APIs,
Embind/emval, application-specific JavaScript callbacks, minified glue
contracts, pthreads/shared memory, sockets, subprocesses, and dynamic linking
still require their generated JavaScript or a separate capability provider.

Native Wasm exception handling remains a Wago engine boundary. C++ built with
`-fno-exceptions` and Emscripten-mode `longjmp` execute. Local time is
deterministic UTC. Unknown modules and known glue-dependent modules are left
byte-for-byte unchanged instead of guessing the meaning of their imports.

## Upstream corpus

`corpus.lock.json` pins archive and extracted-Wasm SHA-256 digests. The fetcher
extracts only the named regular file, validates the Wasm magic, enforces size
limits, and atomically installs it beneath the ignored `.corpus/` directory.

```sh
go run ./cmd/corpusfetch          # quick CI set
go run ./cmd/corpusfetch -full    # all nine pinned binaries
```

| Corpus | Version | Result |
| --- | ---: | --- |
| `@sqlite.org/sqlite-wasm` | `3.46.1-build5` | Executes the database workload on arm64 and amd64. |
| `@sqlite.org/sqlite-wasm` | `3.53.0-build1` | Tracked Wago amd64 backend trap during `CREATE TABLE`; executes on arm64. |
| `sql.js` | `1.14.2` | Generated glue required: minified JS import/export names. |
| `@foxglove/wasm-zstd` | `1.0.1` | Generated glue required: minified JS imports. |
| `@imagemagick/magick-wasm` | `0.0.43` | Generated glue required: large minified JS ABI. |
| `@ffmpeg/core` | `0.12.10` | Generated glue required: large minified JS ABI. |
| `@duckdb/duckdb-wasm` MVP | `1.33.1-dev57.0` | Generated glue required: Embind, emval, and browser filesystem APIs. |
| `@duckdb/duckdb-wasm` EH | `1.33.1-dev57.0` | Same JS API boundary, plus Wasm exception handling. |
| `brotli-wasm` | `3.0.1` | wasm-bindgen control sample; correctly left untouched. |

The glue-required entries are negative capability tests: the plugin must leave
them unchanged rather than claim support based on successful decoding.

## Architecture

| File | Responsibility |
| --- | --- |
| `transform.go` | Detect compatible modules and build standalone launchers and trampolines. |
| `emscripten.go` | Register the Emscripten host ABI and system helpers. |
| `filesystem.go` | Own the bounded per-instance filesystem and descriptor table. |
| `gojs.go` | Implement the Go `js/wasm` value and callback ABI. |
| `ruby.go` | Implement the Ruby JS-ABI and canonical ABI bridge. |
| `plugin.go` | Define authorities, strict configuration, lifecycle, and cleanup. |

The transformation is deterministic. A module is either recognized and
rewritten through a known ABI path, or returned unchanged.

## Test

Build the checked-in Emscripten fixtures with the pinned official toolchain:

```sh
./scripts/build-fixtures.sh
```

Run the hermetic suite and the optional historical Wago corpus:

```sh
go run ./cmd/corpusfetch
WAGO_CORPUS_DIR=/path/to/wago/bench/corpus go test -race -count=1 ./...
go vet ./...
```

Without `WAGO_CORPUS_DIR`, tests look for the sibling
`../wago-emscripten-base/bench/corpus` checkout and skip those historical
binaries when it is unavailable.

## Contributing

Keep new compatibility work capability-based: add a real workload with checked
output, pin external binaries and digests, reject unsupported glue explicitly,
and preserve the per-instance resource bounds. Please open an
[issue](https://github.com/JairusSW/wago-emscripten/issues) before adding an
ambient host capability.

## License

Apache-2.0. See [LICENSE](./LICENSE).

## Contact

Use [GitHub issues](https://github.com/JairusSW/wago-emscripten/issues) for bug
reports, compatibility requests, and corpus suggestions.
