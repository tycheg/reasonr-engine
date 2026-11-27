package storekit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tycheg/reasonr-engine/internal/codecs"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/port"
)

// Sealed is an encrypted personal value of the field or parameter Field
// (RFC §20): the value's canonical JSON encrypted with the keys of the
// subjects Subjects in turn. The store keeps the record without this value
// (none) and the ciphertexts next to it.
type Sealed struct {
	Field    int
	Subjects []port.SubjectRef
	Data     []byte
	// Member is the name the value is encrypted under (part of the AAD):
	// for one read from the store — the stored one, for a field renamed with
	// was — the former one; "" — the current name of Field.
	Member string
}

// member returns the value name for the AAD.
func (sv *Sealed) member(fields []reasonr.FieldInfo) string {
	if sv.Member != "" {
		return sv.Member
	}
	return fields[sv.Field].Name
}

// Renamed reports whether the value is encrypted under the former field
// name (was): a migration reseals it under the current one.
func (sv *Sealed) Renamed(fields []reasonr.FieldInfo) bool {
	return sv.Member != "" && sv.Member != fields[sv.Field].Name
}

// Crypto encrypts the personal data of model records: code shared by the
// stores (memory, PostgreSQL, MongoDB).
type Crypto struct {
	schema *reasonr.Schema
	json   *codecs.Values
	rec    *reasonr.RecordCodec
}

// NewCrypto returns the encryption for a schema.
func NewCrypto(s *reasonr.Schema) *Crypto {
	return &Crypto{schema: s, json: codecs.JSONOf(s, codecs.Options{}), rec: reasonr.NewRecordCodec(s)}
}

// ErrNoSubject: a personal value has no subject (the subject field is
// none): it cannot be stored under nobody's key, the commit is rejected.
var ErrNoSubject = errors.New("the personal value has no subject")

// Ref returns a subject for the key store: the type name and the
// identifier's canonical JSON.
func (c *Crypto) Ref(a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (port.SubjectRef, error) {
	ti := c.schema.Type(t)
	if ti == nil {
		return port.SubjectRef{}, fmt.Errorf("storekit: unknown subject type %d", t)
	}
	b, err := c.json.Append(nil, a, IDType(t), id)
	if err != nil {
		return port.SubjectRef{}, err
	}
	return port.SubjectRef{Type: ti.Name, ID: string(b)}, nil
}

// Refs returns subjects for the key store without a Crypto instance: the
// forget journal and broadcasting the forgotten through the hub.
func Refs(s *reasonr.Schema, a *reasonr.Arena, subjects []reasonr.Subject) ([]port.SubjectRef, error) {
	c := codecs.JSONOf(s, codecs.Options{})
	out := make([]port.SubjectRef, len(subjects))
	for i, subj := range subjects {
		ti := s.Type(subj.Type)
		if ti == nil {
			return nil, fmt.Errorf("storekit: unknown subject type %d", subj.Type)
		}
		id, err := c.Append(nil, a, IDType(subj.Type), subj.ID)
		if err != nil {
			return nil, err
		}
		out[i] = port.SubjectRef{Type: ti.Name, ID: string(id)}
	}
	return out, nil
}

// Subject returns a subject by a key store reference.
func (c *Crypto) Subject(a *reasonr.Arena, ref port.SubjectRef) (reasonr.Subject, error) {
	ti := c.schema.Lookup(ref.Type)
	if ti == nil {
		return reasonr.Subject{}, fmt.Errorf("storekit: the model has no subject type %s", ref.Type)
	}
	id, err := c.json.Decode(a, IDType(ti.Ref), []byte(ref.ID))
	if err != nil {
		return reasonr.Subject{}, fmt.Errorf("storekit: subject %s %s: %w", ref.Type, ref.ID, err)
	}
	return reasonr.Subject{Type: ti.Ref, ID: id}, nil
}

// ArgSubjectsOf returns the subjects of arguments by their ciphertexts.
func (c *Crypto) ArgSubjectsOf(a *reasonr.Arena, sealed []Sealed) ([]reasonr.ArgSubject, error) {
	var out []reasonr.ArgSubject
	for _, sv := range sealed {
		for _, ref := range sv.Subjects {
			subj, err := c.Subject(a, ref)
			if err != nil {
				return nil, err
			}
			out = append(out, reasonr.ArgSubject{Arg: sv.Field, Subject: subj})
		}
	}
	return out, nil
}

// SubjectsOf returns the subject function of the personal fields fields of
// a record with the state state and the identifier id (the subject field is looked up in state).
func (c *Crypto) SubjectsOf(a *reasonr.Arena, fields []reasonr.FieldInfo, state, id reasonr.Value) func(i int) ([]port.SubjectRef, error) {
	return func(i int) ([]port.SubjectRef, error) {
		p := fields[i].Personal
		if p == nil {
			return nil, nil
		}
		t, v, ok := Subject(a, state, id, p)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNoSubject, fields[i].Name)
		}
		ref, err := c.Ref(a, t, v)
		if err != nil {
			return nil, err
		}
		return []port.SubjectRef{ref}, nil
	}
}

