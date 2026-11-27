// Package builtin registers the core adapters (without external
// dependencies): the in-memory store and hub, the HTTP transport, jwt and
// keys authentication. The package is imported for its init, like
// database/sql drivers.
package builtin

import (
	"cmp"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/appapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/integration"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/bus"
	_ "github.com/tycheg/reasonr-engine/internal/codecs" // the built-in json
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

func init() {
	registry.RegisterStore("memory", func(n *config.Node, d registry.Deps) (any, error) {
		if err := n.Only("type"); err != nil {
			return nil, err
		}
		if d.KMS != nil {
			return memory.NewStoreKMS(d.Catalog, d.KMS), nil
		}
		return memory.NewStore(d.Catalog), nil
	}, registry.PersonalData)
	registry.RegisterHub("memory", func(n *config.Node, _ registry.Deps) (port.HubLink, error) {
		return nil, n.Only("type")
	})
	registry.RegisterKMS("memory", func(n *config.Node, _ registry.Deps) (port.KMS, error) {
		if err := n.Only("type"); err != nil {
			return nil, err
		}
		return pii.NewMemoryKMS(), nil
	})
	registry.RegisterKMS("local", localKMS)
	registry.RegisterListener("http", httpListener)
	registry.RegisterAuth("jwt", jwtAuth)
	registry.RegisterAuth("keys", keysAuth)
	registry.RegisterIntegration("http", func(n *config.Node, d registry.Deps) (ports.Transport, error) {
		if err := n.Only("type", "url", "timeout", "codec"); err != nil {
			return nil, err
		}
		c, err := codecOf(n)
		if err != nil {
			return nil, err
		}
		u, err := n.String("url", "")
		if err != nil || u == "" {
			return nil, fmt.Errorf("integrations http: set url")
		}
		timeout, err := n.Duration("timeout", 10*time.Second)
		if err != nil {
			return nil, err
		}
		// A connection per concurrent send: by default two per host stay
		// idle, and the other sends would open new ones.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxIdleConns, tr.MaxIdleConnsPerHost = 0, cmp.Or(d.Workers, 64)
		return &integration.HTTP{URL: strings.TrimRight(u, "/"), Signer: d.Signer, Catalog: d.Catalog, Codec: c,
			Client: &http.Client{Timeout: timeout, Transport: tr}}, nil
	})
	registry.RegisterIntegration("fake", func(n *config.Node, d registry.Deps) (ports.Transport, error) {
		if err := n.Only("type"); err != nil {
			return nil, err
		}
		if d.Fakes == nil { // C0010
			return nil, fmt.Errorf("the fake transport is only for reasonrd --test")
		}
		return d.Fakes, nil
	})
	registry.RegisterPublisher("log", func(n *config.Node, d registry.Deps) (port.Publisher, error) {
		if err := n.Only("type", "notifications"); err != nil {
			return nil, err
		}
		return &logPublisher{log: d.Logger}, nil
	})
	registry.RegisterPublisher("memory", func(n *config.Node, d registry.Deps) (port.Publisher, error) {
		if err := n.Only("type", "notifications"); err != nil {
			return nil, err
		}
		return &memory.Bus{}, nil
	})
}

// logPublisher is a bus for debugging: messages are written to the log.
type logPublisher struct{ log *slog.Logger }

func (p *logPublisher) Publish(_ context.Context, msgs []port.Message) error {
	for i := range msgs {
		m := &msgs[i]
		p.log.Info("bus", "channel", m.Channel(), "id", bus.ID(m), "subject", m.Subject, "data", string(m.Data))
	}
	return nil
}

func httpListener(n *config.Node, d registry.Deps) (ports.Listener, error) {
	if err := n.Only("type", "listen", "codec", "codecs", "log", "ui", "mcp"); err != nil {
		return nil, err
	}
	addr, err := n.String("listen", ":8080")
	if err != nil {
		return nil, err
	}
	opts := httpapi.Options{Addr: addr, Metrics: d.MetricsHandler, AllPublic: d.Test, Issuers: d.Issuers}
	// The codecs of the model API by Content-Type and Accept (plan 0021);
	// json always. codec names one.
	if name, err := n.String("codec", "json"); err != nil {
		return nil, err
	} else if name != "json" {
		c, err := registry.Codec(name)
		if err != nil {
			return nil, fmt.Errorf("line %d: codec: %w", n.Get("codec").Line, err)
		}
		opts.Codecs = append(opts.Codecs, c)
	}
	if cs := n.Get("codecs"); cs != nil {
		if cs.Kind != config.List {
			return nil, fmt.Errorf("line %d: codecs: a list of codec names", cs.Line)
		}
		for _, item := range cs.Values {
			c, err := registry.Codec(item.Value)
			if err != nil {
				return nil, fmt.Errorf("line %d: codecs: %w", item.Line, err)
			}
			opts.Codecs = append(opts.Codecs, c)
		}
	}
	// The scenario clock offset — in test mode and for demo tenants (RFC §27.5).
	opts.TimeOffset = func(t port.Tenant) bool { return d.Test || d.Demo[string(t)] }
	// The MCP server at /mcp (RFC §31.3.2): by ingress.mcp, always in test mode.
	if opts.MCP, err = n.Bool("mcp", d.Test); err != nil {
		return nil, err
	}
	if l := n.Get("log"); l != nil {
		if err := l.Only("requests"); err != nil {
			return nil, err
		}
		if opts.LogRequests, err = l.Bool("requests", false); err != nil {
			return nil, err
		}
	}
	for _, m := range d.Delivery {
		opts.LongPoll = opts.LongPoll || m == "longpoll"
	}
	opts.Mount = map[string]http.Handler{}
	if d.TestAPI != nil {
		opts.Mount["/_test/"] = d.TestAPI
	}
	if d.Signer != nil { // the public keys of the engine's JWT for the integration service
		jwks := d.Signer.JWKS()
		opts.Mount["GET /.well-known/jwks.json"] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/jwk-set+json")
			w.Header().Set("Cache-Control", "max-age=300")
			_, _ = w.Write(jwks)
		})
	}
	if d.UI != nil {
		ui, err := uiOptions(n.Get("ui"), d)
		if err != nil {
			return nil, err
		}
		var once sync.Once
		var app *appapi.Server
		handler := func(x kind.Executor, deps ports.EdgeDeps) http.Handler {
			once.Do(func() { app = appapi.New(x, deps, ui) })
			return app.Handler()
		}
		opts.Handlers = map[string]func(kind.Executor, ports.EdgeDeps) http.Handler{"/app/": handler, "/ui/": handler}
	}
	return &httpapi.Listener{Options: opts}, nil
}

