package codecs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/codecs/codecstest"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/internal/testrun"
	"github.com/tycheg/reasonr-engine/port"
)

// TestRoundTrip: the arguments of every step of the example tests and the
// results of their queries go through a codec adapter and back unchanged —
// a binary test format the compiler does not know and the json codec's own
// writer and reader; the tests pass on the decoded values.
func TestRoundTrip(t *testing.T) {
	for _, c := range []port.Codec{codecstest.TLV, codecs.JSON} {
		for _, m := range []struct{ dir, ext string }{{"taxi", ""}, {"taxi", "acme"}, {"bank", ""}} {
			t.Run(c.Name()+"/"+m.dir+m.ext, func(t *testing.T) {
				model, err := reasonr.Build(testmodels.Dir(m.dir), reasonr.Options{Extension: m.ext, Test: true})
				if err != nil {
					t.Fatal(err)
				}
				vals := &roundTrip{t: t, c: c, s: model.Schema()}
				rep, err := testrun.Run(context.Background(), model, testrun.Options{Drivers: []testrun.Driver{{Name: "codec",
					New: func(context.Context, int) (reasonr.Driver, func(), error) {
						return &codecDriver{Driver: testrun.MemoryDriver(model), v: vals}, func() {}, nil
					}}}})
				if err != nil {
					t.Fatal(err)
				}
				for _, tr := range rep.Tests {
					for _, f := range tr.Failures {
						t.Logf("%s: %s", tr.Name, testrun.Message(&f))
					}
				}
				if len(rep.Tests) == 0 || rep.Failed() > 0 || vals.n == 0 {
					t.Fatalf("%d tests, %d failed, %d values", len(rep.Tests), rep.Failed(), vals.n)
				}
			})
		}
	}
}

type roundTrip struct {
	t *testing.T
	c port.Codec
	s *reasonr.Schema
	n int
}

// args encodes the arguments of mi with the codec and decodes them back.
func (r *roundTrip) args(a *reasonr.Arena, mi *reasonr.MethodInfo, args reasonr.Record) (reasonr.Record, error) {
	v := codecs.New(r.c, r.s, codecs.Options{})
	data, err := v.AppendArgs(nil, a, mi.Params, args)
	if err != nil {
		return args, fmt.Errorf("%s: encoding: %v", mi.Name, err)
	}
	back, err := v.DecodeArgs(a, mi.Params, data)
	if err != nil {
		return args, fmt.Errorf("%s: decoding %q: %v", mi.Name, data, err)
	}
	for i := range mi.Params {
		x, y := a.Elems(args.Value())[i], a.Elems(back.Value())[i]
		if x.Kind != reasonr.KindNone && !reasonr.Equal(a, x, a, y) {
			return args, fmt.Errorf("%s: argument %s changed", mi.Name, mi.Params[i].Name)
		}
	}
	r.n++
	return back, nil
}

// value encodes a value of type t with the codec and decodes it back.
func (r *roundTrip) value(a *reasonr.Arena, t reasonr.Type, x reasonr.Value) error {
	v := codecs.New(r.c, r.s, codecs.Options{})
	data, err := v.Append(nil, a, t, x)
	if err != nil {
		return fmt.Errorf("encoding a %s: %v", t.Kind, err)
	}
	back, err := v.Decode(a, t, data)
	if err != nil {
		return fmt.Errorf("decoding a %s %q: %v", t.Kind, data, err)
	}
	if !reasonr.Equal(a, x, a, back) {
		return fmt.Errorf("a %s changed: %q", t.Kind, data)
	}
	r.n++
	return nil
}

// codecDriver passes every step through the codec: the arguments go to the
// engine decoded, a query result is checked.
type codecDriver struct {
	reasonr.Driver
	v *roundTrip
}

func (d *codecDriver) Command(ctx context.Context, call reasonr.CommandCall) (*reasonr.Step, error) {
	ti := d.v.s.Type(call.Entity)
	var err error
	if call.Args, err = d.v.args(call.Arena, &ti.Methods[call.Command], call.Args); err != nil {
		return nil, err
	}
	if call.ID.Kind != reasonr.KindNone {
		if err := d.v.value(call.Arena, reasonr.Type{Kind: reasonr.KindID, Ref: call.ID.Type}, call.ID); err != nil {
			return nil, err
		}
	}
	return d.Driver.Command(ctx, call)
}

func (d *codecDriver) Query(ctx context.Context, call reasonr.QueryCall) (*reasonr.Step, error) {
	ti := d.v.s.Type(call.Type)
	mi := &ti.Methods[call.Query]
	var err error
	if call.Args, err = d.v.args(call.Arena, mi, call.Args); err != nil {
		return nil, err
	}
	st, err := d.Driver.Query(ctx, call)
	if err == nil && len(st.Errors) == 0 && mi.Result != nil && st.Result.Kind != reasonr.KindNone {
		err = d.v.value(st.Arena, *mi.Result, st.Result)
	}
	return st, err
}

func (d *codecDriver) EnableFeature(ctx context.Context, name string) error {
	fd, ok := d.Driver.(reasonr.FeatureDriver)
	if !ok {
		return errors.New("no feature flags")
	}
	return fd.EnableFeature(ctx, name)
}
