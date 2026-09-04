package emscripten

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wago-org/wago"
	"github.com/wago-org/wasi/p1"
	"github.com/wago-org/wasi/unstable"
)

func TestCorpusLaunchers(t *testing.T) {
	corpus := os.Getenv("WAGO_CORPUS_DIR")
	if corpus == "" {
		corpus = filepath.Join("..", "wago-emscripten-base", "bench", "corpus")
	}
	if _, err := os.Stat(filepath.Join(corpus, "lua.wasm")); err != nil {
		t.Skipf("Wago corpus is unavailable at %s", corpus)
	}
	tests := []struct {
		name       string
		file       string
		args       []string
		stdin      string
		wasiStdin  string
		wantStdout string
		wantStderr string
		exitCode   int32
	}{
		{name: "regexmatch", file: "regexmatch.wasm", wantStdout: "regex:3000:99780"},
		{
			name: "wasm3", file: "wasm3.wasm", args: []string{"--repl"},
			wasiStdin:  fmt.Sprintf(":load-hex %d\n%x\n:invoke fib 25\n:exit\n", len(wasm3Workload), wasm3Workload),
			wantStderr: "Result: 75025",
		},
		{name: "lua", file: "lua.wasm"},
		{name: "sqlite", file: "sqlite3.wasm"},
		{name: "ruby", file: "ruby.wasm"},
		{name: "esbuild", file: "esbuild.wasm", args: []string{"--loader=js", "--minify"}, stdin: esbuildWorkload()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wasiStdout, wasiStderr := captureWASI(t, test.wasiStdin)
			var stdout bytes.Buffer
			provider := Provider()
			provider.New = func() wago.Plugin {
				plugin := newPlugin()
				plugin.stdin = strings.NewReader(test.stdin)
				plugin.stdout = &stdout
				return plugin
			}
			path := filepath.Join(corpus, test.file)
			runtimeArgs := append([]string{path}, test.args...)
			rt := wago.NewRuntime(wago.WithGuestArguments(runtimeArgs))
			t.Cleanup(func() { _ = rt.Close() })
			if err := rt.LoadPlugins(context.Background(), testPluginSet(t, provider)); err != nil {
				t.Fatalf("LoadPlugins: %v", err)
			}
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			module, err := rt.Compile(source)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			t.Cleanup(func() { _ = module.Close() })
			imports := module.Imports()
			for _, imp := range imports {
				if imp.Module == "env" && imp.Name == "memory" {
					t.Fatal("SQLite env.memory was not internalized")
				}
			}
			instance, err := rt.Instantiate(context.Background(), module)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			t.Cleanup(func() { _ = instance.Close() })
			_, err = instance.Call(context.Background(), "_start")
			var exit *wago.ExitError
			if err != nil && (!errors.As(err, &exit) || exit.Code != test.exitCode) {
				t.Fatalf("_start: %v", err)
			}
			if err == nil && test.exitCode != 0 {
				t.Fatalf("_start returned success, want exit code %d", test.exitCode)
			}
			if test.wantStdout != "" && !strings.Contains(wasiStdout(), test.wantStdout) {
				t.Fatalf("WASI stdout = %q, want substring %q", wasiStdout(), test.wantStdout)
			}
			if test.wantStderr != "" && !strings.Contains(wasiStderr(), test.wantStderr) {
				t.Fatalf("WASI stderr = %q, want substring %q", wasiStderr(), test.wantStderr)
			}
			switch test.name {
			case "lua":
				testLuaEvaluation(t, instance)
			case "sqlite":
				testSQLiteQuery(t, instance)
			case "ruby":
				testRubyEvaluation(t, instance)
			case "esbuild":
				got := stdout.String()
				if len(got) < 10_000 || len(got) >= len(test.stdin) || !strings.Contains(got, "function f999") {
					t.Fatalf("esbuild output did not contain the expected minified 1000-function program (input=%d, output=%d)", len(test.stdin), len(got))
				}
			}
		})
	}
}

