package storekit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Outbox entries and responses by idempotency key are stored as canonical
// JSON (a format shared by the stores): types and methods by name, values
// by their schema types. So an entry is read after the model version
// changes too (type numbers change). Personal values are none, their
// ciphertexts are in sealed next to them (RFC §20).

// jsonSealed is the ciphertext of a field or parameter value by name.
type jsonSealed struct {
	Field    string        `json:"field"`
	Subjects []jsonSubject `json:"subjects"`
	Data     []byte        `json:"data"`
}

// PayloadCrypto encrypts the personal values of an outbox entry.
type PayloadCrypto struct {
	Crypto *Crypto
	Sealer *pii.Sealer
}

// sealedJSON turns ciphertexts into JSON; name is the name of the value
// Field (for a ciphertext with Member — that one: the value is encrypted under it).
func sealedJSON(ss []Sealed, name func(i int) string) []jsonSealed {
	if len(ss) == 0 {
		return nil
	}
	out := make([]jsonSealed, len(ss))
	for i, sv := range ss {
		n := sv.Member
		if n == "" {
			n = name(sv.Field)
		}
		js := jsonSealed{Field: n, Data: sv.Data, Subjects: make([]jsonSubject, len(sv.Subjects))}
		for j, r := range sv.Subjects {
			js.Subjects[j] = jsonSubject{Type: r.Type, ID: json.RawMessage(r.ID)}
		}
		out[i] = js
	}
	return out
}

// sealedFrom reads ciphertexts; index is the value number by name (-1 —
// the value is no longer in the model: the ciphertext is skipped).
func sealedFrom(js []jsonSealed, index func(name string) int) []Sealed {
	var out []Sealed
	for _, x := range js {
		i := index(x.Field)
		if i < 0 {
			continue
		}
		sv := Sealed{Field: i, Data: x.Data, Subjects: make([]port.SubjectRef, len(x.Subjects)), Member: x.Field}
		for j, r := range x.Subjects {
			sv.Subjects[j] = port.SubjectRef{Type: r.Type, ID: string(r.ID)}
		}
		out = append(out, sv)
	}
	return out
}

func fieldName(fields []reasonr.FieldInfo) func(int) string {
	return func(i int) string { return fields[i].Name }
}

// fieldIndex is a field number by name, the former one (was) included: a
// value encrypted before the rename reads under the former name in the AAD.
func fieldIndex(fields []reasonr.FieldInfo) func(string) int {
	return func(name string) int {
		for i := range fields {
			if fields[i].Name == name {
				return i
			}
		}
		for i := range fields {
			if fields[i].WasName != "" && fields[i].WasName == name {
				return i
			}
		}
		return -1
	}
}

// SealedNames returns the value names of the ciphertexts data as is,
// including those no longer in the model.
func SealedNames(data []byte) ([]string, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var js []struct {
		Field string `json:"field"`
	}
	if err := json.Unmarshal(data, &js); err != nil {
		return nil, fmt.Errorf("storekit: ciphertexts: %w", err)
	}
	names := make([]string, len(js))
	for i, x := range js {
		names[i] = x.Field
	}
	return names, nil
}

// EncodeSealed encodes the ciphertexts of a record's fields (a store column
// or document field); no ciphertexts — nil.
func EncodeSealed(fields []reasonr.FieldInfo, ss []Sealed) ([]byte, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	return json.Marshal(sealedJSON(ss, fieldName(fields)))
}

// DecodeSealed reads the ciphertexts of a record's fields.
func DecodeSealed(fields []reasonr.FieldInfo, data []byte) ([]Sealed, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var js []jsonSealed
	if err := json.Unmarshal(data, &js); err != nil {
		return nil, fmt.Errorf("storekit: ciphertexts: %w", err)
	}
	return sealedFrom(js, fieldIndex(fields)), nil
}

type jsonEvent struct {
	Name   string          `json:"name"`
	Seq    uint32          `json:"seq"`
	Args   json.RawMessage `json:"args"`
	Sealed []jsonSealed    `json:"sealed,omitempty"`
}

type jsonEntity struct {
	Type    string          `json:"type"`
	ID      json.RawMessage `json:"id"`
	Version uint64          `json:"version"`
	Events  []jsonEvent     `json:"events"`
}

