#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
image='emscripten/emsdk@sha256:27bc6267cb285223b8aebb7627bfebae7cb3ad2aaa0d5923b8aa5321793033e8'

docker run --rm \
  -v "$repo_dir:/src" \
  -w /src \
  "$image" \
  bash -lc 'for name in compute time; do
    emcc "testdata/fixtures/$name.c" -O2 \
      -sWASM=1 \
      -sIMPORTED_MEMORY=1 \
      -sALLOW_MEMORY_GROWTH=1 \
      -sINITIAL_MEMORY=4MB \
      -sEXPORTED_FUNCTIONS=[_main] \
      -sEXIT_RUNTIME=1 \
      -o "testdata/fixtures/$name.js"
    rm "testdata/fixtures/$name.js"
    chmod 0644 "testdata/fixtures/$name.wasm"
  done'
