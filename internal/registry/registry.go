// Package registry is the engine's registry: the factories by type from
// engine.yaml (RFC §30–31), the engine kinds, codecs, test drivers, the UI
// bundle and the commands of modules, and finding them. Plugins register
// through the registry of the contract (port/registry: its Deps are the
// part of Deps an adapter of another module gets), engine kinds and test
// drivers through package kind; the built-in adapters register here with
// the full Deps.
package registry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/tycheg/reasonr-engine/internal/kindreg"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
	plugin "github.com/tycheg/reasonr-engine/port/registry"
)

// Deps are the shared dependencies of factories.
type Deps struct {
	Catalog kind.Catalog
	Logger  *slog.Logger
	Metrics port.Metrics
	// MetricsHandler serves the metrics on the HTTP transport (/metrics);
	// nil — do not serve.
	MetricsHandler http.Handler
	// Delivery holds the notification delivery methods (websocket, longpoll).
	Delivery []string
	// Test is the server's test mode (reasonrd --test).
	Test bool
	// Demo holds the tenants with demo: true (RFC §27.5).
	Demo map[string]bool
	// Issuers are the token issuers from auth (issuer of jwt): the
	// authorization servers in the MCP protected resource metadata.
	Issuers []string
	// State is the main store (already created): a hub may use its
	// connections.
	State any
	// TestAPI is the /_test/* handler of test mode; nil — not test mode.
	TestAPI http.Handler
	// KMS is the personal data master key (crypto.kms; memory in --test
	// without crypto); nil — not set: the store refuses on the first write
	// of personal data.
	KMS port.KMS
	// Signer is the engine's JWT signer for the integration service
	// (integrations.auth; in --test — a key created at startup); nil — not
	// set: requests go without Authorization, no JWKS is served.
	Signer ports.RequestSigner
	// Fakes is the fake integration transport of test mode (RFC §18); nil
	// — not test mode.
	Fakes ports.Transport
	// Workers is integrations.workers: concurrent sends of external
	// requests (0 — 64); the transport keeps that many connections.
	Workers int
	// UI holds the model interfaces for the app API (RFC §22); nil — no
	// interface.
	UI ports.UICatalog
}

// Component factories. A store returns a value implementing the ports it
// supports: the byte-level ports of a plugin's store, or UnitStore,
// ProjectionStore, TimerSource, EffectSource, EventStore of a built-in one.
type (
	StoreFactory func(n *port.Node, d Deps) (any, error)
	// HubFactory gives the link between instances of a notification hub;
	// nil — the instance is alone (memory).
	HubFactory      func(n *port.Node, d Deps) (port.HubLink, error)
	AuthFactory     func(n *port.Node, d Deps) (scheme string, a ports.Authenticator, err error)
	ListenerFactory func(n *port.Node, d Deps) (ports.Listener, error)
	// IntegrationFactory gives a transport to the integration service, and
	// PublisherFactory a bus.
	IntegrationFactory func(n *port.Node, d Deps) (ports.Transport, error)
	PublisherFactory   func(n *port.Node, d Deps) (port.Publisher, error)
	// KMSFactory creates the KMS of the personal data master key (crypto.kms).
	KMSFactory func(n *port.Node, d Deps) (port.KMS, error)
)

var (
	mu           sync.RWMutex
	stores       = map[string]StoreFactory{}
	storeTraits  = map[string]StoreTrait{}
	hubs         = map[string]HubFactory{}
	auths        = map[string]AuthFactory{}
	listeners    = map[string]ListenerFactory{}
	integrations = map[string]IntegrationFactory{}
	publishers   = map[string]PublisherFactory{}
	kmses        = map[string]KMSFactory{}
	codecs       = map[string]port.Codec{}
)

func register[F any](m map[string]F, what, typ string, f F) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := m[typ]; dup {
		panic(fmt.Sprintf("registry: %s %q registered twice", what, typ))
	}
	m[typ] = f
}

