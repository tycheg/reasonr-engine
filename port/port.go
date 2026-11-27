// Package port holds the ports of the ReasonR engine (RFC §30–31): Go
// interfaces of every kind of infrastructure and the types they exchange.
// The kernel (engine/kernel) and transports depend only on ports; adapters
// (memory, PostgreSQL, Kafka, HTTP…) implement them, and the conformance
// tests (engine/conformance) pin the adapters' behavior.
//
// All values are reasonr.Value in the arena that the call or result names
// (RFC §30.1); every method takes a context.Context.
package port

import (
	"errors"
)

// Tenant is a data tenant: the isolation key of rows in stores (RFC §21.1).
// The model of a data tenant is a program from the catalog (Catalog); many
// data tenants may use one model.
type Tenant string

// DefaultTenant is the data tenant of a deployment without multitenancy.
const DefaultTenant Tenant = "default"

// Position is the number of a committed unit of work: it grows within the
// main store (the outbox record number, a journal index). 0 is no
// position: nothing is committed.
type Position uint64

// Reasons of port errors. Adapters return them (or wrap them), the kernel
// checks errors.Is.
var (
	// ErrConflict: the entity version changed after load; the kernel
	// repeats the command (RFC §30.6).
	ErrConflict = errors.New("version conflict")
	// ErrTimerGone: the fired timer is gone — another instance ran it or a
	// command deleted it. The commit is cancelled as a whole.
	ErrTimerGone = errors.New("the timer is gone")
	// ErrDuplicateKey: a response with this idempotency key is already
	// stored; the commit is cancelled, the response is UnitStore.Remembered.
	ErrDuplicateKey = errors.New("the idempotency key is already used")
	// ErrNotReady: the asynchronous projections did not apply the position
	// after in the waiting time.
	ErrNotReady = errors.New("the projections have not caught up with the position")
	// ErrUnknownTenant: the data tenant has no model.
	ErrUnknownTenant = errors.New("unknown tenant")

	// ErrMisdirected: the type belongs to a bounded context this engine
	// process does not serve (contexts in engine.yaml, RFC §23.2).
	ErrMisdirected = errors.New("another process serves the bounded context")
	// ErrPermanent: the recipient rejected the effect (for example, HTTP
	// 4xx); a retry will not help, the effect goes to dead letters.
	ErrPermanent = errors.New("the recipient rejected the effect")
)
