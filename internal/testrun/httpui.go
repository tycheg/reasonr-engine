package testrun

import (
	"context"

	"github.com/tycheg/reasonr-engine/internal/codecs"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// For UI drivers (web/driver, RFC §27.3): given, time offset and the
// answers of external systems go through the test API with this driver;
// steps through the UI parse API responses with the same code.

// Tenant is the test's ephemeral tenant.
func (d *HTTPDriver) Tenant() string { return d.tenant }

// Codec is canonical JSON by the model schema.
func (d *HTTPDriver) Codec() *codecs.Values { return d.codec }

// AppendJSON appends the canonical JSON of a value (port.TestAPI).
func (d *HTTPDriver) AppendJSON(dst []byte, a *reasonr.Arena, t reasonr.Type, v reasonr.Value) ([]byte, error) {
	return d.codec.Append(dst, a, t, v)
}

// DecodeJSON decodes a value from canonical JSON (port.TestAPI).
func (d *HTTPDriver) DecodeJSON(a *reasonr.Arena, t reasonr.Type, data []byte) (reasonr.Value, error) {
	return d.codec.Decode(a, t, data)
}

// Auth is the caller's Authorization value in the ephemeral tenant.
func (d *HTTPDriver) Auth(src *reasonr.Arena, c reasonr.Caller) (string, error) {
	return d.auth(src, c)
}

// IDText is the text form of an identifier.
func (d *HTTPDriver) IDText(a *reasonr.Arena, id reasonr.Value) (string, error) {
	return d.idText(a, id)
}

// NewStep is an empty step result in the driver arena.
func (d *HTTPDriver) NewStep() *reasonr.Step { return d.step() }

// Response parses a command response into the step result: errors and
// notifications to the initiator; it remembers the position for after.
func (d *HTTPDriver) Response(st *reasonr.Step, status int, data []byte, caller reasonr.Caller, src *reasonr.Arena) error {
	return d.response(st, status, data, caller, src)
}

// Captured adds to the step the notifications to other actors and external requests.
func (d *HTTPDriver) Captured(ctx context.Context, st *reasonr.Step) error {
	return d.captured(ctx, st)
}

// After is the position of the last command.
func (d *HTTPDriver) After() string { return d.after }

// SetAfter sets the position of the last command (the command went through the UI).
func (d *HTTPDriver) SetAfter(p string) {
	if p != "" {
		d.after = p
	}
}

// DecodeError decodes a business error from an API response into the driver arena.
func (d *HTTPDriver) DecodeError(raw []byte) (reasonr.BusinessError, error) {
	return d.decodeError(raw)
}