// lookup finds a factory of type typ among the built-in ones (own) and
// those of plugins (plugins, converted by conv).
func lookup[F, P any](own map[string]F, plugins map[string]P, conv func(P) F, what, typ string) (F, error) {
	mu.RLock()
	f, ok := own[typ]
	known := make([]string, 0, len(own)+len(plugins))
	for k := range own {
		known = append(known, k)
	}
	mu.RUnlock()
	p, isPlugin := plugins[typ]
	switch {
	case ok && isPlugin:
		return f, fmt.Errorf("%s %q is both built in and registered by a plugin", what, typ)
	case ok:
		return f, nil
	case isPlugin:
		return conv(p), nil
	}
	for k := range plugins {
		known = append(known, k)
	}
	sort.Strings(known)
	return f, fmt.Errorf("%s %q is not in the build (available: %s); build the engine with the plugin in its manifest (reasonr-build)",
		what, typ, strings.Join(known, ", "))
}

func pluginDeps(d Deps) plugin.Deps {
	return plugin.Deps{Logger: d.Logger, Metrics: d.Metrics, State: d.State}
}

// StoreTrait is a property of a store type that reasonrd check verifies
// without connecting to it.
type StoreTrait = plugin.StoreTrait

// PersonalData: the store encrypts personal data
// (port.Capabilities.Personal).
const PersonalData = plugin.PersonalData

// RegisterStore registers a store of type typ with the properties traits.
func RegisterStore(typ string, f StoreFactory, traits ...StoreTrait) {
	register(stores, "store", typ, f)
	mu.Lock()
	for _, t := range traits {
		storeTraits[typ] |= t
	}
	mu.Unlock()
}

// StoreHas reports whether the store type has the property t; known ==
// false — the type is not in the build.
func StoreHas(typ string, t StoreTrait) (has, known bool) {
	r := plugin.Registered()
	mu.RLock()
	defer mu.RUnlock()
	_, own := stores[typ]
	_, other := r.Stores[typ]
	return (storeTraits[typ]|r.StoreTraits[typ])&t != 0, own || other
}

// RegisterHub registers a notification hub.
func RegisterHub(typ string, f HubFactory) { register(hubs, "notification hub", typ, f) }

// RegisterAuth registers an authenticator.
func RegisterAuth(typ string, f AuthFactory) { register(auths, "authentication", typ, f) }

// RegisterListener registers an incoming transport.
func RegisterListener(typ string, f ListenerFactory) { register(listeners, "transport", typ, f) }

// RegisterIntegration registers a transport to the integration service.
func RegisterIntegration(typ string, f IntegrationFactory) {
	register(integrations, "integration transport", typ, f)
}

// RegisterPublisher registers an event bus.
func RegisterPublisher(typ string, f PublisherFactory) { register(publishers, "bus", typ, f) }

// RegisterKMS registers a KMS.
func RegisterKMS(typ string, f KMSFactory) { register(kmses, "KMS", typ, f) }

// RegisterCodec registers a serialization format by its name (plan 0021,
// decision 6); json is built in.
func RegisterCodec(c port.Codec) { register(codecs, "codec", c.Name(), c) }

// Store returns a store factory.
func Store(typ string) (StoreFactory, error) {
	return lookup(stores, plugin.Registered().Stores, func(f plugin.StoreFactory) StoreFactory {
		return func(n *port.Node, d Deps) (any, error) { return f(n, pluginDeps(d)) }
	}, "store", typ)
}

// Hub returns a hub factory.
func Hub(typ string) (HubFactory, error) {
	return lookup(hubs, plugin.Registered().Hubs, func(f plugin.HubFactory) HubFactory {
		return func(n *port.Node, d Deps) (port.HubLink, error) { return f(n, pluginDeps(d)) }
	}, "notification hub", typ)
}

