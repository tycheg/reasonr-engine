package rows

import (
	"fmt"
	"math/big"
	"strings"
	"unicode"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/port"
)

// The layout of a model in a row store (RFC §31.2): a row per entity or
// projection row — the state (canonical JSON without personal values) and
// columns only for index keys. A column's kind keeps the model's order in
// the store: strings bytewise, timestamps by nanoseconds, enumerations by
// number.

// keyCol is an index key column.
type keyCol struct {
	path reasonr.FieldPath
	port.Column
	typ reasonr.Type
}

// table is the layout of an entity or projection type.
type table struct {
	ti     *reasonr.TypeInfo
	port   port.Table
	keys   []keyCol
	byPath map[string]int // the key column by path
}

// model is a schema with its codecs and table layouts.
type model struct {
	s      *reasonr.Schema
	json   *codecs.Values
	rec    *reasonr.RecordCodec
	crypto *storekit.Crypto
	tables []*table // by TypeRef-1; nil — not stored
	byName map[string]*table
	// personal: the model has personal fields or parameters: units of work
	// are encrypted, the outbox is decrypted.
	personal bool
}

func newModel(s *reasonr.Schema) (*model, error) {
	m := &model{s: s, json: codecs.JSONOf(s, codecs.Options{IgnoreUnknown: true, SealedPersonal: true}),
		rec: reasonr.NewRecordCodec(s), crypto: storekit.NewCrypto(s), tables: make([]*table, len(s.Types)),
		byName: map[string]*table{}, personal: hasPersonal(s)}
	for i := range s.Types {
		ti := &s.Types[i]
		if !ti.Stored() {
			continue
		}
		t, err := layout(s, ti)
		if err != nil {
			return nil, err
		}
		m.tables[i] = t
		m.byName[ti.Name] = t
	}
	return m, nil
}

// Layout is the store layout of the model named name.
func (m *model) layout(name string) (*port.Layout, error) {
	data, err := m.s.MarshalIndent()
	if err != nil {
		return nil, err
	}
	l := &port.Layout{Model: name, Schema: data}
	for _, t := range m.tables {
		if t != nil {
			l.Tables = append(l.Tables, t.port)
		}
	}
	return l, nil
}

// table returns the layout of a type.
func (m *model) table(t reasonr.TypeRef) (*table, error) {
	if t == 0 || int(t) > len(m.tables) || m.tables[t-1] == nil {
		return nil, fmt.Errorf("rows: type %d is not stored", t)
	}
	return m.tables[t-1], nil
}

