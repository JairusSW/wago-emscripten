# Wago standalone JavaScript ABI compatibility

`wago-emscripten` runs command-style WebAssembly modules that were linked for a
small JavaScript host instead of pure WASI. It targets standalone binaries and
deliberately does not emulate a browser, DOM, Node.js, or a general JavaScript
engine.

The plugin is driven by Wago's execution corpus:

| Corpus | Standalone path verified by tests |
|---|---|
| `lua.wasm` | Initializes Lua, opens its standard libraries, sorts 5,000 values, performs arithmetic and string allocation, and checks the exact result through the exported C API. |
| `sqlite3.wasm` | Internalizes imported `env.memory`, creates an in-memory table and index, inserts 5,000 rows with a recursive CTE, and checks an aggregate query result. |
| `ruby.wasm` | Initializes Ruby, evaluates a 2,000-element Enumerable workload, and reads back the exact result through Ruby's canonical ABI. |
| `esbuild.wasm` | Supplies the Go `js/wasm` ABI and stdin callbacks, then parses and minifies a generated 1,000-function JavaScript module. |
| `regexmatch.wasm` | Runs 3,000 regex iterations through WASI Preview 1 and checks the workload's output checksum. |
| `wasm3.wasm` | Uses the `wasi_unstable` REPL to parse and load a Wasm module from hex, then checks recursive `fib(25) = 75025`. |

## Install and run

The GitHub release is available now. Registry installation is pending
[Wago PR #558](https://github.com/wago-org/wago/pull/558) and
[registry PR #67](https://github.com/wago-org/plugins/pull/67):

```sh
wago plugin add github.com/JairusSW/wago-emscripten@0.2.0 --global --allow-all --no-input
wago run lua.wasm
wago run sqlite3.wasm
wago run ruby.wasm
wago run esbuild.wasm -- --version
```

The plugin requests narrowly scoped host-import, guest-argument, active-caller,
instance-close, and source-transform authorities. It also requires Wago's WASI
Preview 1 and `wasi_unstable` providers.

## Boundaries

This is a compatibility shim for recognizable standalone toolchains, not a
complete Emscripten or Node runtime. Unknown modules are left byte-for-byte
unchanged. Recognized modules get only the transformations required to add a
command entry point, internalize SQLite's imported memory, or add a typed Lua
callback trampoline.

Unsupported Emscripten filesystem syscalls fail with `ENOSYS`. SQLite's
in-memory engine works, but its Emscripten filesystem bridge is not implemented.
Ruby's JavaScript reflection/evaluation ABI is stubbed, so Ruby initialization
works while programs requiring JS interop do not. The Go host model provides
the command functionality esbuild needs; browser APIs, arbitrary Node modules,
and durable asynchronous timers are outside scope.

Stdin, stdout, and stderr default to `inherit`. Stdin may be set to `eof`, and
stdout or stderr may independently be set to `discard`, with plugin
configuration.

## Development

The corpus tests use `WAGO_CORPUS_DIR` when set, otherwise the sibling
`../wago-emscripten-base/bench/corpus` checkout:

```sh
WAGO_CORPUS_DIR=/path/to/wago/bench/corpus go test -race -count=1 ./...
go vet ./...
```

Licensed under Apache-2.0.