type jsonOp struct {
	Op         string                     `json:"op"` // upsert, delete
	Projection string                     `json:"projection"`
	ID         json.RawMessage            `json:"id"`
	Seq        uint32                     `json:"seq"`
	Set        map[string]json.RawMessage `json:"set,omitempty"`
	Sealed     []jsonSealed               `json:"sealed,omitempty"`
}

type jsonRequest struct {
	System  string          `json:"system"`
	Request string          `json:"request"`
	Args    json.RawMessage `json:"args"`
	Key     string          `json:"key"` // a value without a schema (storekit), base64
	Sealed  []jsonSealed    `json:"sealed,omitempty"`
}

// jsonCall is a call of a model command by a fake (the test program).
type jsonCall struct {
	System  string          `json:"system"`
	Entity  string          `json:"entity"`
	ID      json.RawMessage `json:"id"`
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
}

type jsonSubject struct {
	Type string          `json:"type"`
	ID   json.RawMessage `json:"id"`
}

type jsonPayload struct {
	At          string        `json:"at"`
	Entities    []jsonEntity  `json:"entities,omitempty"`
	Projections []jsonOp      `json:"projections,omitempty"`
	Requests    []jsonRequest `json:"requests,omitempty"`
	Forget      []jsonSubject `json:"forget,omitempty"`
	Calls       []jsonCall    `json:"calls,omitempty"`
	Contexts    []string      `json:"contexts,omitempty"` // the ACL reactions of these bounded contexts are in the unit
}

func rawValue(c *codecs.Values, a *reasonr.Arena, t reasonr.Type, v reasonr.Value) (json.RawMessage, error) {
	b, err := c.Append(nil, a, t, v)
	return b, err
}

// IDType is the identifier type of the record t.
func IDType(t reasonr.TypeRef) reasonr.Type { return reasonr.Type{Kind: reasonr.KindID, Ref: t} }

// EncodePayload encodes the effects of a unit of work for the outbox;
// versions are the entity versions after commit by change number. pc ==
// nil — without encrypting personal values.
func EncodePayload(ctx context.Context, c *codecs.Values, pc *PayloadCrypto, u *kind.Unit, versions map[int]uint64) ([]byte, error) {
	a, s := u.Arena, c.Schema()
	p := jsonPayload{At: u.Now.UTC().Format(time.RFC3339Nano), Contexts: u.Contexts}
	for i := range u.Work.Entities {
		ch := &u.Work.Entities[i]
		if len(ch.Events) == 0 {
			continue
		}
		ti := s.Type(ch.Type)
		id, err := rawValue(c, a, IDType(ch.Type), ch.ID)
		if err != nil {
			return nil, err
		}
		e := jsonEntity{Type: ti.Name, ID: id, Version: versions[i]}
		for _, ev := range ch.Events {
			mi := &ti.Methods[ev.Name]
			args := ev.Args.Value()
			var sealed []Sealed
			if pc != nil {
				if args, sealed, err = pc.Crypto.Seal(ctx, pc.Sealer, a, ti.Name+"."+mi.Name, mi.Params, args,
					pc.Crypto.SubjectsOf(a, mi.Params, ch.State.Value(), ch.ID)); err != nil {
					return nil, err
				}
			}
			data, err := c.AppendFields(nil, a, mi.Params, args.Record())
			if err != nil {
				return nil, fmt.Errorf("storekit: event %s.%s: %w", ti.Name, mi.Name, err)
			}
			e.Events = append(e.Events, jsonEvent{Name: mi.Name, Seq: ev.Seq, Args: data, Sealed: sealedJSON(sealed, fieldName(mi.Params))})
		}
		p.Entities = append(p.Entities, e)
	}
	for _, op := range u.Async {
		ti := s.Type(op.Projection)
		var sealed []Sealed
		if pc != nil {
			var err error
			if op, sealed, err = pc.Crypto.SealOp(ctx, pc.Sealer, a, op); err != nil {
				return nil, err
			}
		}
		id, err := rawValue(c, a, IDType(op.Projection), op.ID)
		if err != nil {
			return nil, err
		}
		jo := jsonOp{Op: "upsert", Projection: ti.Name, ID: id, Seq: op.Seq}
		if op.Kind == reasonr.ProjectionDelete {
			jo.Op = "delete"
		} else {
			jo.Set = map[string]json.RawMessage{}
			for _, fv := range op.Set {
				f := &ti.Fields[fv.Field]
				v, err := rawValue(c, a, f.Type, fv.Value)
				if err != nil {
					return nil, err
				}
				jo.Set[f.Name] = v
			}
			jo.Sealed = sealedJSON(sealed, func(j int) string { return ti.Fields[op.Set[j].Field].Name })
		}
		p.Projections = append(p.Projections, jo)
	}
	for _, rq := range u.Work.Requests {
		ti := s.Type(rq.System)
		mi := &ti.Methods[rq.Request]
		args := rq.Args.Value()
		var sealed []Sealed
		if pc != nil {
			var err error
			if args, sealed, err = pc.Crypto.Seal(ctx, pc.Sealer, a, ti.Name+"."+mi.Name, mi.Params, args, pc.Crypto.ArgSubjects(a, rq.Subjects)); err != nil {
				return nil, err
			}
		}
		data, err := c.AppendFields(nil, a, mi.Params, args.Record())
		if err != nil {
			return nil, err
		}
		p.Requests = append(p.Requests, jsonRequest{System: ti.Name, Request: mi.Name, Args: data,
			Key: base64.StdEncoding.EncodeToString(AppendValue(nil, a, rq.Key)), Sealed: sealedJSON(sealed, fieldName(mi.Params))})
	}
	for _, subj := range u.Work.Forget {
		id, err := rawValue(c, a, IDType(subj.Type), subj.ID)
		if err != nil {
			return nil, err
		}
		p.Forget = append(p.Forget, jsonSubject{Type: s.Type(subj.Type).Name, ID: id})
	}
	for _, call := range u.Work.Calls {
		ti := s.Type(call.Entity)
		mi := &ti.Methods[call.Command]
		id, err := rawValue(c, a, IDType(call.Entity), call.ID)
		if err != nil {
			return nil, err
		}
		args, err := c.AppendFields(nil, a, mi.Params, call.Args)
		if err != nil {
			return nil, err
		}
		p.Calls = append(p.Calls, jsonCall{System: s.Type(call.System).Name, Entity: ti.Name, ID: id, Command: mi.Name, Args: args})
	}
	return json.Marshal(p)
}

