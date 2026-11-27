package host_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	_ "github.com/tycheg/reasonr-engine/internal/builtin"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// schemaOnly is an interpreter that has only a schema: for build checks.
type schemaOnly struct {
	reasonr.Interpreter
	s *reasonr.Schema
}

func (s schemaOnly) Schema() *reasonr.Schema { return s.s }

func programs(t *testing.T) *kernel.Programs {
	p := kernel.NewPrograms()
	p.Set(port.DefaultTenant, conformance.Machine(t))
	return p
}

func build(t *testing.T, yaml string, p *kernel.Programs, clock port.Clock) (*host.Engine, error) {
	t.Helper()
	cfg, err := config.Decode([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return host.Build(cfg, p, host.Options{Clock: clock})
}

func TestBuildErrors(t *testing.T) {
	unique := &reasonr.Schema{Types: []reasonr.TypeInfo{{Ref: 1, Name: "Mart", Kind: reasonr.TypeProjection,
		Indexes: []reasonr.IndexInfo{{Name: "byCode", Unique: true}}}}}
	pu := kernel.NewPrograms()
	pu.Set(port.DefaultTenant, schemaOnly{s: unique})
	for _, c := range []struct {
		yaml string
		p    *kernel.Programs
		want string
	}{
		{"stores:\n  main: { type: postgres }\n", programs(t), `store "postgres" is not in the build (available: memory)`},
		{"stores:\n  main: { type: memory }\n  views: { type: memory }\nstate: main\nprojections:\n  Mart: views\n", pu,
			"with unique index byCode can only be synchronous"},
		{"stores:\n  main: { type: memory }\n  views: { type: memory }\nstate: main\nprojections:\n  Nope: views\n", programs(t),
			"projections.Nope: no such projection"},
		{"notifications:\n  hub: { type: redis }\n", programs(t), `notification hub "redis" is not in the build`},
		{"auth: { type: oauth }\n", programs(t), `authentication "oauth" is not in the build (available: jwt, keys)`},
		{"auth: { type: jwt }\n", programs(t), "set secret"},
		{"ingress: [{ type: grpc }]\n", programs(t), `transport "grpc" is not in the build`},
		{"ingress: [{ type: http, codec: protobuf }]\n", programs(t), "codec"},
		{"telemetry:\n  metrics: { type: otlp }\n", programs(t), "adapters/otel"},
		{"stores:\n  main: { type: memory, dsn: x }\n", programs(t), `unknown key "dsn"`},
	} {
		if _, err := build(t, c.yaml, c.p, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: %v; want %q", c.yaml, err, c.want)
		}
	}
	e, err := build(t, "", programs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(context.Background(), []string{"web"}); err == nil {
		t.Error("unknown role: no error")
	}
}

// TestAsyncPlacement: a projection in another store is asynchronous, and
// the kernel reads it from there.
func TestAsyncPlacement(t *testing.T) {
	p := programs(t)
	e, err := build(t, "stores:\n  main: { type: memory }\n  views: { type: memory }\nstate: main\nprojections:\n  BoxView: views\n", p, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := conformance.Machine(t).Schema()
	box, view := s.Lookup("Box"), s.Lookup("BoxView")
	a := reasonr.NewArena()
	b, _ := e.Executor.(kind.Kernel).NewID(context.Background(), port.DefaultTenant, box.Ref)
	res, err := e.Executor.(kind.Kernel).Command(context.Background(), &kind.CommandRequest{Tenant: port.DefaultTenant, Call: reasonr.CommandCall{
		Entity: box.Ref, ID: b, Command: box.Method("Put").Ref, Arena: a,
		Args:   a.Record(0, reasonr.Enum(s.Lookup("Kind").Ref, 0), reasonr.Int(3), reasonr.None),
		Caller: reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}},
	}})
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("Put: %v", err)
	}
	res.Release()
	if _, _, ok, _ := e.Store.Load(context.Background(), port.DefaultTenant, a, view.Ref, reasonr.ID(view.Ref, b)); ok {
		t.Fatal("an asynchronous projection is in the main store")
	}
	units, _ := e.Store.(kind.EffectSource).Read(context.Background(), 0, 0)
	if len(units) != 1 || len(units[0].Projections) != 1 {
		t.Fatalf("projection operations in the outbox: %d", len(units))
	}
}

// TestSharedKeyring: the event store and asynchronous projections in a
// separate store encrypt with the main store's keys (RFC §31.2).
func TestSharedKeyring(t *testing.T) {
	e, err := build(t, "stores:\n  main: { type: memory }\n  views: { type: memory }\n  audit: { type: memory }\n"+
		"state: main\nevents: audit\nprojections:\n  BoxView: views\n", programs(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ring := e.Stores["main"].(pii.Shared).Keyring()
	for _, name := range []string{"views", "audit"} {
		if e.Stores[name].(pii.Shared).Keyring() != ring {
			t.Errorf("%s encrypts with its own keys", name)
		}
	}
}

// TestKindRoles: the roles of the process are the host's and the kind's
// (the test kind: timers, dispatch); an unknown one is an error.
func TestKindRoles(t *testing.T) {
	e, err := build(t, "", programs(t), memory.NewClock(memory.DefaultNow))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.Roles(); !slices.Equal(got, []string{"api", "dispatch", "timers"}) {
		t.Fatalf("roles %v", got)
	}
	if err := e.Run(context.Background(), []string{"nope"}); err == nil {
		t.Fatal("an unknown role is accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx, []string{"timers", "dispatch"}) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("roles timers and dispatch: %v", err)
	}
}

// TestTestModeTimers: in test mode the kind runs the timers of ordinary
// tenants (Host.TimerTenants) and leaves those of ephemeral ones to
// /_test/{tenant}/clock.
func TestTestModeTimers(t *testing.T) {
	cfg, err := config.Decode([]byte("ingress:\n  - { type: http, listen: \"127.0.0.1:0\" }\n"))
	if err != nil {
		t.Fatal(err)
	}
	clock := memory.NewClock(memory.DefaultNow)
	m := conformance.Machine(t)
	e, err := host.Build(cfg, programs(t), host.Options{Test: true, Clock: clock, Models: map[string]reasonr.Interpreter{"base": m}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	rec := httptest.NewRecorder()
	e.Listeners[0].(*httpapi.Listener).Options.Mount["/_test/"].ServeHTTP(rec, httptest.NewRequest("POST", "/_test/tenants", strings.NewReader(`{}`)))
	var created struct{ Tenant port.Tenant }
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Tenant == "" {
		t.Fatalf("ephemeral tenant: %d %s", rec.Code, rec.Body)
	}
	s := m.Schema()
	box := s.Lookup("Box")
	for _, tenant := range []port.Tenant{port.DefaultTenant, created.Tenant} {
		a := reasonr.NewArena()
		b, _ := e.Executor.(kind.Kernel).NewID(context.Background(), tenant, box.Ref)
		for _, c := range []struct {
			cmd  string
			args []reasonr.Value
		}{{"Put", []reasonr.Value{reasonr.Enum(s.Lookup("Kind").Ref, 0), reasonr.Int(1), reasonr.None}}, {"Arm", []reasonr.Value{reasonr.Duration(time.Minute)}}} {
			res, err := e.Executor.(kind.Kernel).Command(context.Background(), &kind.CommandRequest{Tenant: tenant, Call: reasonr.CommandCall{
				Entity: box.Ref, ID: b, Command: box.Method(c.cmd).Ref, Arena: a, Args: a.Record(0, c.args...),
				Caller: reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}},
			}})
			if err != nil || len(res.Errors) > 0 {
				t.Fatalf("%s: %v", c.cmd, err)
			}
			res.Release()
		}
		clock.Set(tenant, memory.DefaultNow.Add(time.Minute))
	}
	tenants, all := e.Host().TimerTenants()
	if all || !slices.Contains(tenants, port.DefaultTenant) || slices.Contains(tenants, created.Tenant) {
		t.Fatalf("timer tenants %v (all %v): the ordinary %s, not the ephemeral %s", tenants, all, port.DefaultTenant, created.Tenant)
	}
}