func TestPinnedEmscriptenFixturesExecute(t *testing.T) {
	tests := []struct {
		name       string
		wantOutput string
		wantGrowth bool
	}{
		{name: "compute", wantOutput: "compute:2:6743105635951828498", wantGrowth: true},
		{name: "time", wantOutput: "time:2024-01-01 00:00:00"},
		{name: "filesystem", wantOutput: "filesystem:4227081039"},
		{name: "system", wantOutput: "system:fixture-argument:fixture-value:1:1:1"},
		{name: "cpp", wantOutput: "cpp:5236522040"},
		{name: "setjmp", wantOutput: "setjmp:73"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, _ := captureWASI(t, "")
			path := filepath.Join("testdata", "fixtures", test.name+".wasm")
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			rt := wago.NewRuntime(wago.WithGuestArguments([]string{path, "fixture-argument"}))
			t.Cleanup(func() { _ = rt.Close() })
			provider := Provider()
			provider.New = func() wago.Plugin {
				plugin := newPlugin()
				plugin.env = []string{"WAGO_EMSCRIPTEN_FIXTURE=fixture-value"}
				return plugin
			}
			if err := rt.LoadPlugins(context.Background(), testPluginSet(t, provider)); err != nil {
				t.Fatalf("LoadPlugins: %v", err)
			}
			module, err := rt.Compile(source)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			t.Cleanup(func() { _ = module.Close() })
			for _, imp := range module.Imports() {
				if imp.Module == "env" && imp.Name == "memory" {
					t.Fatal("generic Emscripten env.memory was not internalized")
				}
			}
			instance, err := rt.Instantiate(context.Background(), module)
			if err != nil {
				t.Fatalf("Instantiate: %v", err)
			}
			t.Cleanup(func() { _ = instance.Close() })
			initialMemory := len(instance.Memory().UnsafeBytes())
			if _, err := instance.Call(context.Background(), "_start"); err != nil {
				t.Fatalf("_start: %v", err)
			}
			if got := stdout(); !strings.Contains(got, test.wantOutput) {
				t.Fatalf("stdout = %q, want substring %q", got, test.wantOutput)
			}
			if test.wantGrowth && len(instance.Memory().UnsafeBytes()) <= initialMemory {
				t.Fatalf("memory did not grow beyond %d bytes", initialMemory)
			}
		})
	}
}

func TestEmscriptenFilesystemIsPerInstance(t *testing.T) {
	stdout, _ := captureWASI(t, "")
	path := filepath.Join("testdata", "fixtures", "filesystem.wasm")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rt := wago.NewRuntime(wago.WithGuestArguments([]string{path}))
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.LoadPlugins(context.Background(), testPluginSet(t)); err != nil {
		t.Fatal(err)
	}
	module, err := rt.Compile(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = module.Close() })
	for i := 0; i < 2; i++ {
		instance, err := rt.Instantiate(context.Background(), module)
		if err != nil {
			t.Fatalf("instance %d: %v", i, err)
		}
		if _, err := instance.Call(context.Background(), "_start"); err != nil {
			_ = instance.Close()
			t.Fatalf("instance %d _start: %v", i, err)
		}
		if err := instance.Close(); err != nil {
			t.Fatalf("instance %d close: %v", i, err)
		}
	}
	if got := strings.Count(stdout(), "filesystem:4227081039"); got != 2 {
		t.Fatalf("filesystem output count = %d, want 2", got)
	}
}

func TestDownloadedSQLiteCorpusExecutes(t *testing.T) {
	path := filepath.Join(".corpus", "sqlite-org.wasm")
	source, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("run `go run ./cmd/corpusfetch` to download the pinned upstream corpus")
	}
	if err != nil {
		t.Fatal(err)
	}
	captureWASI(t, "")
	rt := wago.NewRuntime(wago.WithGuestArguments([]string{path}))
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.LoadPlugins(context.Background(), testPluginSet(t)); err != nil {
		t.Fatalf("LoadPlugins: %v", err)
	}
	module, err := rt.Compile(source)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = module.Close() })
	instance, err := rt.Instantiate(context.Background(), module)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	if _, err := instance.Call(context.Background(), "_start"); err != nil {
		t.Fatalf("_start: %v", err)
	}
	testSQLiteQuery(t, instance)
}

