package memory

import (
	"context"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// Bus is an in-memory bus (port.Publisher): the reference of the RunPublisher
// conformance suite and the bus of tests that read what was published.
type Bus struct {
	// Fail is the error of the next publications (an unavailable bus); nil — success.
	Fail func(msgs []port.Message) error

	mu   sync.Mutex
	msgs []port.Message
}

var _ port.Publisher = (*Bus)(nil)

// Publish remembers the messages (port.Publisher).
func (b *Bus) Publish(_ context.Context, msgs []port.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Fail != nil {
		if err := b.Fail(msgs); err != nil {
			return err
		}
	}
	b.msgs = append(b.msgs, msgs...)
	return nil
}

// Messages returns the published messages in order.
func (b *Bus) Messages() []port.Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]port.Message(nil), b.msgs...)
}
