// Package kindreg holds what package kind registers — the engine kinds,
// the test drivers and how a command opens a tool engine — for the
// engine's registry to read. The values are kind's types; this leaf cannot
// name them, since kind imports it.
package kindreg

import (
	"fmt"
	"sync"
)

var (
	mu         sync.RWMutex
	kinds      = map[string]any{}
	drivers    = map[string]any{}
	toolOpener any
)

func register(m map[string]any, what, name string, v any) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := m[name]; dup {
		panic(fmt.Sprintf("registry: %s %q registered twice", what, name))
	}
	m[name] = v
}

// RegisterKind and RegisterDriver add an engine kind and a test driver.
func RegisterKind(name string, k any)   { register(kinds, "engine kind", name, k) }
func RegisterDriver(name string, d any) { register(drivers, "test driver", name, d) }

// Kinds returns the registered engine kinds by name.
func Kinds() map[string]any {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]any, len(kinds))
	for k, v := range kinds {
		out[k] = v
	}
	return out
}

// Driver returns a registered test driver; nil — none.
func Driver(name string) any {
	mu.RLock()
	defer mu.RUnlock()
	return drivers[name]
}

// SetToolOpener sets how commands open tool engines (the server does);
// ToolOpener returns it.
func SetToolOpener(f any) {
	mu.Lock()
	defer mu.Unlock()
	toolOpener = f
}

func ToolOpener() any {
	mu.RLock()
	defer mu.RUnlock()
	return toolOpener
}