func testLuaEvaluation(t *testing.T, instance *wago.Instance) {
	t.Helper()
	state := callOne(t, instance, "luaL_newstate")
	if state == 0 {
		t.Fatal("luaL_newstate returned null")
	}
	defer call(t, instance, "lua_close", state)
	call(t, instance, "luaL_openlibs", state)
	source := []byte(`
local values = {}
for i = 1, 5000 do values[i] = 5001 - i end
table.sort(values)
local sum = 0
for i = 1, #values do sum = (sum + values[i] * values[i]) % 1000000007 end
local payload = table.concat({string.rep("ab", 2048), tostring(sum)}, ":")
assert(#payload > 4096)
return sum
` + "\x00")
	pointer := callOne(t, instance, "malloc", uint64(len(source)))
	defer call(t, instance, "free", pointer)
	copy(instance.Memory().UnsafeBytes()[pointer:pointer+uint64(len(source))], source)
	if status := callOne(t, instance, "luaL_loadstring", state, pointer); status != 0 {
		t.Fatalf("luaL_loadstring = %d", status)
	}
	if status := callOne(t, instance, "lua_pcallk", state, 0, 1, 0, 0, 0); status != 0 {
		t.Fatalf("lua_pcallk = %d", status)
	}
	var want int64
	for i := int64(1); i <= 5000; i++ {
		want = (want + i*i) % 1000000007
	}
	if got := int64(callOne(t, instance, "lua_tointegerx", state, uint64(uint32(^uint32(0))), 0)); got != want {
		t.Fatalf("Lua result = %d, want %d", got, want)
	}
}

func testSQLiteQuery(t *testing.T, instance *wago.Instance) {
	t.Helper()
	if status := callOne(t, instance, "sqlite3_initialize"); status != 0 {
		t.Fatalf("sqlite3_initialize = %d", status)
	}
	memory := instance.Memory().UnsafeBytes()
	filename := putGuestString(t, instance, ":memory:")
	dbOut := callOne(t, instance, "malloc", 4)
	defer call(t, instance, "free", filename)
	defer call(t, instance, "free", dbOut)
	if status := callOne(t, instance, "sqlite3_open", filename, dbOut); status != 0 {
		t.Fatalf("sqlite3_open = %d", status)
	}
	db := uint64(binary.LittleEndian.Uint32(memory[dbOut : dbOut+4]))
	defer call(t, instance, "sqlite3_close_v2", db)
	execSQLite(t, instance, db, `
CREATE TABLE workload(id INTEGER PRIMARY KEY, payload TEXT NOT NULL);
WITH RECURSIVE seq(x) AS (
  VALUES(1) UNION ALL SELECT x + 1 FROM seq WHERE x < 5000
)
INSERT INTO workload SELECT x, printf('row-%05d', x) FROM seq;
CREATE INDEX workload_payload ON workload(payload);
`)
	if got := int32(callOne(t, instance, "sqlite3_total_changes", db)); got != 5000 {
		t.Fatalf("sqlite3_total_changes = %d, want 5000", got)
	}
	query := putGuestString(t, instance, "select count(*), sum(id) from workload where payload >= 'row-01000'")
	stmtOut := callOne(t, instance, "malloc", 4)
	tailOut := callOne(t, instance, "malloc", 4)
	defer call(t, instance, "free", query)
	defer call(t, instance, "free", stmtOut)
	defer call(t, instance, "free", tailOut)
	if status := callOne(t, instance, "sqlite3_prepare_v2", db, query, uint64(uint32(^uint32(0))), stmtOut, tailOut); status != 0 {
		t.Fatalf("sqlite3_prepare_v2 = %d", status)
	}
	stmt := uint64(binary.LittleEndian.Uint32(memory[stmtOut : stmtOut+4]))
	defer call(t, instance, "sqlite3_finalize", stmt)
	if status := callOne(t, instance, "sqlite3_step", stmt); status != 100 {
		t.Fatalf("sqlite3_step = %d, want SQLITE_ROW", status)
	}
	if got := int32(callOne(t, instance, "sqlite3_column_int", stmt, 0)); got != 4001 {
		t.Fatalf("SQLite count = %d, want 4001", got)
	}
	if got := int64(callOne(t, instance, "sqlite3_column_int64", stmt, 1)); got != 12003000 {
		t.Fatalf("SQLite sum = %d, want 12003000", got)
	}
}

func execSQLite(t *testing.T, instance *wago.Instance, db uint64, sql string) {
	t.Helper()
	pointer := putGuestString(t, instance, sql)
	defer call(t, instance, "free", pointer)
	if status := callOne(t, instance, "sqlite3_exec", db, pointer, 0, 0, 0); status != 0 {
		t.Fatalf("sqlite3_exec = %d", status)
	}
}

