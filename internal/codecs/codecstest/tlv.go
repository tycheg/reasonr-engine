// Package codecstest holds a codec adapter for tests: a binary format with
// field numbers only.
package codecstest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// TLV is a binary test codec: a tag byte per token, then its payload. Field
// names and enum names are not written — only numbers, as schema formats
// do. Tests use it as a codec adapter the compiler does not know.
var TLV port.Codec = tlv{}

type tlv struct{}

func (tlv) Name() string                           { return "tlv" }
func (tlv) ContentType() string                    { return "application/x-reasonr-tlv" }
func (tlv) NewWriter() port.ValueWriter            { return &tlvWriter{} }
func (tlv) NewReader(data []byte) port.ValueReader { return &tlvReader{b: data} }

const (
	tObject byte = iota + 1
	tField
	tList
	tMap
	tEnd
	tNull
	tBool
	tInt
	tFloat
	tDecimal
	tString
	tBytes
	tTime
	tDuration
	tUUID
	tEnum
)

type tlvWriter struct{ b []byte }

func (w *tlvWriter) tag(t byte)            { w.b = append(w.b, t) }
func (w *tlvWriter) varint(x int64)        { w.b = binary.AppendVarint(w.b, x) }
func (w *tlvWriter) text(s string)         { w.varint(int64(len(s))); w.b = append(w.b, s...) }
func (w *tlvWriter) Object(n int)          { w.tag(tObject); w.varint(int64(n)) }
func (w *tlvWriter) Field(_ string, n int) { w.tag(tField); w.varint(int64(n)) }
func (w *tlvWriter) List(n int)            { w.tag(tList); w.varint(int64(n)) }
func (w *tlvWriter) Map(n int)             { w.tag(tMap); w.varint(int64(n)) }
func (w *tlvWriter) End()                  { w.tag(tEnd) }
func (w *tlvWriter) Null()                 { w.tag(tNull) }
func (w *tlvWriter) Bool(v bool) {
	w.tag(tBool)
	if v {
		w.b = append(w.b, 1)
	} else {
		w.b = append(w.b, 0)
	}
}
func (w *tlvWriter) Int(v int64) { w.tag(tInt); w.varint(v) }
func (w *tlvWriter) Float(v float64) {
	w.tag(tFloat)
	w.b = binary.BigEndian.AppendUint64(w.b, math.Float64bits(v))
}
func (w *tlvWriter) Decimal(v string)         { w.tag(tDecimal); w.text(v) }
func (w *tlvWriter) String(v string)          { w.tag(tString); w.text(v) }
func (w *tlvWriter) Bytes(v []byte)           { w.tag(tBytes); w.text(string(v)) }
func (w *tlvWriter) Time(v time.Time)         { w.tag(tTime); w.varint(v.UnixNano()) }
func (w *tlvWriter) Duration(v time.Duration) { w.tag(tDuration); w.varint(int64(v)) }
func (w *tlvWriter) UUID(v [16]byte)          { w.tag(tUUID); w.b = append(w.b, v[:]...) }
func (w *tlvWriter) Enum(_ string, n int)     { w.tag(tEnum); w.varint(int64(n)) }
func (w *tlvWriter) Result() []byte           { return w.b }
func (w *tlvWriter) Reset()                   { w.b = w.b[:0] }

type tlvReader struct {
	b []byte
}

func (r *tlvReader) peek() byte {
	if len(r.b) == 0 {
		return 0
	}
	return r.b[0]
}

func (r *tlvReader) expect(t byte) error {
	if r.peek() != t {
		return fmt.Errorf("tlv: tag %d, want %d", r.peek(), t)
	}
	r.b = r.b[1:]
	return nil
}

func (r *tlvReader) varint() (int64, error) {
	x, n := binary.Varint(r.b)
	if n <= 0 {
		return 0, errors.New("tlv: bad varint")
	}
	r.b = r.b[n:]
	return x, nil
}

func (r *tlvReader) text() (string, error) {
	n, err := r.varint()
	if err != nil || int(n) > len(r.b) {
		return "", errors.New("tlv: bad text")
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s, nil
}

func (r *tlvReader) Null() (bool, error) {
	if len(r.b) == 0 {
		return true, nil
	}
	if r.peek() == tNull {
		r.b = r.b[1:]
		return true, nil
	}
	return false, nil
}

func (r *tlvReader) open(t byte) error {
	if err := r.expect(t); err != nil {
		return err
	}
	_, err := r.varint()
	return err
}

func (r *tlvReader) Object() error { return r.open(tObject) }
func (r *tlvReader) List() error   { return r.open(tList) }
func (r *tlvReader) Map() error    { return r.open(tMap) }

func (r *tlvReader) Field() (string, int, bool, error) {
	if r.peek() == tEnd {
		r.b = r.b[1:]
		return "", 0, false, nil
	}
	if err := r.expect(tField); err != nil {
		return "", 0, false, err
	}
	n, err := r.varint()
	return "", int(n), true, err
}

func (r *tlvReader) More() (bool, error) {
	if r.peek() == tEnd {
		r.b = r.b[1:]
		return false, nil
	}
	return len(r.b) > 0, nil
}

func (r *tlvReader) Skip() error { return errors.New("tlv: skip is not needed in the test") }

func (r *tlvReader) Bool() (bool, error) {
	if err := r.expect(tBool); err != nil || len(r.b) == 0 {
		return false, errors.New("tlv: bool")
	}
	v := r.b[0] == 1
	r.b = r.b[1:]
	return v, nil
}

func (r *tlvReader) Int() (int64, error) {
	if err := r.expect(tInt); err != nil {
		return 0, err
	}
	return r.varint()
}

func (r *tlvReader) Float() (float64, error) {
	if err := r.expect(tFloat); err != nil || len(r.b) < 8 {
		return 0, errors.New("tlv: float")
	}
	v := math.Float64frombits(binary.BigEndian.Uint64(r.b))
	r.b = r.b[8:]
	return v, nil
}

func (r *tlvReader) Decimal() (string, error) {
	if err := r.expect(tDecimal); err != nil {
		return "", err
	}
	return r.text()
}

func (r *tlvReader) String() (string, error) {
	if err := r.expect(tString); err != nil {
		return "", err
	}
	return r.text()
}

func (r *tlvReader) Bytes() ([]byte, error) {
	if err := r.expect(tBytes); err != nil {
		return nil, err
	}
	s, err := r.text()
	return []byte(s), err
}

func (r *tlvReader) Time() (time.Time, error) {
	if err := r.expect(tTime); err != nil {
		return time.Time{}, err
	}
	n, err := r.varint()
	return time.Unix(0, n).UTC(), err
}

func (r *tlvReader) Duration() (time.Duration, error) {
	if err := r.expect(tDuration); err != nil {
		return 0, err
	}
	n, err := r.varint()
	return time.Duration(n), err
}

func (r *tlvReader) UUID() ([16]byte, error) {
	var u [16]byte
	if err := r.expect(tUUID); err != nil || len(r.b) < 16 {
		return u, errors.New("tlv: uuid")
	}
	copy(u[:], r.b)
	r.b = r.b[16:]
	return u, nil
}

func (r *tlvReader) Enum() (string, int, error) {
	if err := r.expect(tEnum); err != nil {
		return "", 0, err
	}
	n, err := r.varint()
	return "", int(n), err
}