// Auth returns an authenticator factory.
func Auth(typ string) (AuthFactory, error) {
	return lookup(auths, map[string]AuthFactory(nil), nil, "authentication", typ)
}

// Listener returns a transport factory.
func Listener(typ string) (ListenerFactory, error) {
	return lookup(listeners, map[string]ListenerFactory(nil), nil, "transport", typ)
}

// Integration returns an integration transport factory: a plugin's
// transport gets the request ready (ports.Plugin).
func Integration(typ string) (IntegrationFactory, error) {
	return lookup(integrations, plugin.Registered().Integrations, func(f plugin.IntegrationFactory) IntegrationFactory {
		return func(n *port.Node, d Deps) (ports.Transport, error) {
			t, err := f(n, pluginDeps(d))
			if err != nil || t == nil {
				return nil, err
			}
			return ports.Plugin{IntegrationTransport: t}, nil
		}
	}, "integration transport", typ)
}

// Publisher returns a bus factory.
func Publisher(typ string) (PublisherFactory, error) {
	return lookup(publishers, plugin.Registered().Publishers, func(f plugin.PublisherFactory) PublisherFactory {
		return func(n *port.Node, d Deps) (port.Publisher, error) { return f(n, pluginDeps(d)) }
	}, "bus", typ)
}

// KMS returns a KMS factory.
func KMS(typ string) (KMSFactory, error) {
	return lookup(kmses, plugin.Registered().KMS, func(f plugin.KMSFactory) KMSFactory {
		return func(n *port.Node, d Deps) (port.KMS, error) { return f(n, pluginDeps(d)) }
	}, "KMS", typ)
}

// Codec returns a registered codec.
func Codec(name string) (port.Codec, error) {
	return lookup(codecs, plugin.Registered().Codecs, func(c port.Codec) port.Codec { return c }, "codec", name)
}

// CodecByType returns the registered codec of the media type ct
// (Content-Type, an Accept item) without parameters; false — none.
func CodecByType(ct string) (port.Codec, bool) {
	ct, _, _ = strings.Cut(ct, ";")
	ct = strings.TrimSpace(strings.ToLower(ct))
	mu.RLock()
	all := make([]port.Codec, 0, len(codecs))
	for _, c := range codecs {
		all = append(all, c)
	}
	mu.RUnlock()
	for _, c := range plugin.Registered().Codecs {
		all = append(all, c)
	}
	for _, c := range all {
		if c.ContentType() == ct {
			return c, true
		}
	}
	return nil, false
}

// Kind returns a registered engine kind; "" is stateless.
func Kind(name string) (kind.Kind, error) {
	if name == "" {
		name = "stateless"
	}
	all := kindreg.Kinds()
	if k, ok := all[name].(kind.Kind); ok {
		return k, nil
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("engine %q is not linked into this binary (available: %s)", name, strings.Join(names, ", "))
}

// Command is a reasonrd subcommand a module adds (plugin.Command).
type Command = plugin.Command

// Commands returns the subcommands plugins registered, by name.
func Commands() map[string]Command { return plugin.Registered().Commands }

// SetToolOpener sets how commands open tool engines (the server does): a
// non-zero code means the message is already printed to stderr.
func SetToolOpener(f func(ctx context.Context, name, configPath, model string, test bool, stderr io.Writer) (*kind.Tool, int)) {
	kindreg.SetToolOpener(f)
}

// Secret reads a secret setting: key, or the environment variable named
// by key_env; "" — neither is set.
func Secret(n *port.Node, key string) (string, error) { return plugin.Secret(n, key) }

// Driver returns a registered test run driver; nil — the binary does not
// link it.
func Driver(name string) kind.TestDriver {
	d, _ := kindreg.Driver(name).(kind.TestDriver)
	return d
}

// UIBundle returns the web runtime build a plugin registered; nil — none.
func UIBundle() port.UIBundle { return plugin.Registered().UIBundle }
