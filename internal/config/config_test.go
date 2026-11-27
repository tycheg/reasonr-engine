package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestFull(t *testing.T) {
	data, err := os.ReadFile("testdata/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	dsn, _ := c.Stores["main"].String("dsn", "")
	urls, _ := c.Stores["search"].Strings("urls")
	if c.State != "main" || dsn != "postgres://app@db/main" || len(urls) != 1 || urls[0] != "https://os:9200" {
		t.Errorf("stores: %q %q %v", c.State, dsn, urls)
	}
	if c.Projections["OrderMart"] != "search" || c.Projections["default"] != "main" || c.Events != "analytics" {
		t.Errorf("projections: %v, events %q", c.Projections, c.Events)
	}
	if Type(c.Bus) != "kafka" || !c.BusNotifications {
		t.Errorf("bus: %v", c.Bus)
	}
	if Type(c.Integrations["Matching"]) != "grpc" || len(c.Integrations) != 2 || c.RequestWorkers != 32 || c.IntegrationsAuth.Get("kid").Value != "2026-10" || Type(c.Hub) != "postgres" || len(c.Delivery) != 2 {
		t.Errorf("integrations, hub, delivery")
	}
	if len(c.Ingress) != 2 || Type(c.Ingress[1]) != "grpc" {
		t.Fatalf("ingress: %d", len(c.Ingress))
	}
	if r, _ := c.Ingress[0].Get("log").Bool("requests", false); !r {
		t.Error("ingress[0].log.requests")
	}
	if l, _ := c.Ingress[1].String("listen", ""); l != ":9090" {
		t.Errorf("ingress[1].listen %q", l)
	}
	claim, _ := c.Auth[0].Get("actor").Get("type").String("claim", "")
	if len(c.Auth) != 1 || Type(c.Auth[0]) != "jwt" || claim != "actor_type" {
		t.Errorf("auth: %q", claim)
	}
	if c.Tenants["acme-staging"] != "acme" || c.Tenants["default"] != "base" || len(c.Tenants) != 3 {
		t.Errorf("tenants: %v", c.Tenants)
	}
	if r := c.RequestRetry; r.Initial != 500*time.Millisecond || r.Multiplier != 1.5 || r.MaxInterval != 30*time.Second || r.MaxElapsed != 2*time.Hour {
		t.Errorf("integrations.retry: %+v", r)
	}
	if c.Logs.Format != "json" || Type(c.Metrics) != "prometheus" || Type(c.Traces) != "otlp" {
		t.Errorf("telemetry")
	}
	if c.Limits.Retries != 16 || c.Limits.ConsistencyWait != 2*time.Second || c.Limits.IdempotencyTTL != 24*time.Hour {
		t.Errorf("limits: %+v", c.Limits)
	}
}

func TestCrypto(t *testing.T) {
	c, err := Decode([]byte("stores:\n  main: { type: memory }\n  audit: { type: memory }\nstate: main\n" +
		"crypto:\n  kms: { type: local, key_env: K }\n  cache: { subjects: 10, ttl: 1m }\n  forget_log: audit\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Type(c.Crypto.KMS) != "local" || c.Crypto.Subjects != 10 || c.Crypto.TTL != time.Minute || c.Crypto.ForgetLog != "audit" {
		t.Fatalf("crypto: %+v", c.Crypto)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Decode([]byte("# empty\n"))
	if err != nil || c.State != "main" || Type(c.Stores["main"]) != "memory" || c.Tenants["default"] != "base" || len(c.Ingress) != 1 {
		t.Fatalf("%+v %v", c, err)
	}
	c, err = Decode([]byte("stores:\n  db: { type: memory }\n"))
	if err != nil || c.State != "db" {
		t.Fatalf("a single store is the main one: %v", err)
	}
}

func TestErrors(t *testing.T) {
	for in, want := range map[string]string{
		"engine: stores\n":                                        "1: engine",
		"stors: {}\n":                                             `1: unknown key "stors"`,
		"stores:\n  a: { dsn: x }\n":                              "2: stores.a: no type",
		"stores:\n  a: { type: memory }\nstate: b\n":              `3: state: no store "b"`,
		"projections:\n  X: nope\n":                               `2: X: no store "nope"`,
		"tenants:\n  t1: { extension: base }\n":                   "2: tenants.t1: base is not an extension; do not write extension for the base model",
		"limits: { retries: many }\n":                             "1: retries: expected an integer",
		"limits: { consistency_wait: 2 }\n":                       "1: consistency_wait: expected a duration",
		"notifications: { delivery: [sse] }\n":                    `1: delivery "sse"`,
		"state: main\nstate: x\n":                                 `2: key "state" is repeated`,
		"stores:\n  a: { type: memory\n":                          "2: expected",
		"stores:\n\ta: x\n":                                       "2: a tab",
		"tenants:\n  t1: { extension: 'a }\n":                     "2: unclosed string",
		"telemetry:\n  logs: { colour: red }\n":                   `2: unknown key "colour"`,
		"ingress:\n  - type: http\n   listen: x\n":                "3: expected a list item",
		"stores:\n  a: { type: memory }\n  b: { type: memory }\n": "state: set",
		"crypto: { kms: { key: x } }\n":                           "crypto.kms: no type",
		"crypto: { forget_log: audit }\n":                         `no store "audit"`,
		"crypto: { cache: { size: 1 } }\n":                        `unknown key "size"`,
		"integrations:\n  workers: -1\n":                          "2: integrations.workers: expected a positive",
		"integrations:\n  workers: many\n":                        "2: workers: expected an integer",
		"limits: { request_ttl: 1h }\n":                           `1: unknown key "request_ttl"`,
		"integrations:\n  retry: { multiplier: 0.5 }\n":           "2: integrations.retry.multiplier: expected a number of at least 1",
		"integrations:\n  retry: { initial: -1s }\n":              "2: integrations.retry.initial: expected a positive",
		"integrations:\n  retry: { max_elapsed: 1 }\n":            "2: max_elapsed: expected a duration",
		"integrations:\n  retry: 3\n":                             "2: integrations.retry: expected a map",
		"integrations:\n  auth: x\n":                              "2: integrations.auth: expected a map",
		"integrations:\n  auth: { key: x }\n":                     `2: unknown key "key"`,
		"bus:\n  type: log\n  publish: [A.b]\n":                   "3: bus.publish is no longer supported",
		"bus: { type: log, notifications: maybe }\n":              "notifications",
	} {
		_, err := Decode([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v; want %q", in, err, want)
		}
	}
}

func TestYAML(t *testing.T) {
	n, err := Parse([]byte(`
a:
  - x
  - "y # not a comment"
  -
    k: v
  - [1, {p: q, r: [s]}]
b: 'it''s'
c:
- 1
- 2
d: ~
e: http://host:80/path#frag
`))
	if err != nil {
		t.Fatal(err)
	}
	a := n.Get("a")
	if a.Kind != List || len(a.Values) != 4 || a.Values[1].Value != "y # not a comment" || a.Values[2].Get("k").Value != "v" {
		t.Fatalf("a: %+v", a)
	}
	if r := a.Values[3].Values[1].Get("r"); r == nil || r.Values[0].Value != "s" {
		t.Error("nested flow collections")
	}
	if n.Get("b").Value != "it's" || len(n.Get("c").Values) != 2 || n.Get("d").Kind != Null || n.Get("e").Value != "http://host:80/path#frag" {
		t.Errorf("b, c, d, e: %+v", n)
	}
}