func testRubyEvaluation(t *testing.T, instance *wago.Instance) {
	t.Helper()
	call(t, instance, "ruby-init: func(args: list<string>) -> ()", 0, 0)
	source := []byte(`(1..2000).map { |n| n * n }.select(&:odd?).sum.to_s`)
	pointer := callOne(t, instance, "cabi_realloc", 0, 0, 1, uint64(len(source)))
	if pointer == 0 {
		t.Fatal("Ruby canonical allocator returned null")
	}
	copy(instance.Memory().UnsafeBytes()[pointer:pointer+uint64(len(source))], source)
	result := callOne(t, instance, "rb-eval-string-protect: func(str: string) -> tuple<handle<rb-abi-value>, s32>", pointer, uint64(len(source)))
	memory := instance.Memory().UnsafeBytes()
	if uint64(result)+8 > uint64(len(memory)) {
		t.Fatalf("Ruby result pointer %#x exceeds memory", result)
	}
	handle := uint64(binary.LittleEndian.Uint32(memory[result : result+4]))
	status := binary.LittleEndian.Uint32(memory[result+4 : result+8])
	if status != 0 || handle == 0 {
		t.Fatalf("Ruby evaluation = handle %#x, status %d", handle, status)
	}
	stringResult := callOne(t, instance, "rstring-ptr: func(value: handle<rb-abi-value>) -> string", handle)
	if uint64(stringResult)+8 > uint64(len(memory)) {
		t.Fatalf("Ruby string result pointer %#x exceeds memory", stringResult)
	}
	stringPointer := binary.LittleEndian.Uint32(memory[stringResult : stringResult+4])
	stringLength := binary.LittleEndian.Uint32(memory[stringResult+4 : stringResult+8])
	if uint64(stringPointer)+uint64(stringLength) > uint64(len(memory)) {
		t.Fatal("Ruby string exceeds memory")
	}
	if got := string(memory[stringPointer : stringPointer+stringLength]); got != "1333333000" {
		t.Fatalf("Ruby workload result = %q, want 1333333000", got)
	}
	call(t, instance, "canonical_abi_drop_rb-abi-value", handle)
}

func putGuestString(t *testing.T, instance *wago.Instance, value string) uint64 {
	t.Helper()
	bytes := append([]byte(value), 0)
	pointer := callOne(t, instance, "malloc", uint64(len(bytes)))
	copy(instance.Memory().UnsafeBytes()[pointer:pointer+uint64(len(bytes))], bytes)
	return pointer
}

func callOne(t *testing.T, instance *wago.Instance, export string, args ...uint64) uint64 {
	t.Helper()
	results := call(t, instance, export, args...)
	if len(results) != 1 {
		t.Fatalf("%s returned %d results", export, len(results))
	}
	return results[0]
}

func call(t *testing.T, instance *wago.Instance, export string, args ...uint64) []uint64 {
	t.Helper()
	results, err := instance.InvokeContext(context.Background(), export, args...)
	if err != nil {
		t.Fatalf("%s: %v", export, err)
	}
	return results
}

