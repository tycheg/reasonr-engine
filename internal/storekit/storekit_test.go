package storekit

import (
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
)

func TestValueRoundTrip(t *testing.T) {
	a := reasonr.NewArena()
	m := a.Mark()
	a.Push(a.String("k"))
	a.Push(reasonr.Int(-1))
	mp := a.End(reasonr.KindMap, 0, m)
	vals := []reasonr.Value{
		reasonr.None, reasonr.Int(-5), reasonr.Bigint(1 << 60), reasonr.Float(1.5), reasonr.Bool(true),
		reasonr.UUID([16]byte{1, 15: 2}), reasonr.TimerID([16]byte{3}), reasonr.Enum(4, 2),
		a.String("a string"), a.Bytes([]byte{0, 1}), reasonr.ID(3, reasonr.UUID([16]byte{9})), a.ID(5, a.String("A1")),
		a.Record(7, a.String("x"), reasonr.None, a.List(reasonr.Int(1), reasonr.Int(2))).Value(), mp,
	}
	for _, v := range vals {
		data := AppendValue(nil, a, v)
		b := reasonr.NewArena()
		b.String("shift")
		got, err := DecodeValue(b, data)
		if err != nil || !reasonr.Equal(a, v, b, got) || got.Type != v.Type {
			t.Errorf("%s: %s, %v", a.Format(v), b.Format(got), err)
		}
		for i := range data {
			if _, err := DecodeValue(b, data[:i]); err == nil {
				t.Errorf("%s: truncated to %d without an error", a.Format(v), i)
			}
		}
	}
}
