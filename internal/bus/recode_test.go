package bus_test

import (
	"context"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/codecs/codecstest"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/port"
)

type sink struct{ got []port.Message }

func (s *sink) Publish(_ context.Context, msgs []port.Message) error {
	s.got = append(s.got, msgs...)
	return nil
}

// TestRecoder: with bus.codec the message body is the event arguments in
// that codec and content-type is its media type; the arguments decode back.
func TestRecoder(t *testing.T) {
	m, err := reasonr.Build(testmodels.Dir("taxi"), reasonr.Options{})
	if err != nil {
		t.Fatal(err)
	}
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, m)
	sc := m.Schema()
	ti := sc.Lookup("Ordering.Customer")
	ev := ti.Method("signedUp")
	if ev == nil {
		t.Fatal("no Customer.signedUp")
	}
	msg := port.Message{Kind: port.MessageEvent, Tenant: port.DefaultTenant, Type: ti.Name, Name: ev.Name,
		Data: []byte(`{"phoneNumber":null}`)}
	out := &sink{}
	r := &bus.Recoder{Publisher: out, Catalog: programs, Codec: codecstest.TLV}
	if err := r.Publish(context.Background(), []port.Message{msg}); err != nil {
		t.Fatal(err)
	}
	got := out.got[0]
	if got.ContentType != codecstest.TLV.ContentType() || string(got.Data) == string(msg.Data) {
		t.Fatalf("not recoded: %s %q", got.ContentType, got.Data)
	}
	if h := bus.Headers(&got, "ce_"); h[len(h)-1].Value != codecstest.TLV.ContentType() {
		t.Fatalf("content-type: %v", h[len(h)-1])
	}
	a := reasonr.NewArena()
	args, err := codecs.New(codecstest.TLV, sc, codecs.Options{SealedPersonal: true}).DecodeArgs(a, ev.Params, got.Data)
	if err != nil {
		t.Fatal(err)
	}
	back, err := codecs.JSONOf(sc, codecs.Options{SealedPersonal: true}).AppendFields(nil, a, ev.Params, args)
	if err != nil || string(back) != string(msg.Data) {
		t.Fatalf("back: %s %v", back, err)
	}
}
