package ports

import (
	"context"
	"log/slog"

	"github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Listener is a transport of incoming calls: HTTP, gRPC, TCP, UDP. It
// accepts calls, decodes them with a codec, authenticates them and passes
// them to the executor; it returns notifications to the initiator in the
// command response, to other actors by subscription (Executor.Subscribe).
type Listener interface {
	// Serve serves calls until ctx is cancelled.
	Serve(ctx context.Context, x kind.Executor, deps EdgeDeps) error
}

// EdgeDeps are the shared dependencies of transports.
type EdgeDeps struct {
	Auth    Authenticator
	Logger  *slog.Logger
	Metrics port.Metrics
	Tracer  Tracer
}

// Credentials are what the caller proves itself with.
type Credentials struct {
	Scheme string // Bearer, ApiKey, Test, mtls
	Token  string
	Peer   string // an address or a certificate subject
	// Tenant is the tenant the caller named itself (X-Reasonr-Tenant):
	// only an external system token for every tenant accepts it
	// (RFC §18); other authenticators ignore it.
	Tenant port.Tenant
}

// Authenticator determines the data tenant and the caller; Subject is in
// the arena a. Not authenticated — ErrUnauthenticated; the credentials are
// valid but do not give what is asked — ErrForbidden.
type Authenticator interface {
	Authenticate(ctx context.Context, cred Credentials, a *reasonr.Arena) (port.Tenant, reasonr.Caller, error)
}
