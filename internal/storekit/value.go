package storekit

import (
	"encoding/binary"
	"errors"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// Encoding a value without a schema: the kind, the type number, a scalar or
// nested values. Components that pass values between instances of one model
// version and do not know their type in advance use it (the notification
// hub). Data must not be stored this way: type numbers change between
// model versions — runtime/record and runtime/jsonv are for storage.

const maxDepth = 64

// ErrValue: the data is not an encoded value.
var ErrValue = errors.New("storekit: a corrupt value")

// AppendValue appends the value v from the arena a.
func AppendValue(dst []byte, a *reasonr.Arena, v reasonr.Value) []byte {
	dst = append(dst, byte(v.Kind))
	switch v.Kind {
	case reasonr.KindNone:
	case reasonr.KindString, reasonr.KindBytes:
		d := a.Data(v)
		dst = binary.AppendUvarint(dst, uint64(len(d)))
		dst = append(dst, d...)
	case reasonr.KindID:
		dst = binary.AppendUvarint(dst, uint64(v.Type))
		if v.Boxed() {
			dst = append(dst, 1)
			return AppendValue(dst, a, a.Elems(v)[0])
		}
		dst = append(dst, 0)
		dst = binary.AppendUvarint(dst, v.Lo)
		dst = binary.AppendUvarint(dst, v.Hi)
	case reasonr.KindRecord, reasonr.KindList, reasonr.KindSet, reasonr.KindMap, reasonr.KindPage, reasonr.KindPageRequest:
		dst = binary.AppendUvarint(dst, uint64(v.Type))
		elems := a.Elems(v)
		dst = binary.AppendUvarint(dst, uint64(len(elems)))
		for _, e := range elems {
			dst = AppendValue(dst, a, e)
		}
	default:
		dst = binary.AppendUvarint(dst, uint64(v.Type))
		dst = binary.AppendUvarint(dst, v.Lo)
		dst = binary.AppendUvarint(dst, v.Hi)
	}
	return dst
}

// DecodeValue reads a value into the arena a; data must be read whole.
func DecodeValue(a *reasonr.Arena, data []byte) (reasonr.Value, error) {
	d := valueDecoder{a: a, data: data}
	v, err := d.value(0)
	if err == nil && d.pos != len(d.data) {
		err = ErrValue
	}
	return v, err
}

type valueDecoder struct {
	a    *reasonr.Arena
	data []byte
	pos  int
}

func (d *valueDecoder) uvarint() (uint64, error) {
	x, n := binary.Uvarint(d.data[d.pos:])
	if n <= 0 {
		return 0, ErrValue
	}
	d.pos += n
	return x, nil
}

func (d *valueDecoder) value(depth int) (reasonr.Value, error) {
	if depth > maxDepth || d.pos >= len(d.data) {
		return reasonr.None, ErrValue
	}
	kind := reasonr.Kind(d.data[d.pos])
	d.pos++
	switch kind {
	case reasonr.KindNone:
		return reasonr.None, nil
	case reasonr.KindString, reasonr.KindBytes:
		n, err := d.uvarint()
		if err != nil || n > uint64(len(d.data)-d.pos) {
			return reasonr.None, ErrValue
		}
		b := d.data[d.pos : d.pos+int(n)]
		d.pos += int(n)
		if kind == reasonr.KindBytes {
			return d.a.Bytes(b), nil
		}
		return d.a.StringBytes(b), nil
	case reasonr.KindID:
		t, err := d.uvarint()
		if err != nil || d.pos >= len(d.data) {
			return reasonr.None, ErrValue
		}
		boxed := d.data[d.pos]
		d.pos++
		if boxed == 1 {
			inner, err := d.value(depth + 1)
			if err != nil {
				return reasonr.None, err
			}
			return d.a.ID(reasonr.TypeRef(t), inner), nil
		}
		lo, err1 := d.uvarint()
		hi, err2 := d.uvarint()
		if err1 != nil || err2 != nil || boxed != 0 {
			return reasonr.None, ErrValue
		}
		return reasonr.Value{Kind: kind, Type: reasonr.TypeRef(t), Lo: lo, Hi: hi}, nil
	case reasonr.KindRecord, reasonr.KindList, reasonr.KindSet, reasonr.KindMap, reasonr.KindPage, reasonr.KindPageRequest:
		t, err := d.uvarint()
		if err != nil {
			return reasonr.None, err
		}
		n, err := d.uvarint()
		if err != nil || n > uint64(len(d.data)-d.pos) {
			return reasonr.None, ErrValue
		}
		m := d.a.Mark()
		for range n {
			e, err := d.value(depth + 1)
			if err != nil {
				d.a.Drop(m)
				return reasonr.None, err
			}
			d.a.Push(e)
		}
		return d.a.End(kind, reasonr.TypeRef(t), m), nil
	}
	if kind > reasonr.KindPageRequest {
		return reasonr.None, ErrValue
	}
	t, err := d.uvarint()
	if err != nil {
		return reasonr.None, err
	}
	lo, err1 := d.uvarint()
	hi, err2 := d.uvarint()
	if err1 != nil || err2 != nil {
		return reasonr.None, ErrValue
	}
	return reasonr.Value{Kind: kind, Type: reasonr.TypeRef(t), Lo: lo, Hi: hi}, nil
}
