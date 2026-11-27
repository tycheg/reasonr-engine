// Package codecs encodes and decodes model values with a codec (plan
// 0021, decision 6): the built-in json — the canonical JSON the compiler
// writes directly — or a codec adapter through the model's value walk.
package codecs

import (
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/port"
)

func init() { registry.RegisterCodec(JSON) }

// Options are the walk options: sealed personal fields as null, unknown
// fields skipped.
type Options = reasonr.CodecOptions

// Values encodes and decodes the values of one schema with one codec.
type Values struct {
	codec  port.Codec
	schema *reasonr.Schema
	opts   Options
	json   *reasonr.JSONCodec // the built-in json: the compiler's canonical JSON
}

// New returns the values of schema s in codec c.
func New(c port.Codec, s *reasonr.Schema, o Options) *Values {
	v := &Values{codec: c, schema: s, opts: o}
	if c == nil || c.Name() == JSON.Name() {
		v.codec = JSON
		v.json = reasonr.NewJSONCodec(s)
		v.json.SealedPersonal, v.json.IgnoreUnknown = o.SealedPersonal, o.IgnoreUnknown
	}
	return v
}

// JSONOf returns the canonical JSON values of schema s.
func JSONOf(s *reasonr.Schema, o Options) *Values { return New(JSON, s, o) }

// Codec is the codec.
func (v *Values) Codec() port.Codec { return v.codec }

// Schema is the schema.
func (v *Values) Schema() *reasonr.Schema { return v.schema }

// Append appends value x of type t.
func (v *Values) Append(dst []byte, a *reasonr.Arena, t reasonr.Type, x reasonr.Value) ([]byte, error) {
	if v.json != nil {
		return v.json.Append(dst, a, t, x)
	}
	w := v.codec.NewWriter()
	if err := reasonr.EncodeValue(v.schema, t, x, a, w, v.opts); err != nil {
		return dst, err
	}
	return append(dst, w.Result()...), nil
}

// AppendRecord appends a record by the fields of its type.
func (v *Values) AppendRecord(dst []byte, a *reasonr.Arena, r reasonr.Record) ([]byte, error) {
	return v.Append(dst, a, reasonr.Type{Kind: reasonr.KindRecord, Ref: r.Type}, r.Value())
}

// AppendFields appends a record with fields fields (method arguments) as an object.
func (v *Values) AppendFields(dst []byte, a *reasonr.Arena, fields []reasonr.FieldInfo, r reasonr.Record) ([]byte, error) {
	if v.json != nil {
		return v.json.AppendFields(dst, a, fields, r)
	}
	w := v.codec.NewWriter()
	if err := reasonr.EncodeFields(v.schema, fields, r, a, w, v.opts); err != nil {
		return dst, err
	}
	return append(dst, w.Result()...), nil
}

// AppendArgs appends the given arguments params as an object: an argument
// that is none is left out (a call on the wire).
func (v *Values) AppendArgs(dst []byte, a *reasonr.Arena, params []reasonr.FieldInfo, args reasonr.Record) ([]byte, error) {
	if v.json == nil {
		w := v.codec.NewWriter()
		if err := reasonr.EncodeArgs(v.schema, params, args, a, w); err != nil {
			return dst, err
		}
		return append(dst, w.Result()...), nil
	}
	dst = append(dst, '{')
	vals := a.Elems(args.Value())
	n := 0
	for i := range params {
		if i >= len(vals) || vals[i].Kind == reasonr.KindNone {
			continue
		}
		if n > 0 {
			dst = append(dst, ',')
		}
		n++
		dst = append(reasonr.AppendJSONString(dst, params[i].Name), ':')
		t := params[i].Type
		t.Optional = true
		var err error
		if dst, err = v.json.Append(dst, a, t, vals[i]); err != nil {
			return dst, fmt.Errorf("argument %s: %w", params[i].Name, err)
		}
	}
	return append(dst, '}'), nil
}

// Decode decodes a value of type t.
func (v *Values) Decode(a *reasonr.Arena, t reasonr.Type, data []byte) (reasonr.Value, error) {
	if v.json != nil {
		return v.json.Decode(a, t, data)
	}
	return reasonr.DecodeValue(v.schema, t, v.codec.NewReader(data), a, v.opts)
}

// DecodeRecord decodes a record of type t.
func (v *Values) DecodeRecord(a *reasonr.Arena, t reasonr.TypeRef, data []byte) (reasonr.Record, error) {
	x, err := v.Decode(a, reasonr.Type{Kind: reasonr.KindRecord, Ref: t}, data)
	return x.Record(), err
}

// DecodeFields decodes a record of type t with fields fields (a record's
// fields without the ones other columns hold).
func (v *Values) DecodeFields(a *reasonr.Arena, t reasonr.TypeRef, fields []reasonr.FieldInfo, data []byte) (reasonr.Record, error) {
	if v.json != nil {
		return v.json.DecodeFields(a, t, fields, data)
	}
	x, err := v.Decode(a, reasonr.Type{Kind: reasonr.KindRecord, Ref: t}, data)
	return x.Record(), err
}

// DecodeArgs decodes the arguments params; a missing one is none.
func (v *Values) DecodeArgs(a *reasonr.Arena, params []reasonr.FieldInfo, data []byte) (reasonr.Record, error) {
	if v.json != nil {
		return v.json.DecodeArgs(a, params, data)
	}
	return reasonr.DecodeArgs(v.schema, params, v.codec.NewReader(data), a)
}

// DecodeIDText decodes an identifier of type t from the text of a path
// segment (the HTTP API): the canonical JSON's rules in every codec.
func (v *Values) DecodeIDText(a *reasonr.Arena, t reasonr.TypeRef, text string) (reasonr.Value, error) {
	j := v.json
	if j == nil {
		j = reasonr.NewJSONCodec(v.schema)
	}
	return j.DecodeIDText(a, t, text)
}
