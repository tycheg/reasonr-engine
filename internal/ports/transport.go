package ports

import (
	"context"

	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Transport is an integration transport as the request sender holds it: it
// gets the request with its model values (the built-in HTTP transport
// encodes them with its codec, the fakes of test mode run them). A
// plugin's transport gets the ready request through Plugin.
type Transport interface {
	Send(ctx context.Context, r *kind.LeasedRequest) error
}

// Plugin is the transport of a plugin (port.IntegrationTransport): it gets
// the ready request the sender filled.
type Plugin struct {
	port.IntegrationTransport
}

// Send sends the ready request.
func (p Plugin) Send(ctx context.Context, r *kind.LeasedRequest) error {
	return p.IntegrationTransport.Send(ctx, &r.LeasedRequest)
}

// TopicBroker returns the topic broker of a transport over a bus.
func TopicBroker(t Transport) (port.TopicBroker, bool) {
	if p, ok := t.(Plugin); ok {
		tb, ok := p.IntegrationTransport.(port.TopicBroker)
		return tb, ok
	}
	tb, ok := t.(port.TopicBroker)
	return tb, ok
}