// Snake is a name in snake_case; a full name (Ordering.Order) has its parts
// joined with "__": ordering__order.
func Snake(s string) string {
	if strings.Contains(s, ".") {
		parts := strings.Split(s, ".")
		for i, p := range parts {
			parts[i] = Snake(p)
		}
		return strings.Join(parts, "__")
	}
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// hasPersonal reports whether the schema has personal fields or parameters.
func hasPersonal(s *reasonr.Schema) bool {
	for i := range s.Types {
		ti := &s.Types[i]
		if personalFields(ti) {
			return true
		}
		for j := range ti.Methods {
			for k := range ti.Methods[j].Params {
				if ti.Methods[j].Params[k].Personal != nil {
					return true
				}
			}
		}
	}
	return false
}

func personalFields(ti *reasonr.TypeInfo) bool {
	for i := range ti.Fields {
		if ti.Fields[i].Personal != nil {
			return true
		}
	}
	return false
}

func layout(s *reasonr.Schema, ti *reasonr.TypeInfo) (*table, error) {
	t := &table{ti: ti, byPath: map[string]int{},
		port: port.Table{Type: ti.Name, Projection: ti.Kind == reasonr.TypeProjection, Ordered: true, Personal: personalFields(ti)}}
	t.port.ID = kindOf(s, reasonr.Type{Kind: reasonr.KindID, Ref: ti.Ref})
	if t.port.ID == port.ColOpaque {
		t.port.Ordered = false
	}
	for _, in := range ti.Indexes {
		ix := port.Index{Name: Snake(in.Name), Unique: in.Unique}
		for _, k := range in.Keys {
			col, err := t.column(s, k.Field)
			if err != nil {
				return nil, fmt.Errorf("rows: index %s.%s: %w", ti.Name, in.Name, err)
			}
			ix.Columns = append(ix.Columns, col)
			ix.Desc = append(ix.Desc, k.Desc)
		}
		t.port.Indexes = append(t.port.Indexes, ix)
	}
	t.port.Keys = make([]port.Column, len(t.keys))
	for i := range t.keys {
		t.port.Keys[i] = t.keys[i].Column
	}
	return t, nil
}

// column returns the column name of a path, adding a key column.
func (t *table) column(s *reasonr.Schema, path reasonr.FieldPath) (string, error) {
	if len(path) == 1 && path[0] == reasonr.IDField {
		return "id", nil
	}
	if i, ok := t.byPath[pathKey(path)]; ok {
		return t.keys[i].Name, nil
	}
	typ, names, err := fieldPath(s, t.ti, path)
	if err != nil {
		return "", err
	}
	c := keyCol{path: path, typ: typ, Column: port.Column{Name: "k_" + strings.Join(names, "__"), Kind: kindOf(s, typ)}}
	if f := &t.ti.Fields[path[0]]; f.Personal != nil {
		c.Kind, c.Field = port.ColBlind, f.Name
	}
	if c.Kind == port.ColOpaque {
		t.port.Ordered = false
	}
	t.keys = append(t.keys, c)
	t.byPath[pathKey(path)] = len(t.keys) - 1
	return c.Name, nil
}

func pathKey(p reasonr.FieldPath) string { return fmt.Sprint([]reasonr.FieldRef(p)) }

// fieldPath returns the value type at a path and the field names (snake_case).
func fieldPath(s *reasonr.Schema, ti *reasonr.TypeInfo, path reasonr.FieldPath) (reasonr.Type, []string, error) {
	typ := reasonr.Type{Kind: reasonr.KindRecord, Ref: ti.Ref}
	var names []string
	for _, f := range path {
		cur := s.Type(typ.Ref)
		if typ.Kind != reasonr.KindRecord || cur == nil || int(f) >= len(cur.Fields) {
			return typ, nil, fmt.Errorf("bad path %v", path)
		}
		names = append(names, Snake(cur.Fields[f].Name))
		typ = cur.Fields[f].Type
	}
	return typ, names, nil
}

// kindOf is the column kind of a value of type t.
func kindOf(s *reasonr.Schema, t reasonr.Type) port.ColumnKind {
	switch t.Kind {
	case reasonr.KindInt, reasonr.KindBigint, reasonr.KindDate, reasonr.KindTime, reasonr.KindDuration, reasonr.KindEnum:
		return port.ColInt
	case reasonr.KindFloat:
		return port.ColFloat
	case reasonr.KindDecimal:
		return port.ColNumeric
	case reasonr.KindTimestamp:
		return port.ColNanos
	case reasonr.KindString:
		return port.ColText
	case reasonr.KindBytes:
		return port.ColBytes
	case reasonr.KindBool:
		return port.ColBool
	case reasonr.KindUUID, reasonr.KindTimerID:
		return port.ColUUID
	case reasonr.KindID:
		ti := s.Type(t.Ref)
		for range 64 {
			if ti == nil || ti.ID != reasonr.IDDerived {
				break
			}
			ti = s.Type(ti.IDType)
		}
		switch {
		case ti == nil:
			return port.ColOpaque
		case ti.ID == reasonr.IDUUID:
			return port.ColUUID
		case ti.ID == reasonr.IDSequence:
			return port.ColInt
		case ti.ID == reasonr.IDGiven && ti.IDType == 0:
			return port.ColText
		}
	}
	return port.ColOpaque
}

// scalar is the value of a column of kind k for the store; none — nil.
func (m *model) scalar(a *reasonr.Arena, k port.ColumnKind, t reasonr.Type, v reasonr.Value) (port.Scalar, error) {
	if v.Kind == reasonr.KindNone {
		return nil, nil
	}
	switch k {
	case port.ColInt:
		return int64(v.Lo), nil
	case port.ColFloat:
		return v.Float(), nil
	case port.ColNumeric:
		return v.Decimal().String(), nil
	case port.ColNanos:
		n := new(big.Int).Mul(big.NewInt(int64(v.Lo)), big.NewInt(1e9))
		return n.Add(n, big.NewInt(int64(v.Hi))).String(), nil
	case port.ColText:
		if v.Kind == reasonr.KindID {
			v = a.Elems(v)[0]
		}
		return string(a.Data(v)), nil
	case port.ColBytes:
		return append([]byte(nil), a.Data(v)...), nil
	case port.ColBool:
		return v.Bool(), nil
	case port.ColUUID:
		return v.UUID(), nil
	}
	t.Optional = true
	return m.rec.Append(nil, a, t, v)
}

// idScalar is the value of a row's id column.
func (m *model) idScalar(t *table, a *reasonr.Arena, id reasonr.Value) (port.Scalar, error) {
	return m.scalar(a, t.port.ID, reasonr.Type{Kind: reasonr.KindID, Ref: t.ti.Ref}, id)
}

// idValue builds an identifier from the value of the id column.
func (m *model) idValue(t *table, a *reasonr.Arena, raw port.Scalar) (reasonr.Value, error) {
	ref := t.ti.Ref
	switch x := raw.(type) {
	case [16]byte:
		return reasonr.ID(ref, reasonr.UUID(x)), nil
	case int64:
		return reasonr.ID(ref, reasonr.Bigint(x)), nil
	case string:
		return a.ID(ref, a.String(x)), nil
	case []byte:
		return m.rec.Decode(a, reasonr.Type{Kind: reasonr.KindID, Ref: ref}, x)
	}
	return reasonr.None, fmt.Errorf("rows: id of %s of kind %T", t.ti.Name, raw)
}

// col returns the column of a path: id or a key column.
func (t *table) col(path reasonr.FieldPath) (name string, kind port.ColumnKind, typ reasonr.Type, ok bool) {
	if len(path) == 1 && path[0] == reasonr.IDField {
		return "id", t.port.ID, reasonr.Type{Kind: reasonr.KindID, Ref: t.ti.Ref}, true
	}
	i, ok := t.byPath[pathKey(path)]
	if !ok {
		return "", 0, reasonr.Type{}, false
	}
	k := &t.keys[i]
	return k.Name, k.Kind, k.typ, true
}
