package kind

import (
	"context"
	"flag"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// TestDriver is a driver of acceptance test runs an adapter adds to
// reasonrd test (--driver web, RFC §27.3): a binary that links it has the
// driver. It registers itself with registry.RegisterDriver.
type TestDriver interface {
	// Name is the driver name in --driver.
	Name() string
	// Flags adds the driver's flags to reasonrd test.
	Flags(fl *flag.FlagSet)
	// Model prepares the runs of the model m (model is base or a tenant
	// extension) on the test mode server url; api opens a client of the
	// server's test API for a run (given steps, the clock, fakes).
	Model(ctx context.Context, m *reasonr.Model, model, url string, api TestAPIFactory) (TestRuns, error)
	// Close frees the driver after all models.
	Close()
}

// TestRuns are the runs of a model's tests on a driver.
type TestRuns struct {
	// New gives the driver of the run run (from 0) and its release after the
	// run. UI drivers run a test k times: in run i a step takes place i.
	New func(ctx context.Context, run int) (reasonr.Driver, func(), error)
	// Runs is how many runs a test needs; it is asked of the driver of the
	// first run after it. nil — one run.
	Runs func(d reasonr.Driver) int
	// Uncovered gives the places no step went through, for the report;
	// nil — coverage is not counted.
	Uncovered func() []string
}

// TestAPI is a client of the test API of a test mode server for one run:
// the steps that do not go through the driver's own channel.
type TestAPI interface {
	reasonr.Driver
	reasonr.FeatureDriver
	// NewStep is an empty step result in the client's arena.
	NewStep() *reasonr.Step
	// Close drops the run's ephemeral tenant.
	Close(ctx context.Context) error

	// What a UI driver needs to call the server's API as the caller of a
	// step. Auth is the caller's Authorization value in the ephemeral
	// tenant; IDText is the text form of an identifier; AppendJSON and
	// DecodeJSON are canonical JSON by the model schema.
	Auth(a *reasonr.Arena, c reasonr.Caller) (string, error)
	IDText(a *reasonr.Arena, id reasonr.Value) (string, error)
	AppendJSON(dst []byte, a *reasonr.Arena, t reasonr.Type, v reasonr.Value) ([]byte, error)
	DecodeJSON(a *reasonr.Arena, t reasonr.Type, data []byte) (reasonr.Value, error)
	// After is the position of the last command (the _after of a query).
	After() string
	// Response parses a command response of the API into the step result:
	// errors and notifications to the initiator; it remembers the position.
	Response(st *reasonr.Step, status int, data []byte, caller reasonr.Caller, src *reasonr.Arena) error
	// Captured adds to the step the notifications to other actors and the
	// external requests of the test API.
	Captured(ctx context.Context, st *reasonr.Step) error
	// DecodeError decodes a business error of an API response.
	DecodeError(raw []byte) (reasonr.BusinessError, error)
}

// TestAPIFactory opens a test API client.
type TestAPIFactory func(ctx context.Context) (TestAPI, error)
