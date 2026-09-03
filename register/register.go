// Package register exposes the standalone JavaScript ABI provider catalog.
package register

import (
	emscripten "github.com/JairusSW/wago-emscripten"
	"github.com/wago-org/wago"
)

func Provider() wago.PluginProvider { return emscripten.Provider() }

func Providers() []wago.PluginProvider { return []wago.PluginProvider{Provider()} }
