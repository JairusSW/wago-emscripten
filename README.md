# Wago Emscripten compatibility

`wago-emscripten` is a small compatibility runtime for command-style
WebAssembly produced by Emscripten and other JavaScript-oriented toolchains. It
lets standalone-capable binaries run directly under Wago without embedding a
browser, Node.js, or a JavaScript engine.

```sh
wago plugin add github.com/JairusSW/wago-emscripten@0.3.0 --global --allow-all --no-input
wago run program.wasm -- argument
```

The package recognizes capabilities rather than a fixed application list. For
Emscripten modules it can internalize imported memory, grow the heap through a
guest trampoline, launch `main` or `__main_argc_argv`, service common typed
`invoke_*` callbacks, provide monotonic and wall clocks, convert UTC broken-down
time, copy large memory ranges, and fail unsupported filesystem operations
closed. It also retains the Go `js/wasm` and Ruby JS-ABI paths used by esbuild
and the Ruby corpus.

## Executed workloads

Tests check outputs and API results after substantial work; successful
instantiation alone is not considered support.

| Binary | Work performed |
|---|---|
| Official `@sqlite.org/sqlite-wasm` | Creates an in-memory database and index, inserts 5,000 rows with a recursive CTE, and verifies an aggregate query. |
| Emscripten C compute fixture | Allocates more than the initial heap, sorts 2.5 million integers with `qsort`, and verifies output after forced `memory.grow`. |
| Emscripten C time fixture | Converts a fixed Unix timestamp with `gmtime`, formats it with `strftime`, and checks the exact UTC output. |
| Wago `lua.wasm` | Opens Lua libraries, sorts 5,000 values, performs arithmetic and string allocation, and reads the exact result through the C API. |
| Wago `sqlite3.wasm` | Creates and indexes a 5,000-row database and checks an aggregate query. |
| Wago `ruby.wasm` | Evaluates a 2,000-element Enumerable workload and reads its result through the canonical ABI. |
| Wago `esbuild.wasm` | Parses and minifies a generated 1,000-function JavaScript module through the Go `js/wasm` ABI. |
| Wago `regexmatch.wasm` | Runs 3,000 regex iterations and checks the workload checksum. |
| Wago `wasm3.wasm` | Loads a Wasm module into the interpreter and verifies recursive `fib(25) = 75025`. |

The checked-in fixtures are reproducible with a pinned official Emscripten
container:

```sh
./scripts/build-fixtures.sh
```

## Upstream corpus

`corpus.lock.json` pins archive and extracted-Wasm SHA-256 digests. The fetcher
extracts only the named regular file, validates the Wasm magic, bounds download
and output sizes, and atomically installs it under the ignored `.corpus/`
directory.

```sh
go run ./cmd/corpusfetch          # quick CI set
go run ./cmd/corpusfetch -full    # all nine pinned binaries
go test -count=1 ./...
```

| Corpus | Version | Result |
|---|---:|---|
| `@sqlite.org/sqlite-wasm` | `3.46.1-build5` | Executes the database workload on arm64 and amd64. |
| `@sqlite.org/sqlite-wasm` | `3.53.0-build1` | Tracked gap: `CREATE TABLE` traps in Wago's amd64 backend; it passes on arm64. |
| `sql.js` | `1.14.2` | Minified JS import/export names; generated glue is required. |
| `@foxglove/wasm-zstd` | `1.0.1` | Minified JS imports; generated glue is required. |
| `@imagemagick/magick-wasm` | `0.0.43` | Large minified JS ABI; generated glue is required. |
| `@ffmpeg/core` | `0.12.10` | Large minified JS ABI; generated glue is required. |
| `@duckdb/duckdb-wasm` MVP | `1.33.1-dev57.0` | Embind, emval, and DuckDB browser filesystem APIs require generated glue. |
| `@duckdb/duckdb-wasm` EH | `1.33.1-dev57.0` | Same JS API boundary with Wasm exception handling. |
| `brotli-wasm` | `3.0.1` | wasm-bindgen control sample; correctly left untouched. |

The glue-required entries are negative capability tests. The plugin deliberately
leaves them byte-for-byte unchanged instead of guessing the meaning of minified
imports such as `a.a` or pretending that Embind is a standalone ABI.

## Boundaries

This is not a browser or a general JavaScript runtime. Modules that depend on
DOM, Node APIs, Embind/emval, application-specific JavaScript callbacks, or
minified glue contracts still need their generated JavaScript.

Filesystem syscalls currently return `ENOSYS`; in-memory SQLite works, but a
host filesystem requires an explicit authority and mapping design. Subprocess
execution is denied. Local time is deterministic UTC. Unknown and known
glue-dependent modules are left byte-for-byte unchanged.

The plugin requires Wago's WASI Preview 1 and `wasi_unstable` providers. Its
stdin, stdout, and stderr default to `inherit`; stdin may be `eof`, and stdout
or stderr may independently be `discard`.

## Development

The historical Wago corpus tests use `WAGO_CORPUS_DIR`, otherwise they look for
the sibling `../wago-emscripten-base/bench/corpus` checkout:

```sh
go run ./cmd/corpusfetch
WAGO_CORPUS_DIR=/path/to/wago/bench/corpus go test -race -count=1 ./...
go vet ./...
```

Licensed under Apache-2.0.
