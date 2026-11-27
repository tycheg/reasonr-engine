package port

import "time"

// LeasedRequest is a leased external request, ready for a transport: the
// engine encodes it before Send.
type LeasedRequest struct {
	Tenant   Tenant
	Position Position  // the unit of work that sent the request
	At       time.Time // the time of that unit of work
	// Deadline is the moment of the deadline timer (the timeout
	// parameter); zero — the request has no timer. TimerGone: the timer is
	// gone — it fired or was deleted, the model no longer waits for an answer.
	Deadline  time.Time
	TimerGone bool
	Attempts  int // the attempt number: the number of leases, this one included

	// System and Name are the short name of the external system and the
	// request; Key is the idempotency key as text; Body are the arguments
	// without the timeout parameter in canonical JSON (the body of an HTTP
	// request); Attributes are the CloudEvents attributes of a transport
	// over a bus; Topic is the channel by the transport's TopicTemplate (a
	// TopicBroker).
	System     string
	Name       string
	Key        string
	Body       []byte
	Attributes []Attribute
	Topic      string
}

// Headers are the headers of the binary mode of a binding over a bus: the
// attributes with the prefix prefix (ce_ for Kafka) and content-type.
func (r *LeasedRequest) Headers(prefix string) []Attribute {
	return headers(r.Attributes, prefix, "")
}
