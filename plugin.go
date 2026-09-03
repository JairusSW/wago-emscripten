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
	Version = "0.2.1"
)

var configSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "stdin": {"type": "string", "enum": ["inherit", "eof"]},
    "stdout": {"type": "string", "enum": ["inherit", "discard"]},
    "stderr": {"type": "string", "enum": ["inherit", "discard"]}
  }
}`)

type pluginConfig struct {
	Stdin  string `json:"stdin,omitempty"`
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

var Definition = wago.PluginDefinition{
	ID:          ID,
	Name:        "Standalone JavaScript ABI compatibility",
	Version:     Version,
	Description: "Barebones Emscripten, Go js/wasm, and Ruby JS-ABI compatibility for standalone Wago execution.",
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
		{Name: wago.AuthorityHostCallerIdentify, Mode: wago.AuthorityRequired, Reason: "isolate Go js/wasm value tables by guest instance"},
		{Name: wago.AuthorityHostCallerInvoke, Mode: wago.AuthorityRequired, Reason: "run Emscripten and Go js/wasm callbacks on the active guest"},
		{Name: wago.AuthorityInstanceCloseObserve, Mode: wago.AuthorityRequired, Reason: "release per-instance Go js/wasm value tables"},
		{Name: wago.AuthorityModuleSourceTransform, Mode: wago.AuthorityRequired, Reason: "internalize Emscripten imported memory and add narrow standalone launchers"},
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
	mu      sync.Mutex
	args    []string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	callers *wago.CallerResolver
	invoker *wago.CallerInvoker
	states  map[wago.InstanceIdentity]*goState
	started bool
	argView *wago.GuestArgumentsAccess
}

func newPlugin() *plugin {
	return &plugin{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, states: make(map[wago.InstanceIdentity]*goState)}
}

func decodeConfig(raw json.RawMessage, dst *pluginConfig) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > 4096 {
		return fmt.Errorf("emscripten: config exceeds 4096 bytes")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	for key := range probe {
		if key != "stdin" && key != "stdout" && key != "stderr" {
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
		p.mu.Unlock()
		return transformModule(source, args)
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
	p.started = false
	p.mu.Unlock()
	return nil
}