// uiOptions reads ingress[].ui: the runtime build directory and OIDC
// sign-in (RFC §22).
func uiOptions(n *config.Node, d registry.Deps) (appapi.Options, error) {
	opts := appapi.Options{Catalog: d.UI, Test: d.Test}
	if n == nil {
		return opts, nil
	}
	if err := n.Only("dir", "proxy", "auth"); err != nil {
		return opts, err
	}
	var err error
	if opts.Dir, err = n.String("dir", ""); err != nil {
		return opts, err
	}
	if opts.Proxy, err = n.String("proxy", ""); err != nil {
		return opts, err
	}
	a := n.Get("auth")
	if a == nil {
		return opts, nil
	}
	if err := a.Only("oidc"); err != nil {
		return opts, err
	}
	o := a.Get("oidc")
	if o == nil {
		return opts, nil
	}
	if err := o.Only("issuer", "client_id", "scopes"); err != nil {
		return opts, err
	}
	oidc := &appapi.OIDC{}
	if oidc.Issuer, err = o.String("issuer", ""); err != nil {
		return opts, err
	}
	if oidc.ClientID, err = o.String("client_id", ""); err != nil {
		return opts, err
	}
	if oidc.Scopes, err = o.Strings("scopes"); err != nil {
		return opts, err
	}
	if oidc.Issuer == "" || oidc.ClientID == "" {
		return opts, fmt.Errorf("ui.auth.oidc: issuer and client_id are needed")
	}
	opts.OIDC = oidc
	return opts, nil
}

// secret reads the value key or the environment variable key_env.
// localKMS is a master key from the configuration: key, key_env or
// key_file (32 bytes in base64).
func localKMS(n *config.Node, d registry.Deps) (port.KMS, error) {
	if err := n.Only("type", "key", "key_env", "key_file"); err != nil {
		return nil, err
	}
	v, err := secret(n, "key")
	if err != nil {
		return nil, fmt.Errorf("crypto.kms local: %w", err)
	}
	if v == "" {
		file, err := n.String("key_file", "")
		if err != nil {
			return nil, err
		}
		if file == "" {
			return nil, fmt.Errorf("crypto.kms local: set key, key_env or key_file")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("crypto.kms local: %w", err)
		}
		v = string(data)
	}
	key, err := pii.ParseKey(v)
	if err != nil {
		return nil, fmt.Errorf("crypto.kms local: %w", err)
	}
	if !d.Test {
		d.Logger.Warn("the personal data master key is stored locally (crypto.kms local): use a KMS in production")
	}
	return pii.NewLocalKMS(key)
}

func secret(n *config.Node, key string) (string, error) { return registry.Secret(n, key) }

