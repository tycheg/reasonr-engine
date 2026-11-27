package host_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// brokerState holds the topics of the topicstest check broker: shared by
// the bus and the transport, as in one cluster.
var brokerState struct {
	sync.Mutex
	have    map[string]int // topic → partitions
	down    bool
	created map[string]port.TopicSettings
}

func resetBroker() {
	brokerState.Lock()
	defer brokerState.Unlock()
	brokerState.have, brokerState.down, brokerState.created = map[string]int{}, false, map[string]port.TopicSettings{}
}

// testBroker is a bus and a transport with topics: topic is the template,
// partitions — create_topics.
type testBroker struct {
	template string
	settings *port.TopicSettings
}

func (b *testBroker) Publish(context.Context, []port.Message) error   { return nil }
func (b *testBroker) Send(context.Context, *kind.LeasedRequest) error { return nil }
func (b *testBroker) TopicTemplate() string                           { return b.template }
func (b *testBroker) TopicSettings() *port.TopicSettings              { return b.settings }
func (b *testBroker) DescribeTopics(_ context.Context, names []string) (map[string]int, error) {
	brokerState.Lock()
	defer brokerState.Unlock()
	if brokerState.down {
		return nil, errors.New("the broker is unavailable")
	}
	out := map[string]int{}
	for _, n := range names {
		if p, ok := brokerState.have[n]; ok {
			out[n] = p
		}
	}
	return out, nil
}

func (b *testBroker) CreateTopics(_ context.Context, names []string, s port.TopicSettings) error {
	brokerState.Lock()
	defer brokerState.Unlock()
	for _, n := range names {
		brokerState.have[n] = max(s.Partitions, 1)
		brokerState.created[n] = s
	}
	return nil
}

func init() {
	newBroker := func(n *config.Node) *testBroker {
		topic, _ := n.String("topic", "")
		b := &testBroker{template: topic}
		if p, _ := n.Int("partitions", -1); p >= 0 {
			b.settings = &port.TopicSettings{Partitions: p}
		}
		return b
	}
	registry.RegisterPublisher("topicstest", func(n *config.Node, _ registry.Deps) (port.Publisher, error) { return newBroker(n), nil })
	registry.RegisterIntegration("topicstest", func(n *config.Node, _ registry.Deps) (ports.Transport, error) {
		return newBroker(n), nil
	})
}

const topicsYAML = `
stores:
  main: { type: memory }
state: main
events: main
bus: { type: topicstest, topic: "ev.{tenant}.{kind}.{type}", notifications: true }
integrations:
  Store: { type: topicstest, topic: "ev.{tenant}.request.{system}", partitions: 2 }
`

// TestTopics: the topics of the bus and the request transport are derived
// from the tenant models; serve checks them, migrate creates them by
// create_topics, test mode always does; an unavailable broker and another
// number of partitions are warnings.
func TestTopics(t *testing.T) {
	cfg, err := config.Decode([]byte(topicsYAML))
	if err != nil {
		t.Fatal(err)
	}
	p := programs(t)
	p.Set("t2", conformance.Machine(t))
	var log bytes.Buffer
	e, err := host.Build(cfg, p, host.Options{Logger: slog.New(slog.NewTextHandler(&log, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	var bus []string
	for _, tn := range []string{"default", "t2"} {
		for _, s := range []string{"event.Box", "event.Keeper", "event.Ticket", "notification.Keeper"} {
			bus = append(bus, "ev."+tn+"."+s)
		}
	}
	var names []string
	for _, tp := range e.Topics {
		names = append(names, tp.Name)
		want := "bus"
		if strings.Contains(tp.Name, ".request.") {
			want = "integrations.Store"
			if tp.Settings == nil || tp.Settings.Partitions != 2 {
				t.Errorf("settings of %s: %+v", tp.Name, tp.Settings)
			}
		}
		if tp.Component != want {
			t.Errorf("topic %s: component %s, want %s", tp.Name, tp.Component, want)
		}
	}
	reqs := []string{"ev.default.request.Store", "ev.t2.request.Store"}
	if want := slices.Sorted(slices.Values(append(slices.Clone(bus), reqs...))); !slices.Equal(names, want) {
		t.Fatalf("topics %v, want %v", names, want)
	}
	ctx := context.Background()

	resetBroker()
	brokerState.down = true
	if err := e.EnsureTopics(ctx, false); err != nil || !strings.Contains(log.String(), "topics not checked") {
		t.Fatalf("unavailable broker: %v\n%s", err, log.String())
	}
	brokerState.down = false
	err = e.EnsureTopics(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "no topics "+strings.Join(names, ", ")+" — ") || len(brokerState.created) != 0 {
		t.Fatalf("serve without topics: %v, created %v", err, brokerState.created)
	}
	err = e.EnsureTopics(ctx, true) // migrate: only for a component with create_topics
	if err == nil || !strings.Contains(err.Error(), "no topics "+strings.Join(bus, ", ")+" — ") {
		t.Fatalf("migrate without create_topics for the bus: %v", err)
	}
	if got := slices.Sorted(maps.Keys(brokerState.created)); !slices.Equal(got, reqs) || brokerState.created[reqs[1]].Partitions != 2 {
		t.Fatalf("created %v", brokerState.created)
	}
	for _, n := range bus {
		brokerState.have[n] = 1 // operations created the bus topics
	}
	brokerState.have[reqs[0]] = 3
	if err := e.EnsureTopics(ctx, false); err != nil || !strings.Contains(log.String(), "another number of partitions") {
		t.Fatalf("all topics exist: %v\n%s", err, log.String())
	}

	// Test mode creates every topic: without create_topics — with one partition.
	resetBroker()
	te, err := host.Build(cfg, p, host.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	defer te.Close()
	if err := te.EnsureTopics(ctx, false); err != nil || len(brokerState.created) != len(names) ||
		brokerState.created["ev.t2.event.Box"].Partitions != 1 || brokerState.created[reqs[0]].Partitions != 2 {
		t.Fatalf("test mode: %v, created %v", err, brokerState.created)
	}
}

// TestTopicsList: host.Topics lists the topics without stores; the
// default transport serves systems without their own entry, the fake
// transport has no topics and exists only in test mode.
func TestTopicsList(t *testing.T) {
	cfg, err := config.Decode([]byte(`
stores:
  main: { type: postgres }
bus: { type: topicstest, topic: "ev.{kind}.{type}" }
integrations:
  default: { type: topicstest, topic: "req.{tenant}.{system}" }
  Other: { type: fake }
`))
	if err != nil {
		t.Fatal(err)
	}
	p := programs(t)
	if _, err := host.Topics(cfg, p, host.Options{}); err == nil || !strings.Contains(err.Error(), "integrations.Other") {
		t.Fatalf("fake outside test mode: %v", err)
	}
	ts, err := host.Topics(cfg, p, host.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tp := range ts {
		got = append(got, tp.Component+" "+tp.Name)
	}
	want := []string{"bus ev.event.Box", "bus ev.event.Keeper", "bus ev.event.Ticket", "integrations.default req.default.Store"}
	if !slices.Equal(got, want) {
		t.Fatalf("topics %v, want %v", got, want)
	}
}