// ArgSubjects returns the subject function of arguments by the subjects
// from the unit of work (external requests, notifications, errors; RFC
// §20.2, rule 7).
func (c *Crypto) ArgSubjects(a *reasonr.Arena, ss []reasonr.ArgSubject) func(i int) ([]port.SubjectRef, error) {
	return func(i int) ([]port.SubjectRef, error) {
		var out []port.SubjectRef
		for _, s := range ss {
			if s.Arg != i {
				continue
			}
			ref, err := c.Ref(a, s.Subject.Type, s.Subject.ID)
			if err != nil {
				return nil, err
			}
			out = append(out, ref)
		}
		return out, nil
	}
}

// Seal encrypts the personal values of the record rec (the fields or
// parameters fields of the owner type owner): it returns a copy of rec in
// the arena a where they are none, and the ciphertexts. subjects(i) gives
// the subjects of value i; nil — the value is not personal. A value that
// the resealer (pii.Keyring.Resealer) cannot encrypt with the previous key
// becomes none in rec itself too: no blind values are built from it.
func (c *Crypto) Seal(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, owner string, fields []reasonr.FieldInfo, rec reasonr.Value,
	subjects func(i int) ([]port.SubjectRef, error)) (reasonr.Value, []Sealed, error) {
	var sealed []Sealed
	var strip []int
	vals := a.Elems(rec)
	for i := range vals {
		if i >= len(fields) || vals[i].Kind == reasonr.KindNone {
			continue
		}
		refs, err := subjects(i)
		if err != nil {
			return reasonr.None, nil, fmt.Errorf("storekit: %s.%s: %w", owner, fields[i].Name, err)
		}
		if len(refs) == 0 {
			continue
		}
		data, err := c.SealValue(ctx, s, a, fields[i].Type, a.Elems(rec)[i], refs, owner, fields[i].Name)
		if errors.Is(err, pii.ErrKeyGone) {
			a.Elems(rec)[i] = reasonr.None
			continue
		}
		if err != nil {
			return reasonr.None, nil, err
		}
		sealed = append(sealed, Sealed{Field: i, Subjects: refs, Data: data})
		strip = append(strip, i)
	}
	if len(strip) == 0 {
		return rec, nil, nil
	}
	return Strip(a, rec, strip), sealed, nil
}

// Strip returns a copy of the record rec where the values fields are none.
func Strip(a *reasonr.Arena, rec reasonr.Value, fields []int) reasonr.Value {
	m := a.Mark()
	for i, v := range a.Elems(rec) {
		for _, f := range fields {
			if f == i {
				v = reasonr.None
				break
			}
		}
		a.Push(v)
	}
	return a.End(reasonr.KindRecord, rec.Type, m)
}

// Open decrypts the ciphertexts sealed into the record rec (in the arena a,
// in place): values whose keys are destroyed stay none.
func (c *Crypto) Open(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, owner string, fields []reasonr.FieldInfo, rec reasonr.Value, sealed []Sealed) error {
	for _, sv := range sealed {
		if sv.Field >= len(fields) {
			return fmt.Errorf("storekit: a ciphertext of field %d of %s with %d fields", sv.Field, owner, len(fields))
		}
		v, err := c.OpenValue(ctx, s, a, fields[sv.Field].Type, sv.Subjects, owner, sv.member(fields), sv.Data)
		if err != nil {
			return err
		}
		a.Elems(rec)[sv.Field] = v // Elems again: decryption may have allocated
	}
	return nil
}

