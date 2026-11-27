package kindtest

import (
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/port"
)

// Ready wraps a bus adapter as the engine does: each message gets its topic
// (by the adapter's TopicTemplate, if it is a port.TopicBroker) and its
// CloudEvents attributes before the adapter publishes it.
func Ready(pub port.Publisher) port.Publisher {
	r := &bus.Ready{Publisher: pub}
	if tb, ok := pub.(port.TopicBroker); ok {
		r.Template = tb.TopicTemplate()
	}
	return r
}
