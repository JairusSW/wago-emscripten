package emscripten

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
		name     string
		file     string
		args     []string
		exitCode int32
	}{
		{name: "regexmatch", file: "regexmatch.wasm"},
		{name: "wasm3", file: "wasm3.wasm", exitCode: 1},
		{name: "lua", file: "lua.wasm"},
		{name: "sqlite", file: "sqlite3.wasm"},
		{name: "ruby", file: "ruby.wasm"},
		{name: "esbuild", file: "esbuild.wasm", args: []string{"--version"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			provider := Provider()
			provider.New = func() wago.Plugin {
				plugin := newPlugin()
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
			switch test.name {
			case "lua":
				testLuaEvaluation(t, instance)
			case "sqlite":
				testSQLiteQuery(t, instance)
			case "esbuild":
				if got := strings.TrimSpace(stdout.String()); got != "0.21.5" {
					t.Fatalf("esbuild --version output = %q", got)
				}
			}
		})
	}
}

func testLuaEvaluation(t *testing.T, instance *wago.Instance) {
	t.Helper()
	state := callOne(t, instance, "luaL_newstate")
	if state == 0 {
		t.Fatal("luaL_newstate returned null")
	}
	defer call(t, instance, "lua_close", state)
	call(t, instance, "luaL_openlibs", state)
	source := []byte("return 6 * 7\x00")
	pointer := callOne(t, instance, "malloc", uint64(len(source)))
	defer call(t, instance, "free", pointer)
	copy(instance.Memory().UnsafeBytes()[pointer:pointer+uint64(len(source))], source)
	if status := callOne(t, instance, "luaL_loadstring", state, pointer); status != 0 {
		t.Fatalf("luaL_loadstring = %d", status)
	}
	if status := callOne(t, instance, "lua_pcallk", state, 0, 1, 0, 0, 0); status != 0 {
		t.Fatalf("lua_pcallk = %d", status)
	}
	if got := int64(callOne(t, instance, "lua_tointegerx", state, uint64(uint32(^uint32(0))), 0)); got != 42 {
		t.Fatalf("Lua result = %d, want 42", got)
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
	query := putGuestString(t, instance, "select 40 + 2")
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
	if got := int32(callOne(t, instance, "sqlite3_column_int", stmt, 0)); got != 42 {
		t.Fatalf("SQLite result = %d, want 42", got)
	}
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