func claim(n *config.Node, key, def string) (string, error) {
	if c := n.Get(key); c != nil {
		if err := c.Only("claim", "type", "default"); err != nil {
			return "", err
		}
		return c.String("claim", def)
	}
	return def, nil
}

func jwtAuth(n *config.Node, d registry.Deps) (string, ports.Authenticator, error) {
	if err := n.Only("type", "secret", "secret_env", "public_key", "jwks", "issuer", "audience", "leeway",
		"tenant", "actor", "roles"); err != nil {
		return "", nil, err
	}
	j := &auth.JWT{Catalog: d.Catalog}
	var err error
	if j.Issuer, err = n.String("issuer", ""); err != nil {
		return "", nil, err
	}
	if j.Audience, err = n.String("audience", ""); err != nil {
		return "", nil, err
	}
	if j.Leeway, err = n.Duration("leeway", 0); err != nil {
		return "", nil, err
	}
	sec, err := secret(n, "secret")
	if err != nil {
		return "", nil, fmt.Errorf("jwt: %w", err)
	}
	pemPath, _ := n.String("public_key", "")
	jwks, _ := n.String("jwks", "")
	switch {
	case sec != "":
		j.Keys = auth.StaticKey{K: []byte(sec)}
	case pemPath != "":
		data, err := os.ReadFile(pemPath)
		if err != nil {
			return "", nil, fmt.Errorf("jwt: %w", err)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return "", nil, fmt.Errorf("jwt: %s is not PEM", pemPath)
		}
		key, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return "", nil, fmt.Errorf("jwt: %s: %w", pemPath, err)
		}
		j.Keys = auth.StaticKey{K: key}
	case jwks != "":
		j.Keys = &auth.JWKS{URL: jwks}
	default:
		return "", nil, fmt.Errorf("jwt: set secret, secret_env, public_key or jwks")
	}
	if t := n.Get("tenant"); t != nil {
		if err := t.Only("claim", "default", "any"); err != nil {
			return "", nil, err
		}
		if j.Claims.Tenant, err = t.String("claim", ""); err != nil {
			return "", nil, err
		}
		def, _ := t.String("default", "")
		j.Claims.DefaultTenant = port.Tenant(def)
		// any is the tenant of an external system token for every tenant (RFC §18).
		if j.Claims.AnyTenant, err = t.String("any", ""); err != nil {
			return "", nil, err
		}
		if j.Claims.AnyTenant != "" && j.Claims.Tenant == "" {
			return "", nil, fmt.Errorf("jwt: tenant.any without tenant.claim")
		}
	}
	if j.Claims.Actor, err = claim(n, "actor", "sub"); err != nil {
		return "", nil, err
	}
	if a := n.Get("actor"); a != nil {
		if typ := a.Get("type"); typ != nil && typ.Kind == config.Map {
			if j.Claims.ActorType, err = typ.String("claim", ""); err != nil {
				return "", nil, err
			}
		} else if j.Claims.FixedActor, err = a.String("type", ""); err != nil {
			return "", nil, err
		}
	}
	if j.Claims.Roles, err = claim(n, "roles", "roles"); err != nil {
		return "", nil, err
	}
	return "Bearer", j, nil
}

func keysAuth(n *config.Node, d registry.Deps) (string, ports.Authenticator, error) {
	if err := n.Only("type", "keys"); err != nil {
		return "", nil, err
	}
	ks := &auth.APIKeys{Catalog: d.Catalog}
	list := n.Get("keys")
	if list == nil || list.Kind != config.List {
		return "", nil, fmt.Errorf("keys: a list of keys is needed")
	}
	for _, k := range list.Values {
		if err := k.Only("key", "key_env", "tenant", "roles", "actor"); err != nil {
			return "", nil, err
		}
		key, err := secret(k, "key")
		if err != nil || key == "" {
			return "", nil, fmt.Errorf("keys: set key or key_env: %v", err)
		}
		id := auth.Identity{}
		t, _ := k.String("tenant", "")
		id.Tenant = port.Tenant(t)
		if id.Roles, err = k.Strings("roles"); err != nil {
			return "", nil, err
		}
		if a := k.Get("actor"); a != nil {
			if err := a.Only("type", "id"); err != nil {
				return "", nil, err
			}
			id.ActorType, _ = a.String("type", "")
			id.ActorID, _ = a.String("id", "")
		}
		ks.Add(key, id)
	}
	return "ApiKey", ks, nil
}

// codecOf returns the codec a component names in codec (plan 0021,
// decision 6); json without it.
func codecOf(n *config.Node) (port.Codec, error) {
	name, err := n.String("codec", "json")
	if err != nil {
		return nil, err
	}
	c, err := registry.Codec(name)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", n.Line, err)
	}
	return c, nil
}
