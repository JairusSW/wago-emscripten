#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd "$(dirname "$0")/.." && pwd)
image='emscripten/emsdk@sha256:27bc6267cb285223b8aebb7627bfebae7cb3ad2aaa0d5923b8aa5321793033e8'

docker run --rm \
  -v "$repo_dir:/src" \
  -w /src \
  "$image" \
  bash -lc 'set -euo pipefail
  for name in compute time filesystem system; do
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
  done
  emcc testdata/fixtures/setjmp.c -O2 \
    -sWASM=1 -sIMPORTED_MEMORY=1 -sALLOW_MEMORY_GROWTH=1 -sINITIAL_MEMORY=4MB \
    -sEXPORTED_FUNCTIONS=[_main] -sEXIT_RUNTIME=1 -sSUPPORT_LONGJMP=emscripten \
    -o testdata/fixtures/setjmp.js
  em++ testdata/fixtures/cpp.cpp -O2 -fno-exceptions \
    -sWASM=1 -sIMPORTED_MEMORY=1 -sALLOW_MEMORY_GROWTH=1 -sINITIAL_MEMORY=4MB \
    -sEXPORTED_FUNCTIONS=[_main] -sEXIT_RUNTIME=1 \
    -o testdata/fixtures/cpp.js
  rm testdata/fixtures/setjmp.js testdata/fixtures/cpp.js
  chmod 0644 testdata/fixtures/setjmp.wasm testdata/fixtures/cpp.wasm'
