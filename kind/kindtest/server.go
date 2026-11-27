package kindtest

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/servertest"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// The test kit of a distribution: a binary that links the server package
// runs reasonrd in its tests, calls its API and checks what it sends.

// Buffer is a log safe for concurrent use; Call calls the HTTP API of a
// test server: the status and the body.
type Buffer = servertest.Buffer

var Call = servertest.Call

// linked fails the test if it does not link the server package.
func linked(t testing.TB) {
	t.Helper()
	if servertest.Serve == nil {
		t.Fatal("kindtest: the test does not link github.com/tycheg/reasonr-engine/server")
	}
}

// StartServer runs reasonrd with args on a free local address and returns
// its base URL and the stop function.
func StartServer(t testing.TB, args ...string) (string, func()) {
	t.Helper()
	linked(t)
	return servertest.Start(t, servertest.Serve, args...)
}

// Serve runs reasonrd with args until ctx ends, as server.Run does; ready
// is called each time the engine is ready to serve (dev reloads it).
func Serve(t testing.TB, ctx context.Context, args []string, stdout, stderr io.Writer, ready func()) int {
	t.Helper()
	linked(t)
	return servertest.Serve(ctx, args, stdout, stderr, func(*host.Engine) {
		if ready != nil {
			ready()
		}
	})
}

// Scenarios runs the scenarios of the model in dir on the test server
// base in the tenant tenant.
func Scenarios(t testing.TB, base, dir, tenant string) { servertest.Scenarios(t, base, dir, tenant) }

// OpenConfig builds the engine of the configuration file path and the
// model in dir, as reasonrd does at startup, and closes it: an error of
// the configuration, of the model or of a store connected eagerly.
func OpenConfig(path, dir string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	programs, err := servertest.LoadPrograms(dir, cfg.Tenants, false)
	if err != nil {
		return err
	}
	e, err := host.Build(cfg, programs, host.Options{})
	if err != nil {
		return err
	}
	return e.Close()
}

// PublishedModel is a copy of the example model name that publishes the
// types publish (the bus).
func PublishedModel(t testing.TB, name string, publish ...string) string {
	return testmodels.Published(t, name, publish...)
}

// FreeAddr is a free local TCP address.
func FreeAddr() (string, error) { return testrun.FreeAddr() }

// DialWebSocket connects to the notification WebSocket url with the
// headers h: read gives the next message.
func DialWebSocket(ctx context.Context, url string, h http.Header) (read func() ([]byte, error), closeFn func() error, err error) {
	return httpapi.DialWebSocket(ctx, url, h)
}

// SignToken is a JWT with the claims signed by HS256 with secret (an
// identity provider in tests).
func SignToken(claims map[string]any, secret []byte) (string, error) {
	return auth.Sign(claims, secret)
}

// TokenVerifier checks the JWT the engine signs for the integration
// service: by the engine's JWKS, its issuer and the audience (the system).
type TokenVerifier struct{ jwt *auth.JWT }

// NewTokenVerifier verifies tokens by the keys at jwksURL.
func NewTokenVerifier(jwksURL, issuer, audience string) *TokenVerifier {
	return &TokenVerifier{jwt: &auth.JWT{Keys: &auth.JWKS{URL: jwksURL}, Issuer: issuer, Audience: audience}}
}

// Verify checks the Authorization header value and returns the claims.
func (v *TokenVerifier) Verify(ctx context.Context, authorization string) (map[string]any, error) {
	return v.jwt.Verify(ctx, auth.ParseAuthorization(authorization).Token)
}