// SealOp encrypts the personal values of a projection operation: the
// subject comes from the projection fields after the operation (E0906
// guarantees that the subject field is set together with the personal
// one). The ciphertext's Field is the number in op.Set.
func (c *Crypto) SealOp(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, op reasonr.ProjectionOp) (reasonr.ProjectionOp, []Sealed, error) {
	if op.Kind != reasonr.ProjectionUpsert {
		return op, nil, nil
	}
	ti := c.schema.Type(op.Projection)
	var sealed []Sealed
	var subjects func(int) ([]port.SubjectRef, error)
	set := op.Set
	for j, fv := range op.Set {
		fi := &ti.Fields[fv.Field]
		if fi.Personal == nil || fv.Value.Kind == reasonr.KindNone {
			continue
		}
		if subjects == nil {
			full, err := ProjectionRow(c.rec, a, &op)
			if err != nil {
				return op, nil, err
			}
			subjects = c.SubjectsOf(a, ti.Fields, full, op.ID)
			set = append([]reasonr.FieldValue(nil), op.Set...)
		}
		refs, err := subjects(int(fv.Field))
		if err != nil {
			return op, nil, fmt.Errorf("storekit: %s.%s: %w", ti.Name, fi.Name, err)
		}
		data, err := c.SealValue(ctx, s, a, fi.Type, fv.Value, refs, ti.Name, fi.Name)
		set[j].Value = reasonr.None
		if errors.Is(err, pii.ErrKeyGone) {
			continue
		}
		if err != nil {
			return op, nil, err
		}
		sealed = append(sealed, Sealed{Field: j, Subjects: refs, Data: data})
	}
	op.Set = set
	return op, sealed, nil
}

// OpenOp decrypts the operation's ciphertexts (Field is the number in
// op.Set) in place: op.Set must belong to the caller.
func (c *Crypto) OpenOp(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, op reasonr.ProjectionOp, sealed []Sealed) error {
	ti := c.schema.Type(op.Projection)
	for _, sv := range sealed {
		if sv.Field >= len(op.Set) {
			return fmt.Errorf("storekit: a ciphertext of operation %d of %s with %d values", sv.Field, ti.Name, len(op.Set))
		}
		fi := &ti.Fields[op.Set[sv.Field].Field]
		member := fi.Name
		if sv.Member != "" {
			member = sv.Member
		}
		v, err := c.OpenValue(ctx, s, a, fi.Type, sv.Subjects, ti.Name, member, sv.Data)
		if err != nil {
			return err
		}
		op.Set[sv.Field].Value = v
	}
	return nil
}