// DecodePayload reads an outbox entry into a unit of work by the codec's
// schema and decrypts personal values (pc == nil — they stay none); it
// fills Subjects of events and external requests and Keys of the unit.
func DecodePayload(ctx context.Context, c *codecs.Values, pc *PayloadCrypto, data []byte, cu *kind.CommittedUnit) error {
	var p jsonPayload
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if pc != nil {
		if err := prefetch(ctx, pc.Sealer, &p); err != nil {
			return err
		}
	}
	a, s := cu.Arena, c.Schema()
	at, err := time.Parse(time.RFC3339Nano, p.At)
	if err != nil {
		return err
	}
	cu.At = at
	cu.Contexts = p.Contexts
	lookup := func(name string) (*reasonr.TypeInfo, error) {
		ti := s.Lookup(name)
		if ti == nil {
			return nil, fmt.Errorf("postgres: the model has no type %s", name)
		}
		return ti, nil
	}
	method := func(ti *reasonr.TypeInfo, name string) (*reasonr.MethodInfo, error) {
		mi := ti.Method(name)
		if mi == nil {
			return nil, fmt.Errorf("postgres: the model has no method %s.%s", ti.Name, name)
		}
		return mi, nil
	}
	for _, e := range p.Entities {
		ti, err := lookup(e.Type)
		if err != nil {
			return err
		}
		id, err := c.Decode(a, IDType(ti.Ref), e.ID)
		if err != nil {
			return err
		}
		ee := kind.EntityEvents{Type: ti.Ref, ID: id, Version: e.Version}
		for _, ev := range e.Events {
			mi, err := method(ti, ev.Name)
			if err != nil {
				return err
			}
			args, err := c.DecodeFields(a, 0, mi.Params, ev.Args)
			if err != nil {
				return err
			}
			e := reasonr.Event{Name: mi.Ref, Args: args, Seq: ev.Seq}
			if ss := sealedFrom(ev.Sealed, fieldIndex(mi.Params)); pc != nil && len(ss) > 0 {
				if err := pc.Crypto.Open(ctx, pc.Sealer, a, ti.Name+"."+mi.Name, mi.Params, args.Value(), ss); err != nil {
					return err
				}
				if e.Subjects, err = pc.Crypto.ArgSubjectsOf(a, ss); err != nil {
					return err
				}
			}
			ee.Events = append(ee.Events, e)
		}
		cu.Entities = append(cu.Entities, ee)
	}
	for _, jo := range p.Projections {
		ti, err := lookup(jo.Projection)
		if err != nil {
			return err
		}
		id, err := c.Decode(a, IDType(ti.Ref), jo.ID)
		if err != nil {
			return err
		}
		op := reasonr.ProjectionOp{Kind: reasonr.ProjectionUpsert, Projection: ti.Ref, ID: id, Seq: jo.Seq}
		if jo.Op == "delete" {
			op.Kind = reasonr.ProjectionDelete
		}
		for i := range ti.Fields { // in field order: deterministic
			raw, ok := jo.Set[ti.Fields[i].Name]
			if !ok {
				continue
			}
			v, err := c.Decode(a, ti.Fields[i].Type, raw)
			if err != nil {
				return err
			}
			op.Set = append(op.Set, reasonr.FieldValue{Field: ti.Fields[i].Ref, Value: v})
		}
		if ss := sealedFrom(jo.Sealed, func(name string) int {
			for j, fv := range op.Set {
				if ti.Fields[fv.Field].Name == name {
					return j
				}
			}
			return -1
		}); pc != nil && len(ss) > 0 {
			if err := pc.Crypto.OpenOp(ctx, pc.Sealer, a, op, ss); err != nil {
				return err
			}
		}
		cu.Projections = append(cu.Projections, op)
	}
	for _, jr := range p.Requests {
		ti, err := lookup(jr.System)
		if err != nil {
			return err
		}
		mi, err := method(ti, jr.Request)
		if err != nil {
			return err
		}
		args, err := c.DecodeFields(a, 0, mi.Params, jr.Args)
		if err != nil {
			return err
		}
		var subjects []reasonr.ArgSubject
		if ss := sealedFrom(jr.Sealed, fieldIndex(mi.Params)); pc != nil && len(ss) > 0 {
			if err := pc.Crypto.Open(ctx, pc.Sealer, a, ti.Name+"."+mi.Name, mi.Params, args.Value(), ss); err != nil {
				return err
			}
			if subjects, err = pc.Crypto.ArgSubjectsOf(a, ss); err != nil {
				return err
			}
		}
		kb, err := base64.StdEncoding.DecodeString(jr.Key)
		if err != nil {
			return err
		}
		key, err := DecodeValue(a, kb)
		if err != nil {
			return err
		}
		cu.Requests = append(cu.Requests, reasonr.ExternalRequest{System: ti.Ref, Request: mi.Ref, Args: args, Key: key, Subjects: subjects})
	}
	for _, js := range p.Forget {
		ti, err := lookup(js.Type)
		if err != nil {
			return err
		}
		id, err := c.Decode(a, IDType(ti.Ref), js.ID)
		if err != nil {
			return err
		}
		cu.Forget = append(cu.Forget, reasonr.Subject{Type: ti.Ref, ID: id})
	}
	for _, jc := range p.Calls {
		sys, err := lookup(jc.System)
		if err != nil {
			return err
		}
		ti, err := lookup(jc.Entity)
		if err != nil {
			return err
		}
		mi, err := method(ti, jc.Command)
		if err != nil {
			return err
		}
		id, err := c.Decode(a, IDType(ti.Ref), jc.ID)
		if err != nil {
			return err
		}
		args, err := c.DecodeFields(a, 0, mi.Params, jc.Args)
		if err != nil {
			return err
		}
		cu.Calls = append(cu.Calls, reasonr.FakeCall{System: sys.Ref, Entity: ti.Ref, ID: id, Command: mi.Ref, Args: args})
	}
	if pc != nil {
		cu.Keys = pc.Sealer.Opened()
	}
	return nil
}

// prefetch loads the subject keys of every ciphertext of an entry with one
// query.
func prefetch(ctx context.Context, s *pii.Sealer, p *jsonPayload) error {
	var refs []port.SubjectRef
	for _, e := range p.Entities {
		for _, ev := range e.Events {
			refs = appendRefs(refs, ev.Sealed)
		}
	}
	for _, op := range p.Projections {
		refs = appendRefs(refs, op.Sealed)
	}
	for _, rq := range p.Requests {
		refs = appendRefs(refs, rq.Sealed)
	}
	if len(refs) == 0 {
		return nil
	}
	return s.Prefetch(ctx, refs)
}
