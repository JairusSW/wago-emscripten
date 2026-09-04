// Package emscripten provides small host-ABI shims for standalone WebAssembly
// programs that were emitted for JavaScript rather than WASI alone.
package emscripten

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/wago-org/wago"
)

const (
	ID      = "github.com/JairusSW/wago-emscripten"
	Version = "0.4.0"
)

var configSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "stdin": {"type": "string", "enum": ["inherit", "eof"]},
    "stdout": {"type": "string", "enum": ["inherit", "discard"]},
    "stderr": {"type": "string", "enum": ["inherit", "discard"]},
    "env": {"type": "array", "maxItems": 256, "items": {"type": "string", "maxLength": 4096}},
    "maxFilesystemBytes": {"type": "integer", "minimum": 65536, "maximum": 268435456},
    "maxOpenFiles": {"type": "integer", "minimum": 3, "maximum": 65536}
  }
}`)

type pluginConfig struct {
	Stdin              string   `json:"stdin,omitempty"`
	Stdout             string   `json:"stdout,omitempty"`
	Stderr             string   `json:"stderr,omitempty"`
	Env                []string `json:"env,omitempty"`
	MaxFilesystemBytes uint64   `json:"maxFilesystemBytes,omitempty"`
	MaxOpenFiles       uint32   `json:"maxOpenFiles,omitempty"`
}

var Definition = wago.PluginDefinition{
	ID:          ID,
	Name:        "Emscripten compatibility runtime",
	Version:     Version,
	Description: "General standalone Emscripten compatibility, plus Go js/wasm and Ruby JS-ABI execution for Wago.",
	Stability:   wago.Experimental,
	Compatibility: wago.Compatibility{
		Engines:   map[string]string{"wago": ">=0.1.0", "go": ">=1.22"},
		Platforms: []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"},
	},
	Provenance: wago.PluginProvenance{
		Homepage:   "https://github.com/JairusSW/wago-emscripten#readme",
		Repository: "https://github.com/JairusSW/wago-emscripten",
		License:    "Apache-2.0",
		Authors:    []string{"JairusSW"},
	},
	Requires: []wago.PluginRequirement{
		{ID: "github.com/wago-org/wasi/p1", Version: "^0.2.1"},
		{ID: "github.com/wago-org/wasi/unstable", Version: "^0.2.1"},
	},
	Authorities: []wago.AuthorityRequest{
		{Name: wago.AuthorityHostImportDefine, Mode: wago.AuthorityRequired, Reason: "define the selected JavaScript-facing host ABIs", Scope: wago.AuthorityScope{Modules: []string{"env", "go", "rb-js-abi-host", "canonical_abi"}}},
		{Name: wago.AuthorityHostArgumentsRead, Mode: wago.AuthorityRequired, Reason: "construct argv for Go js/wasm standalone entry points"},
		{Name: wago.AuthorityHostCallerIdentify, Mode: wago.AuthorityRequired, Reason: "isolate compatibility state and files by guest instance"},
		{Name: wago.AuthorityHostCallerInvoke, Mode: wago.AuthorityRequired, Reason: "run Emscripten and Go js/wasm callbacks on the active guest"},
		{Name: wago.AuthorityInstanceCloseObserve, Mode: wago.AuthorityRequired, Reason: "release per-instance compatibility state and files"},
		{Name: wago.AuthorityModuleSourceTransform, Mode: wago.AuthorityRequired, Reason: "internalize Emscripten memory and add typed callbacks, heap growth, and standalone launchers"},
	},
	ConfigSchema: append(json.RawMessage(nil), configSchema...),
}

func Provider() wago.PluginProvider {
	return wago.PluginProvider{
		Definition: Definition,
		New:        func() wago.Plugin { return newPlugin() },
		ValidateConfig: func(raw json.RawMessage) error {
			var cfg pluginConfig
			return decodeConfig(raw, &cfg)
		},
	}
}

type plugin struct {
	mu                 sync.Mutex
	args               []string
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	callers            *wago.CallerResolver
	invoker            *wago.CallerInvoker
	states             map[wago.InstanceIdentity]*goState
	files              map[wago.InstanceIdentity]*fileSystem
	started            bool
	argView            *wago.GuestArgumentsAccess
	env                []string
	maxFilesystemBytes int64
	maxOpenFiles       int
}

func newPlugin() *plugin {
	return &plugin{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, states: make(map[wago.InstanceIdentity]*goState), files: make(map[wago.InstanceIdentity]*fileSystem), maxFilesystemBytes: 32 << 20, maxOpenFiles: 1024}
}

func decodeConfig(raw json.RawMessage, dst *pluginConfig) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > 65536 {
		return fmt.Errorf("emscripten: config exceeds 65536 bytes")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	for key := range probe {
		if key != "stdin" && key != "stdout" && key != "stderr" && key != "env" && key != "maxFilesystemBytes" && key != "maxOpenFiles" {
			return fmt.Errorf("emscripten: unknown config field %q", key)
		}
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return err
	}
	if dst.Stdin != "" && dst.Stdin != "inherit" && dst.Stdin != "eof" {
		return fmt.Errorf("emscripten: stdin must be inherit or eof")
	}
	for name, value := range map[string]string{"stdout": dst.Stdout, "stderr": dst.Stderr} {
		if value != "" && value != "inherit" && value != "discard" {
			return fmt.Errorf("emscripten: %s must be inherit or discard", name)
		}
	}
	if len(dst.Env) > 256 {
		return fmt.Errorf("emscripten: env has %d entries, max 256", len(dst.Env))
	}
	for _, entry := range dst.Env {
		if len(entry) > 4096 || bytes.IndexByte([]byte(entry), 0) >= 0 || bytes.IndexByte([]byte(entry), '=') <= 0 {
			return fmt.Errorf("emscripten: invalid env entry %q", entry)
		}
	}
	if dst.MaxFilesystemBytes != 0 && (dst.MaxFilesystemBytes < 65536 || dst.MaxFilesystemBytes > 256<<20) {
		return fmt.Errorf("emscripten: maxFilesystemBytes must be between 65536 and 268435456")
	}
	if dst.MaxOpenFiles != 0 && (dst.MaxOpenFiles < 3 || dst.MaxOpenFiles > 65536) {
		return fmt.Errorf("emscripten: maxOpenFiles must be between 3 and 65536")
	}
	return nil
}

func (p *plugin) Register(reg *wago.Registrar) error {
	var cfg pluginConfig
	if err := reg.Config(&cfg); err != nil {
		return err
	}
	if cfg.Stdout == "discard" {
		p.stdout = io.Discard
	}
	if cfg.Stdin == "eof" {
		p.stdin = bytes.NewReader(nil)
	}
	if cfg.Stderr == "discard" {
		p.stderr = io.Discard
	}
	if cfg.Env != nil {
		p.env = append([]string(nil), cfg.Env...)
	}
	if cfg.MaxFilesystemBytes != 0 {
		p.maxFilesystemBytes = int64(cfg.MaxFilesystemBytes)
	}
	if cfg.MaxOpenFiles != 0 {
		p.maxOpenFiles = int(cfg.MaxOpenFiles)
	}
	var err error
	p.argView, err = reg.GuestArguments()
	if err != nil {
		return err
	}
	p.callers, err = reg.HostCallers()
	if err != nil {
		return err
	}
	p.invoker, err = reg.HostCallerInvoker()
	if err != nil {
		return err
	}
	imports, err := reg.HostImports()
	if err != nil {
		return err
	}
	if err := p.registerEmscripten(imports); err != nil {
		return err
	}
	if err := p.registerGoJS(imports); err != nil {
		return err
	}
	if err := p.registerRuby(imports); err != nil {
		return err
	}
	transformer, err := reg.ModuleSourceTransformer()
	if err != nil {
		return err
	}
	if err := transformer.Transform(func(_ wago.ModuleSourceContext, source []byte) ([]byte, error) {
		p.mu.Lock()
		args := append([]string(nil), p.args...)
		env := append([]string(nil), p.env...)
		p.mu.Unlock()
		return transformModuleWithEnvironment(source, args, env)
	}); err != nil {
		return err
	}
	closed, err := reg.InstanceCloseObserver()
	if err != nil {
		return err
	}
	if err := closed.After(func(event wago.InstanceCloseEvent) {
		p.mu.Lock()
		delete(p.states, event.Instance)
		delete(p.files, event.Instance)
		p.mu.Unlock()
	}); err != nil {
		return err
	}
	return reg.Lifecycle(wago.PluginLifecycle{Start: p.start, Stop: p.stop})
}

func (p *plugin) start(_ context.Context) error {
	args, err := p.argView.Args()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.args = append([]string(nil), args...)
	p.started = true
	p.mu.Unlock()
	return nil
}

func (p *plugin) stop(context.Context) error {
	p.mu.Lock()
	clear(p.states)
	clear(p.files)
	p.started = false
	p.mu.Unlock()
	return nil
}
