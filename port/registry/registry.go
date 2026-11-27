// Package registry is how a plugin adds itself to the engine: an adapter,
// a codec, a UI bundle or a command registers in init(), like a
// database/sql driver, and engine.yaml chooses it by type. The set of
// plugins is chosen when a binary is built (reasonr-build and its
// manifest).
package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// Deps are what the engine gives a factory.
type Deps struct {
	Logger  *slog.Logger
	Metrics port.Metrics
	// State is the main store (already created): a hub may use its
	// connections.
	State any
}

// Factories of adapters by their settings node (a section of engine.yaml).
type (
	// StoreFactory gives a store: a row store (port.RowStore and the other
	// byte-level ports it supports) the engine lays the model out for.
	StoreFactory func(n *port.Node, d Deps) (any, error)
	// HubFactory gives the link between instances of a notification hub.
	HubFactory         func(n *port.Node, d Deps) (port.HubLink, error)
	PublisherFactory   func(n *port.Node, d Deps) (port.Publisher, error)
	IntegrationFactory func(n *port.Node, d Deps) (port.IntegrationTransport, error)
	KMSFactory         func(n *port.Node, d Deps) (port.KMS, error)
)

// StoreTrait is a property of a store type that reasonrd check verifies
// without connecting to it.
type StoreTrait uint8

// PersonalData: the store encrypts personal data
// (port.Capabilities.Personal).
const PersonalData StoreTrait = 1

// Command is a reasonrd subcommand a module adds: it returns the exit code
// (0 success, 1 failure, 2 usage error).
type Command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var (
	mu  sync.RWMutex
	reg = Registrations{
		Stores:       map[string]StoreFactory{},
		StoreTraits:  map[string]StoreTrait{},
		Hubs:         map[string]HubFactory{},
		Publishers:   map[string]PublisherFactory{},
		Integrations: map[string]IntegrationFactory{},
		KMS:          map[string]KMSFactory{},
		Codecs:       map[string]port.Codec{},
		Commands:     map[string]Command{},
	}
)

func register[F any](m map[string]F, what, name string, f F) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := m[name]; dup {
		panic(fmt.Sprintf("registry: %s %q registered twice", what, name))
	}
	m[name] = f
}

// RegisterStore registers a store of type typ (stores.<name>.type) with the
// properties traits.
func RegisterStore(typ string, f StoreFactory, traits ...StoreTrait) {
	register(reg.Stores, "store", typ, f)
	mu.Lock()
	defer mu.Unlock()
	for _, t := range traits {
		reg.StoreTraits[typ] |= t
	}
}

// RegisterHub registers the link of a notification hub (notifications.hub.type).
func RegisterHub(typ string, f HubFactory) { register(reg.Hubs, "notification hub", typ, f) }

// RegisterPublisher registers a bus (bus.type).
func RegisterPublisher(typ string, f PublisherFactory) { register(reg.Publishers, "bus", typ, f) }

// RegisterIntegration registers a transport of external requests
// (integrations.<system>.type).
func RegisterIntegration(typ string, f IntegrationFactory) {
	register(reg.Integrations, "integration transport", typ, f)
}

// RegisterKMS registers a KMS of the personal data master key (crypto.kms.type).
func RegisterKMS(typ string, f KMSFactory) { register(reg.KMS, "KMS", typ, f) }

// RegisterCodec registers a codec by its name (codec: in engine.yaml) and
// media type (Content-Type and Accept).
func RegisterCodec(c port.Codec) { register(reg.Codecs, "codec", c.Name(), c) }

// RegisterCommand adds the subcommand name (an engine kind's administration).
func RegisterCommand(name string, run Command) { register(reg.Commands, "command", name, run) }

// RegisterUIBundle sets the web runtime build served at /ui/ when ingress.ui
// has neither dir nor proxy; nil drops it.
func RegisterUIBundle(b port.UIBundle) {
	mu.Lock()
	defer mu.Unlock()
	reg.UIBundle = b
}

// Secret reads a secret setting: key, or the environment variable named by
// key_env; "" — neither is set.
func Secret(n *port.Node, key string) (string, error) {
	v, err := n.String(key, "")
	if err != nil || v != "" {
		return v, err
	}
	env, err := n.String(key+"_env", "")
	if err != nil || env == "" {
		return "", err
	}
	v = os.Getenv(env)
	if v == "" {
		return "", fmt.Errorf("the environment variable %s is empty", env)
	}
	return v, nil
}

// Registrations are what the plugins of a binary registered, by type or
// name.
type Registrations struct {
	Stores       map[string]StoreFactory
	StoreTraits  map[string]StoreTrait
	Hubs         map[string]HubFactory
	Publishers   map[string]PublisherFactory
	Integrations map[string]IntegrationFactory
	KMS          map[string]KMSFactory
	Codecs       map[string]port.Codec
	Commands     map[string]Command
	UIBundle     port.UIBundle
}

// Registered returns a copy of the registrations. It is the engine's: the
// engine finds the plugins engine.yaml names in it; a plugin has no use
// for it.
func Registered() Registrations {
	mu.RLock()
	defer mu.RUnlock()
	return Registrations{
		Stores:       maps.Clone(reg.Stores),
		StoreTraits:  maps.Clone(reg.StoreTraits),
		Hubs:         maps.Clone(reg.Hubs),
		Publishers:   maps.Clone(reg.Publishers),
		Integrations: maps.Clone(reg.Integrations),
		KMS:          maps.Clone(reg.KMS),
		Codecs:       maps.Clone(reg.Codecs),
		Commands:     maps.Clone(reg.Commands),
		UIBundle:     reg.UIBundle,
	}
}