func TestUnrelatedSourceIsByteIdentical(t *testing.T) {
	source := append([]byte(nil), wasmHeader...)
	got, err := transformModule(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(source) {
		t.Fatal("unrelated module changed")
	}
}

func TestTransformIsDeterministic(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "fixtures", "compute.wasm"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := transformModule(source, []string{"program.wasm", "argument"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := transformModule(source, []string{"program.wasm", "argument"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent transforms produced different modules")
	}
}

func testPluginSet(t *testing.T, direct ...wago.PluginProvider) wago.PluginSet {
	t.Helper()
	provider := Provider()
	if len(direct) != 0 {
		provider = direct[0]
	}
	providers := []wago.PluginProvider{provider, p1.Provider(), unstable.Provider()}
	selections := make([]wago.PluginSelection, 0, len(providers))
	for i, provider := range providers {
		digest, err := wago.DefinitionDigest(provider.Definition)
		if err != nil {
			t.Fatal(err)
		}
		grants := make([]wago.AuthorityGrant, 0, len(provider.Definition.Authorities))
		for _, request := range provider.Definition.Authorities {
			grants = append(grants, wago.AuthorityGrant{Name: request.Name, Scope: request.Scope})
		}
		selection := wago.PluginSelection{
			ID: provider.Definition.ID, DefinitionDigest: digest, Direct: i == 0, Grants: grants,
			Dependencies: make(map[string]string, len(provider.Definition.Requires)),
		}
		for _, requirement := range provider.Definition.Requires {
			selection.Dependencies[requirement.ID] = requirement.Version
		}
		selections = append(selections, selection)
	}
	return wago.PluginSet{Providers: providers, Selections: selections}
}

func captureWASI(t *testing.T, input string) (func() string, func() string) {
	t.Helper()
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin")
	stdoutPath := filepath.Join(dir, "stdout")
	stderrPath := filepath.Join(dir, "stderr")
	if err := os.WriteFile(stdinPath, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(stdinPath)
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout, originalStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
	t.Cleanup(func() {
		os.Stdin, os.Stdout, os.Stderr = originalStdin, originalStdout, originalStderr
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
	})
	read := func(file *os.File, path string) func() string {
		return func() string {
			_ = file.Sync()
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return string(contents)
		}
	}
	return read(stdout, stdoutPath), read(stderr, stderrPath)
}

func esbuildWorkload() string {
	var source strings.Builder
	for i := 0; i < 1000; i++ {
		fmt.Fprintf(&source, "export function f%d(value) { const offset = %d; return value * %d + offset; }\n", i, i, i+1)
	}
	return source.String()
}

var wasm3Workload = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x09, 0x02, 0x60,
	0x01, 0x7f, 0x01, 0x7f, 0x60, 0x00, 0x00, 0x03, 0x03, 0x02, 0x00, 0x01,
	0x07, 0x10, 0x02, 0x03, 0x66, 0x69, 0x62, 0x00, 0x00, 0x06, 0x5f, 0x73,
	0x74, 0x61, 0x72, 0x74, 0x00, 0x01,
	0x0a, 0x2e, 0x02, 0x1c, 0x00, 0x20, 0x00, 0x41, 0x02, 0x48, 0x04, 0x7f,
	0x20, 0x00, 0x05, 0x20, 0x00, 0x41, 0x01, 0x6b, 0x10, 0x00, 0x20, 0x00,
	0x41, 0x02, 0x6b, 0x10, 0x00, 0x6a, 0x0b, 0x0b, 0x0f, 0x00, 0x41, 0x19,
	0x10, 0x00, 0x41, 0x91, 0xca, 0x04, 0x47, 0x04, 0x40, 0x00, 0x0b, 0x0b,
}

func TestDefinitionHasExactScopes(t *testing.T) {
	if Definition.ID != ID || Definition.Version != Version {
		t.Fatalf("definition identity = %s@%s", Definition.ID, Definition.Version)
	}
	joined := ""
	for _, authority := range Definition.Authorities {
		joined += string(authority.Name) + " " + strings.Join(authority.Scope.Modules, ",") + "\n"
	}
	for _, want := range []string{"host.import.define env,go,rb-js-abi-host,canonical_abi", "module.source.transform"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("authority inventory missing %q:\n%s", want, joined)
		}
	}
}

func TestDecodeConfig(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "empty", raw: `{}`},
		{name: "stdin inherit", raw: `{"stdin":"inherit"}`},
		{name: "stdin eof", raw: `{"stdin":"eof"}`},
		{name: "environment", raw: `{"env":["LANG=C.UTF-8","MODE=test"]}`},
		{name: "filesystem limits", raw: `{"maxFilesystemBytes":1048576,"maxOpenFiles":64}`},
		{name: "invalid environment", raw: `{"env":["missing-separator"]}`, wantErr: "invalid env entry"},
		{name: "invalid filesystem limit", raw: `{"maxFilesystemBytes":1024}`, wantErr: "maxFilesystemBytes must be between"},
		{name: "invalid stdin", raw: `{"stdin":"discard"}`, wantErr: "stdin must be inherit or eof"},
		{name: "unknown", raw: `{"network":"inherit"}`, wantErr: `unknown config field "network"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var cfg pluginConfig
			err := decodeConfig([]byte(test.raw), &cfg)
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("decodeConfig error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}
