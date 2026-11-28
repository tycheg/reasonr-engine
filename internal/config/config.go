package config

import (
	"cmp"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config is the engine configuration. Component settings are nodes: the
// adapter factories read them by their type.
type Config struct {
	// Engine is the engine kind; stateless by default. The kinds are
	// registered by their modules (registry.RegisterKind).
	Engine string
	// Settings is the section named after an engine kind other than
	// stateless (for example, a kind named acme reads acme: …); the
	// kind's module parses it.
	Settings *Node
	// Plugins are the binary plugins (.so) the binary loads before it reads
	// the rest (plan 0024), relative to the configuration file.
	Plugins []string
	// Stores are the stores by name; each has a type and adapter settings.
	Stores map[string]*Node
	// State is the store of entities, timers, the outbox, uniqueness.
	State string
	// Projections is the projection store: "default" and by projection
	// name. A projection in the State store is synchronous, in another one asynchronous.
	Projections map[string]string
	Events      string // the event store; "" — not stored
	// Features is the store of the feature flag state and journal (RFC
	// §23.4); State by default.
	Features string
	Bus      *Node // the bus: all events from the event store (RFC §31)
	// BusCodec is the codec of bus message bodies (bus.codec, plan 0021):
	// json by default.
	BusCodec string
	// BusNotifications is bus.notifications: publish notifications to the
	// bus without delivery guarantees.
	BusNotifications bool
	Integrations     map[string]*Node // the transport to the integration service: default and per external system
	// RequestWorkers is integrations.workers: concurrent sends of external
	// requests; 0 — the default (64).
	RequestWorkers int
	Hub            *Node    // the notification hub (notifications.hub)
	Delivery       []string // notification delivery to other actors: websocket, longpoll
	Ingress        []*Node  // incoming transports
	Auth           []*Node  // authenticators

	// RequestRetry is integrations.retry: the retry strategy of external
	// requests.
	RequestRetry Retry
	// IntegrationsAuth is integrations.auth: the engine's JWT signing key
	// for the integration service; nil — not set (in --test a key is
	// created at startup).
	IntegrationsAuth *Node
	// Contexts are the model's bounded contexts this process serves (RFC
	// §23.2): their commands, queries and ACL reactions to events of other
	// bounded contexts; nil — all. The processes share the main store:
	// events of other bounded contexts come from the outbox.
	Contexts []string
	// Tenants maps a data tenant to a model: base (no extension) or an extension name.
	Tenants map[string]string
	// Demo holds the tenants with demo: true (RFC §27.5): demo data, the
	// scenario clock offset (Reasonr-Time-Offset), test sign-in and fakes of
	// external systems.
	Demo    map[string]bool
	Logs    Logs
	Metrics *Node // telemetry.metrics
	Traces  *Node // telemetry.traces
	Limits  Limits
	Crypto  Crypto
	// Root is the parsed file: line numbers for diagnostics (nil for Default).
	Root *Node
}

// Logs are the log settings.
type Logs struct {
	Format string // text, json
	Level  string // debug, info, warn, error
}

// Crypto configures personal data (RFC §20).
type Crypto struct {
	KMS       *Node         // crypto.kms; nil — not set (KMS memory in --test)
	Subjects  int           // crypto.cache.subjects: the size of the subject key cache; 0 — the default
	TTL       time.Duration // crypto.cache.ttl: how long a subject key stays in the cache; 0 — the default
	ForgetLog string        // crypto.forget_log: the store of the forget journal; "" — not kept
}

// Limits are the kernel limits.
type Limits struct {
	Retries         int
	ConsistencyWait time.Duration
	IdempotencyTTL  time.Duration
}

// Retry is the retry strategy of external requests (integrations.retry);
// zero fields mean the defaults.
type Retry struct {
	Initial     time.Duration
	Multiplier  float64
	MaxInterval time.Duration
	MaxElapsed  time.Duration // only for requests without a deadline timer
}

// Default is the configuration without a file: everything in memory, HTTP
// on :8080, tenant default with the base model.
func Default() *Config {
	return &Config{
		Engine:      "stateless",
		Stores:      map[string]*Node{"main": typeNode("memory")},
		State:       "main",
		Features:    "main",
		Projections: map[string]string{"default": "main"},
		Hub:         typeNode("memory"),
		Delivery:    []string{"websocket"},
		Ingress:     []*Node{{Kind: Map, Keys: []string{"type", "listen"}, Values: []*Node{scalar("http"), scalar(":8080")}}},
		Tenants:     map[string]string{"default": "base"},
	}
}

// HTTP returns the first type: http ingress or nil.
func (c *Config) HTTP() *Node {
	for _, n := range c.Ingress {
		if Type(n) == "http" {
			return n
		}
	}
	return nil
}

// Listen sets the http ingress address (reasonrd --listen): of the first
// type: http ingress.
func (c *Config) Listen(addr string) error {
	for _, n := range c.Ingress {
		if Type(n) == "http" {
			n.Set("listen", scalar(addr))
			return nil
		}
	}
	return errors.New("the ingress has no type: http entry")
}

func scalar(s string) *Node { return &Node{Kind: Scalar, Value: s} }

func typeNode(t string) *Node {
	return &Node{Kind: Map, Keys: []string{"type"}, Values: []*Node{scalar(t)}}
}

// Load reads the configuration from a file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Decode parses and checks the configuration: unknown keys, store
// references, required fields. Missing sections are as in Default.
func Decode(data []byte) (*Config, error) {
	root, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if root.Kind != Map {
		return nil, errAt(root.Line, "expected a map of sections")
	}
	c := Default()
	c.Root = root
	if c.Engine, err = root.String("engine", "stateless"); err != nil {
		return nil, err
	}
	keys := []string{"engine", "stores", "state", "projections", "events", "bus", "integrations", "notifications",
		"ingress", "auth", "tenants", "telemetry", "limits", "crypto", "ui", "features", "contexts", "plugins"}
	if c.Engine != "stateless" {
		if slices.Contains(keys, c.Engine) {
			return nil, errAt(root.Get("engine").Line, "engine %q: the name of a configuration section", c.Engine)
		}
		keys = append(keys, c.Engine)
		c.Settings = root.Get(c.Engine)
		if c.Settings == nil {
			c.Settings = &Node{Kind: Map}
		}
		if c.Settings.Kind != Map {
			return nil, errAt(c.Settings.Line, "%s: expected a map of settings", c.Engine)
		}
	}
	if err := root.Only(keys...); err != nil {
		return nil, err
	}
	if c.Contexts, err = root.Strings("contexts"); err != nil {
		return nil, err
	}
	if c.Plugins, err = root.Strings("plugins"); err != nil {
		return nil, err
	}
	if s := root.Get("stores"); s != nil {
		if s.Kind != Map {
			return nil, errAt(s.Line, "stores: expected a map name → store")
		}
		c.Stores = map[string]*Node{}
		for i, name := range s.Keys {
			if err := needType(s.Values[i], "stores."+name); err != nil {
				return nil, err
			}
			c.Stores[name] = s.Values[i]
		}
		c.State = ""
	}
	if c.State, err = root.String("state", c.State); err != nil {
		return nil, err
	}
	if c.State == "" {
		if len(c.Stores) != 1 {
			return nil, errAt(root.Line, "state: set the main store")
		}
		for name := range c.Stores {
			c.State = name
		}
	}
	if err := c.store(root, "state", c.State); err != nil {
		return nil, err
	}
	c.Projections = map[string]string{"default": c.State}
	if p := root.Get("projections"); p != nil {
		if p.Kind != Map {
			return nil, errAt(p.Line, "projections: expected a map projection → store")
		}
		for i, name := range p.Keys {
			v := p.Values[i]
			if v.Kind != Scalar {
				return nil, errAt(v.Line, "projections.%s: expected a store name", name)
			}
			if err := c.store(p, name, v.Value); err != nil {
				return nil, err
			}
			c.Projections[name] = v.Value
		}
	}
	if c.Features, err = root.String("features", c.State); err != nil {
		return nil, err
	}
	if err := c.store(root, "features", c.Features); err != nil {
		return nil, err
	}
	if c.Events, err = root.String("events", ""); err != nil {
		return nil, err
	}
	if c.Events != "" {
		if err := c.store(root, "events", c.Events); err != nil {
			return nil, err
		}
	}
	if c.Bus = root.Get("bus"); c.Bus != nil {
		if p := c.Bus.Get("publish"); p != nil {
			return nil, errAt(p.Line, "bus.publish is no longer supported: every event from the event store (events) is published to the bus; remove bus.publish")
		}
		if c.BusNotifications, err = c.Bus.Bool("notifications", false); err != nil {
			return nil, err
		}
		// The codec of the message bodies is the engine's: the bus adapter
		// gets the messages encoded.
		if c.BusCodec, err = c.Bus.String("codec", "json"); err != nil {
			return nil, err
		}
		c.Bus.Delete("codec")
	}
	if err := c.crypto(root); err != nil {
		return nil, err
	}
	if in := root.Get("integrations"); in != nil {
		if in.Kind != Map {
			return nil, errAt(in.Line, "integrations: expected a map system → transport, and workers")
		}
		c.Integrations = map[string]*Node{}
		if c.RequestWorkers, err = in.Int("workers", 0); err != nil {
			return nil, err
		}
		if c.RequestWorkers < 0 {
			return nil, errAt(in.Get("workers").Line, "integrations.workers: expected a positive number")
		}
		if r := in.Get("retry"); r != nil {
			if err := c.retry(r); err != nil {
				return nil, err
			}
		}
		if a := in.Get("auth"); a != nil {
			if a.Kind != Map {
				return nil, errAt(a.Line, "integrations.auth: expected a map: key_file or key_env, kid, issuer, also")
			}
			if err := a.Only("key_file", "key_env", "kid", "issuer", "also"); err != nil {
				return nil, err
			}
			c.IntegrationsAuth = a
		}
		for i, name := range in.Keys {
			if name == "workers" || name == "auth" || name == "retry" {
				continue
			}
			if err := needType(in.Values[i], "integrations."+name); err != nil {
				return nil, err
			}
			c.Integrations[name] = in.Values[i]
		}
	}
	if n := root.Get("notifications"); n != nil {
		if err := n.Only("hub", "delivery"); err != nil {
			return nil, err
		}
		if h := n.Get("hub"); h != nil {
			if err := needType(h, "notifications.hub"); err != nil {
				return nil, err
			}
			c.Hub = h
		}
		if d, err := n.Strings("delivery"); err != nil {
			return nil, err
		} else if d != nil {
			c.Delivery = d
		}
		for _, d := range c.Delivery {
			if d != "websocket" && d != "longpoll" {
				return nil, errAt(n.Get("delivery").Line, "delivery %q is not supported (websocket, longpoll)", d)
			}
		}
	}
	if c.Ingress, err = items(root, "ingress", c.Ingress); err != nil {
		return nil, err
	}
	if c.Auth, err = items(root, "auth", nil); err != nil {
		return nil, err
	}
	if t := root.Get("tenants"); t != nil {
		if t.Kind != Map {
			return nil, errAt(t.Line, "tenants: expected a map tenant → {extension: …, demo: true}")
		}
		c.Tenants = map[string]string{}
		for i, name := range t.Keys {
			v := t.Values[i]
			if err := v.Only("extension", "demo"); err != nil {
				return nil, err
			}
			ext, err := v.String("extension", "")
			if err != nil {
				return nil, err
			}
			demo, err := v.Bool("demo", false)
			if err != nil {
				return nil, err
			}
			if demo {
				if c.Demo == nil {
					c.Demo = map[string]bool{}
				}
				c.Demo[name] = true
			}
			if ext == "base" {
				return nil, errAt(v.Line, "tenants.%s: base is not an extension; do not write extension for the base model", name)
			}
			c.Tenants[name] = cmp.Or(ext, "base")
		}
	}
	if t := root.Get("telemetry"); t != nil {
		if err := t.Only("logs", "metrics", "traces"); err != nil {
			return nil, err
		}
		if l := t.Get("logs"); l != nil {
			if err := l.Only("format", "level"); err != nil {
				return nil, err
			}
			if c.Logs.Format, err = l.String("format", ""); err != nil {
				return nil, err
			}
			if c.Logs.Level, err = l.String("level", ""); err != nil {
				return nil, err
			}
		}
		c.Metrics, c.Traces = t.Get("metrics"), t.Get("traces")
	}
	if l := root.Get("limits"); l != nil {
		if err := l.Only("retries", "consistency_wait", "idempotency_ttl"); err != nil {
			return nil, err
		}
		if c.Limits.Retries, err = l.Int("retries", 0); err != nil {
			return nil, err
		}
		if c.Limits.ConsistencyWait, err = l.Duration("consistency_wait", 0); err != nil {
			return nil, err
		}
		if c.Limits.IdempotencyTTL, err = l.Duration("idempotency_ttl", 0); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// retry reads integrations.retry: { initial, multiplier, max_interval,
// max_elapsed }.
func (c *Config) retry(r *Node) error {
	if r.Kind != Map {
		return errAt(r.Line, "integrations.retry: expected a map: initial, multiplier, max_interval, max_elapsed")
	}
	if err := r.Only("initial", "multiplier", "max_interval", "max_elapsed"); err != nil {
		return err
	}
	var err error
	rt := &c.RequestRetry
	for _, d := range []struct {
		key string
		to  *time.Duration
	}{{"initial", &rt.Initial}, {"max_interval", &rt.MaxInterval}, {"max_elapsed", &rt.MaxElapsed}} {
		if *d.to, err = r.Duration(d.key, 0); err != nil {
			return err
		}
		if *d.to < 0 {
			return errAt(r.Get(d.key).Line, "integrations.retry.%s: expected a positive duration", d.key)
		}
	}
	if m := r.Get("multiplier"); m != nil {
		if rt.Multiplier, err = strconv.ParseFloat(m.Value, 64); err != nil || rt.Multiplier < 1 {
			return errAt(m.Line, "integrations.retry.multiplier: expected a number of at least 1")
		}
	}
	return nil
}

// store checks a store reference.
func (c *Config) store(n *Node, key, name string) error {
	if _, ok := c.Stores[name]; !ok {
		line := 0
		if v := n.Get(key); v != nil {
			line = v.Line
		}
		return errAt(line, "%s: no store %q in stores", key, name)
	}
	return nil
}

func needType(n *Node, what string) error {
	if n.Kind != Map {
		return errAt(n.Line, "%s: expected a map with type", what)
	}
	if t, _ := n.String("type", ""); t == "" {
		return errAt(n.Line, "%s: no type", what)
	}
	return nil
}

// items reads a section that is a list of components (or one component).
func items(root *Node, key string, def []*Node) ([]*Node, error) {
	n := root.Get(key)
	switch {
	case n == nil:
		return def, nil
	case n.Kind == Map:
		return []*Node{n}, needType(n, key)
	case n.Kind != List:
		return nil, errAt(n.Line, "%s: expected a list of components", key)
	}
	for i, x := range n.Values {
		if err := needType(x, key+"["+strconv.Itoa(i)+"]"); err != nil {
			return nil, err
		}
	}
	return n.Values, nil
}

// Type returns the component type.
func Type(n *Node) string {
	t, _ := n.String("type", "")
	return t
}

// Line returns the line number of the node at the key path; 0 — no such node.
func (c *Config) Line(keys ...string) int {
	n := c.Root
	line := 0
	for _, k := range keys {
		if n = n.Get(k); n == nil {
			return line
		}
		line = n.Line
	}
	return line
}

func (c *Config) crypto(root *Node) error {
	n := root.Get("crypto")
	if n == nil {
		return nil
	}
	if err := n.Only("kms", "cache", "forget_log"); err != nil {
		return err
	}
	if k := n.Get("kms"); k != nil {
		if err := needType(k, "crypto.kms"); err != nil {
			return err
		}
		c.Crypto.KMS = k
	}
	if cache := n.Get("cache"); cache != nil {
		if err := cache.Only("subjects", "ttl"); err != nil {
			return err
		}
		var err error
		if c.Crypto.Subjects, err = cache.Int("subjects", 0); err != nil {
			return err
		}
		if c.Crypto.TTL, err = cache.Duration("ttl", 0); err != nil {
			return err
		}
	}
	var err error
	if c.Crypto.ForgetLog, err = n.String("forget_log", ""); err != nil || c.Crypto.ForgetLog == "" {
		return err
	}
	return c.store(n, "forget_log", c.Crypto.ForgetLog)
}

// Projection returns the store of the projection name (the model's full
// name: Ordering.OrderMart) and the projections key that set it: the full
// or the short name (OrderMart); explicit == false — the default store.
func (c *Config) Projection(name string) (store, key string, explicit bool) {
	short := name[strings.LastIndex(name, ".")+1:]
	for _, k := range []string{name, short} {
		if s, ok := c.Projections[k]; ok {
			return s, k, true
		}
	}
	return c.Projections["default"], "default", false
}

// KnownProjection reports whether a projections key names a projection of
// names: by the full or the short name.
func KnownProjection(key string, names map[string]bool) bool {
	if names[key] {
		return true
	}
	for n := range names {
		if n[strings.LastIndex(n, ".")+1:] == key {
			return true
		}
	}
	return false
}
