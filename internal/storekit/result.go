package storekit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// A response by idempotency key.

type jsonError struct {
	Owner  string          `json:"owner,omitempty"` // "" — built-in
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args,omitempty"` // by the error's parameters
	Raw    string          `json:"raw,omitempty"`  // arguments without a schema (storekit): for built-in ones
	Sealed []jsonSealed    `json:"sealed,omitempty"`
}

type jsonNotification struct {
	Owner  string          `json:"owner"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	ToType string          `json:"toType"`
	To     json.RawMessage `json:"to"`
	Sealed []jsonSealed    `json:"sealed,omitempty"`
}

type jsonResult struct {
	Position      string             `json:"position"`
	IDType        string             `json:"idType,omitempty"` // a create command: the type of the issued identifier
	ID            json.RawMessage    `json:"id,omitempty"`
	Errors        []jsonError        `json:"errors,omitempty"`
	Notifications []jsonNotification `json:"notifications,omitempty"`
}

// errorParams are the parameters of a declared error or nil.
func errorParams(s *reasonr.Schema, be *reasonr.BusinessError) []reasonr.FieldInfo {
	if ti := s.Type(be.Owner); ti != nil {
		if mi := ti.Method(be.Name); mi != nil && mi.Kind == reasonr.MethodError {
			return mi.Params
		}
	}
	return nil
}

// EncodeResult encodes a response by idempotency key; the personal
// arguments of errors and notifications are encrypted with the keys of
// their subjects (RFC §20.2). pc == nil — without encryption.
func EncodeResult(ctx context.Context, c *codecs.Values, pc *PayloadCrypto, a *reasonr.Arena, pos port.Position, res *kind.CommandResult) ([]byte, error) {
	s := c.Schema()
	r := jsonResult{Position: strconv.FormatUint(uint64(pos), 10)}
	seal := func(owner string, params []reasonr.FieldInfo, args reasonr.Record, subjects []reasonr.ArgSubject) (json.RawMessage, []jsonSealed, error) {
		v := args.Value()
		var sealed []Sealed
		if pc != nil && len(subjects) > 0 {
			var err error
			if v, sealed, err = pc.Crypto.Seal(ctx, pc.Sealer, a, owner, params, v, pc.Crypto.ArgSubjects(a, subjects)); err != nil {
				return nil, nil, err
			}
		}
		data, err := c.AppendFields(nil, a, params, v.Record())
		return data, sealedJSON(sealed, fieldName(params)), err
	}
	for i := range res.Errors {
		be := &res.Errors[i]
		je := jsonError{Name: be.Name}
		if params := errorParams(s, be); params != nil {
			je.Owner = s.Type(be.Owner).Name
			args, sealed, err := seal(je.Owner+"."+be.Name, params, be.Args, be.Subjects)
			if err != nil {
				return nil, err
			}
			je.Args, je.Sealed = args, sealed
		} else {
			je.Raw = base64.StdEncoding.EncodeToString(AppendValue(nil, a, be.Args.Value()))
		}
		r.Errors = append(r.Errors, je)
	}
	for _, n := range res.Notifications {
		ti := s.Type(n.Owner)
		mi := &ti.Methods[n.Name]
		args, sealed, err := seal(ti.Name+"."+mi.Name, mi.Params, n.Args, n.Subjects)
		if err != nil {
			return nil, err
		}
		to, err := rawValue(c, a, IDType(n.To.Type), n.To)
		if err != nil {
			return nil, err
		}
		r.Notifications = append(r.Notifications, jsonNotification{Owner: ti.Name, Name: mi.Name, Args: args,
			ToType: s.Type(n.To.Type).Name, To: to, Sealed: sealed})
	}
	if res.ID.Kind == reasonr.KindID {
		id, err := rawValue(c, a, IDType(res.ID.Type), res.ID)
		if err != nil {
			return nil, err
		}
		r.IDType, r.ID = s.Type(res.ID.Type).Name, id
	}
	return json.Marshal(r)
}

// DecodeResult reads a response by idempotency key and decrypts the
// personal arguments: for forgotten subjects they are none (pc == nil —
// none too).
func DecodeResult(ctx context.Context, c *codecs.Values, pc *PayloadCrypto, a *reasonr.Arena, data []byte) (*kind.CommandResult, error) {
	var r jsonResult
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if pc != nil {
		var refs []port.SubjectRef
		for _, je := range r.Errors {
			refs = appendRefs(refs, je.Sealed)
		}
		for _, jn := range r.Notifications {
			refs = appendRefs(refs, jn.Sealed)
		}
		if len(refs) > 0 {
			if err := pc.Sealer.Prefetch(ctx, refs); err != nil {
				return nil, err
			}
		}
	}
	open := func(owner string, params []reasonr.FieldInfo, args reasonr.Record, js []jsonSealed) error {
		if ss := sealedFrom(js, fieldIndex(params)); pc != nil && len(ss) > 0 {
			return pc.Crypto.Open(ctx, pc.Sealer, a, owner, params, args.Value(), ss)
		}
		return nil
	}
	pos, err := strconv.ParseUint(r.Position, 10, 64)
	if err != nil {
		return nil, err
	}
	s := c.Schema()
	res := &kind.CommandResult{Position: port.Position(pos), Arena: a, ID: reasonr.None}
	if r.IDType != "" {
		ti := s.Lookup(r.IDType)
		if ti == nil {
			return nil, fmt.Errorf("the model has no type %s", r.IDType)
		}
		if res.ID, err = c.Decode(a, IDType(ti.Ref), r.ID); err != nil {
			return nil, err
		}
	}
	for _, je := range r.Errors {
		be := reasonr.BusinessError{Name: je.Name, Args: a.Record(0)}
		if je.Owner != "" {
			ti := s.Lookup(je.Owner)
			if ti == nil {
				return nil, fmt.Errorf("postgres: the model has no type %s", je.Owner)
			}
			be.Owner = ti.Ref
			if params := errorParams(s, &be); params != nil {
				if be.Args, err = c.DecodeFields(a, 0, params, je.Args); err != nil {
					return nil, err
				}
				if err := open(je.Owner+"."+je.Name, params, be.Args, je.Sealed); err != nil {
					return nil, err
				}
			}
		} else if je.Raw != "" {
			b, err := base64.StdEncoding.DecodeString(je.Raw)
			if err != nil {
				return nil, err
			}
			v, err := DecodeValue(a, b)
			if err != nil {
				return nil, err
			}
			be.Args = v.Record()
		}
		res.Errors = append(res.Errors, be)
	}
	for _, jn := range r.Notifications {
		ti, tt := s.Lookup(jn.Owner), s.Lookup(jn.ToType)
		if ti == nil || tt == nil {
			return nil, fmt.Errorf("postgres: the model has no type %s or %s", jn.Owner, jn.ToType)
		}
		mi := ti.Method(jn.Name)
		if mi == nil {
			return nil, fmt.Errorf("postgres: the model has no notification %s.%s", jn.Owner, jn.Name)
		}
		args, err := c.DecodeFields(a, 0, mi.Params, jn.Args)
		if err != nil {
			return nil, err
		}
		if err := open(jn.Owner+"."+jn.Name, mi.Params, args, jn.Sealed); err != nil {
			return nil, err
		}
		to, err := c.Decode(a, IDType(tt.Ref), jn.To)
		if err != nil {
			return nil, err
		}
		res.Notifications = append(res.Notifications, reasonr.Notification{Owner: ti.Ref, Name: mi.Ref, Args: args, To: to})
	}
	return res, nil
}

// appendRefs appends the subjects of ciphertexts.
func appendRefs(refs []port.SubjectRef, js []jsonSealed) []port.SubjectRef {
	for _, x := range js {
		for _, r := range x.Subjects {
			refs = append(refs, port.SubjectRef{Type: r.Type, ID: string(r.ID)})
		}
	}
	return refs
}