// SealValue encrypts the value v of type t with the keys refs in turn.
func (c *Crypto) SealValue(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, t reasonr.Type, v reasonr.Value,
	refs []port.SubjectRef, owner, member string) ([]byte, error) {
	data, err := c.json.Append(nil, a, t, v)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if data, err = s.Seal(ctx, nil, ref, owner, member, data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// OpenValue decrypts a value of type t; a destroyed key gives none.
func (c *Crypto) OpenValue(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, t reasonr.Type,
	refs []port.SubjectRef, owner, member string, data []byte) (reasonr.Value, error) {
	for i := len(refs) - 1; i >= 0; i-- {
		plain, ok, err := s.Open(ctx, nil, refs[i], owner, member, data)
		if errors.Is(err, pii.ErrTampered) {
			return reasonr.None, fmt.Errorf("storekit: %s.%s: %w", owner, member, err)
		}
		if err != nil || !ok {
			return reasonr.None, err
		}
		data = plain
	}
	return c.json.Decode(a, t, data)
}

// SubjectRefs returns the subjects of ciphertexts without repeats.
func SubjectRefs(sealed []Sealed) []port.SubjectRef {
	var out []port.SubjectRef
	for _, sv := range sealed {
	next:
		for _, r := range sv.Subjects {
			for _, o := range out {
				if o == r {
					continue next
				}
			}
			out = append(out, r)
		}
	}
	return out
}

// Prefetch loads the subject keys of ciphertexts with one query.
func Prefetch(ctx context.Context, s *pii.Sealer, sealed ...[]Sealed) error {
	var refs []port.SubjectRef
	for _, ss := range sealed {
		refs = append(refs, SubjectRefs(ss)...)
	}
	if len(refs) == 0 {
		return nil
	}
	return s.Prefetch(ctx, refs)
}

// Personal reports whether the root of the path path in the type t is
// personal: the index keys over such a field are blind values (RFC §20).
func (c *Crypto) Personal(t reasonr.TypeRef, path reasonr.FieldPath) bool {
	ti := c.schema.Type(t)
	if ti == nil || len(path) == 0 || path[0] == reasonr.IDField || int(path[0]) >= len(ti.Fields) {
		return false
	}
	return ti.Fields[path[0]].Personal != nil
}

// BlindName is the name of a blind index field: "Type.path".
func (c *Crypto) BlindName(t reasonr.TypeRef, path reasonr.FieldPath) string {
	var b strings.Builder
	b.WriteString(c.schema.Type(t).Name)
	owner := t
	for _, f := range path {
		b.WriteByte('.')
		if owner == 0 || f == reasonr.IDField {
			fmt.Fprint(&b, f)
			owner = 0
			continue
		}
		fi := c.schema.Type(owner).Fields[f]
		b.WriteString(fi.Name)
		owner = 0
		if fi.Type.Kind == reasonr.KindRecord {
			owner = fi.Type.Ref
		}
	}
	return b.String()
}

// Blind returns the blind value of v of the field path of type t.
func (c *Crypto) Blind(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, t reasonr.TypeRef, path reasonr.FieldPath, v reasonr.Value) (string, error) {
	ft, err := FieldType(c.schema, t, path)
	if err != nil {
		return "", err
	}
	data, err := c.json.Append(nil, a, ft, v)
	if err != nil {
		return "", err
	}
	b, err := s.Blind(ctx, c.BlindName(t, path), data)
	return string(b[:]), err
}

// BlindCond is a search condition on a personal field: the path and the
// blind values it is compared with (== — one, in — each).
type BlindCond struct {
	Field  reasonr.FieldPath
	Values []string
}

// SplitConds splits the search conditions of type t into ordinary ones and
// conditions on personal fields with blind values (only == and in, E0905).
func (c *Crypto) SplitConds(ctx context.Context, s *pii.Sealer, a *reasonr.Arena, t reasonr.TypeRef, where []reasonr.Cond) ([]reasonr.Cond, []BlindCond, error) {
	var plain []reasonr.Cond
	var blind []BlindCond
	for _, cd := range where {
		if !c.Personal(t, cd.Field) {
			plain = append(plain, cd)
			continue
		}
		bc := BlindCond{Field: cd.Field}
		var vals []reasonr.Value
		switch cd.Op {
		case reasonr.OpEq:
			vals = []reasonr.Value{cd.Value}
		case reasonr.OpIn:
			vals = a.Elems(cd.Value)
		default:
			return nil, nil, fmt.Errorf("storekit: condition %d on the personal field %s: equality search only", cd.Op, c.BlindName(t, cd.Field))
		}
		for _, v := range vals {
			if v.Kind == reasonr.KindNone {
				continue
			}
			b, err := c.Blind(ctx, s, a, t, cd.Field, v)
			if err != nil {
				return nil, nil, err
			}
			bc.Values = append(bc.Values, b)
		}
		blind = append(blind, bc)
	}
	return plain, blind, nil
}

// NeedsReseal reports whether the storage of a record diverges from the
// model (RFC §20): state has a value of a personal field (the field became
// personal or was written before encryption), or the ciphertexts sealed
// have a field that is not personal, renamed (was) or removed.
func NeedsReseal(a *reasonr.Arena, fields []reasonr.FieldInfo, state reasonr.Value, sealed []byte) (bool, error) {
	for i, v := range a.Elems(state) {
		if i < len(fields) && fields[i].Personal != nil && v.Kind != reasonr.KindNone {
			return true, nil
		}
	}
	names, err := SealedNames(sealed)
	if err != nil {
		return false, err
	}
next:
	for _, n := range names {
		for i := range fields {
			if fields[i].Name == n {
				if fields[i].Personal == nil {
					return true, nil
				}
				continue next
			}
		}
		return true, nil
	}
	return false, nil
}
