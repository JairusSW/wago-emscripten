# Wago standalone JavaScript ABI compatibility

`wago-emscripten` runs command-style WebAssembly modules that were linked for a
small JavaScript host instead of pure WASI. It targets standalone binaries and
deliberately does not emulate a browser, DOM, Node.js, or a general JavaScript
engine.

The first release is driven by Wago's execution corpus:

| Corpus | Standalone path verified by tests |
|---|---|
| `lua.wasm` | Emscripten initialization and callback re-entry; evaluates `return 6 * 7` through the exported Lua C API. |
| `sqlite3.wasm` | Internalizes imported `env.memory`, initializes SQLite, and executes `select 40 + 2` in memory. |
| `ruby.wasm` | Initializes the Ruby VM and invokes its version entry point. |
| `esbuild.wasm` | Supplies the Go `js/wasm` ABI, argv, bounded JS values, and synchronous filesystem callbacks; `--version` prints `0.21.5`. |
| `regexmatch.wasm` | Executes through the required WASI Preview 1 provider. |
| `wasm3.wasm` | Executes through the required legacy `wasi_unstable` provider and reaches its usage path. |

## Install and run

Once version `v0.1.0` is published:

```sh
wago plugin add github.com/JairusSW/wago-emscripten@v0.1.0 --global --allow-all --no-input
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

Stdout and stderr default to `inherit` and may independently be set to
`discard` with plugin configuration.

## Development

The corpus tests use `WAGO_CORPUS_DIR` when set, otherwise the sibling
`../wago-emscripten-base/bench/corpus` checkout:

```sh
WAGO_CORPUS_DIR=/path/to/wago/bench/corpus go test -race -count=1 ./...
go vet ./...
```

Licensed under Apache-2.0.
