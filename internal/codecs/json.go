package codecs

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// JSON is the built-in codec: the canonical JSON of values (RFC §30). Values
// of this codec go through the compiler's canonical JSON directly; its
// Writer and Reader serve code that walks values itself.
var JSON port.Codec = jsonCodec{}

type jsonCodec struct{}

func (jsonCodec) Name() string                           { return "json" }
func (jsonCodec) ContentType() string                    { return "application/json" }
func (jsonCodec) NewWriter() port.ValueWriter            { return &jsonWriter{} }
func (jsonCodec) NewReader(data []byte) port.ValueReader { return newJSONReader(data) }

// jsonWriter writes JSON tokens: a map is a flat list of keys and values.
type jsonWriter struct {
	buf    []byte
	first  []bool // per open container: no element written yet
	closer []byte // per open container: its closing bracket
	key    bool   // a field name was just written: no comma before its value
}

var _ reasonr.ValueWriter = (*jsonWriter)(nil)

func (w *jsonWriter) sep() {
	if w.key {
		w.key = false
		return
	}
	if n := len(w.first); n > 0 {
		if !w.first[n-1] {
			w.buf = append(w.buf, ',')
		}
		w.first[n-1] = false
	}
}

func (w *jsonWriter) open(c, closer byte) {
	w.sep()
	w.buf = append(w.buf, c)
	w.first = append(w.first, true)
	w.closer = append(w.closer, closer)
}

func (w *jsonWriter) Object(int) { w.open('{', '}') }
func (w *jsonWriter) List(int)   { w.open('[', ']') }
func (w *jsonWriter) Map(int)    { w.open('[', ']') }

func (w *jsonWriter) Field(name string, _ int) {
	w.sep()
	w.buf = reasonr.AppendJSONString(w.buf, name)
	w.buf = append(w.buf, ':')
	w.key = true
}

// End closes the last container.
func (w *jsonWriter) End() {
	n := len(w.closer) - 1
	w.buf = append(w.buf, w.closer[n])
	w.first, w.closer = w.first[:n], w.closer[:n]
}

func (w *jsonWriter) scalar(b []byte)          { w.sep(); w.buf = append(w.buf, b...) }
func (w *jsonWriter) str(s string)             { w.sep(); w.buf = reasonr.AppendJSONString(w.buf, s) }
func (w *jsonWriter) Null()                    { w.scalar([]byte("null")) }
func (w *jsonWriter) Bool(v bool)              { w.scalar(strconv.AppendBool(nil, v)) }
func (w *jsonWriter) Int(v int64)              { w.scalar(strconv.AppendInt(nil, v, 10)) }
func (w *jsonWriter) Float(v float64)          { w.scalar(strconv.AppendFloat(nil, v, 'g', -1, 64)) }
func (w *jsonWriter) Decimal(v string)         { w.str(v) }
func (w *jsonWriter) String(v string)          { w.str(v) }
func (w *jsonWriter) Bytes(v []byte)           { w.str(base64.StdEncoding.EncodeToString(v)) }
func (w *jsonWriter) Time(v time.Time)         { w.str(v.UTC().Format(time.RFC3339Nano)) }
func (w *jsonWriter) Duration(v time.Duration) { w.str(v.String()) }
func (w *jsonWriter) UUID(v [16]byte)          { w.str(uuidText(v)) }
func (w *jsonWriter) Enum(name string, _ int)  { w.str(name) }
func (w *jsonWriter) Result() []byte           { return w.buf }
func (w *jsonWriter) Reset() {
	w.buf, w.first, w.closer, w.key = w.buf[:0], w.first[:0], w.closer[:0], false
}

func uuidText(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:], u[10:])
	return string(b[:])
}

func parseUUID(s string) ([16]byte, error) {
	var u [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(s) != 36 || len(h) != 32 {
		return u, fmt.Errorf("invalid uuid %q", s)
	}
	_, err := hex.Decode(u[:], []byte(h))
	return u, err
}

// jsonReader reads the tokens a jsonWriter writes, with one token of lookahead.
type jsonReader struct {
	d     *json.Decoder
	empty bool
	next  json.Token
	ok    bool // next holds a token
}

func newJSONReader(data []byte) *jsonReader {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	return &jsonReader{d: d, empty: len(bytes.TrimSpace(data)) == 0}
}

var _ reasonr.ValueReader = (*jsonReader)(nil)

func (r *jsonReader) peek() (json.Token, error) {
	if !r.ok {
		t, err := r.d.Token()
		if err == io.EOF {
			return nil, errors.New("unexpected end of the value")
		}
		if err != nil {
			return nil, err
		}
		r.next, r.ok = t, true
	}
	return r.next, nil
}

func (r *jsonReader) take() (json.Token, error) {
	t, err := r.peek()
	r.ok = false
	return t, err
}

func (r *jsonReader) Null() (bool, error) {
	if r.empty {
		return true, nil
	}
	t, err := r.peek()
	if err != nil {
		return false, err
	}
	if t == nil {
		r.ok = false
		return true, nil
	}
	return false, nil
}

func (r *jsonReader) delim(want json.Delim) error {
	t, err := r.take()
	if err != nil {
		return err
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %s, got %v", want, t)
	}
	return nil
}

func (r *jsonReader) Object() error { return r.delim('{') }
func (r *jsonReader) List() error   { return r.delim('[') }
func (r *jsonReader) Map() error    { return r.delim('[') }

func (r *jsonReader) Field() (string, int, bool, error) {
	t, err := r.take()
	if err != nil {
		return "", 0, false, err
	}
	switch t := t.(type) {
	case json.Delim:
		if t == '}' {
			return "", 0, false, nil
		}
	case string:
		return t, 0, true, nil
	}
	return "", 0, false, fmt.Errorf("expected a field, got %v", t)
}

func (r *jsonReader) More() (bool, error) {
	t, err := r.peek()
	if err != nil {
		return false, err
	}
	if d, ok := t.(json.Delim); ok && (d == ']' || d == '}') {
		r.ok = false
		return false, nil
	}
	return true, nil
}

func (r *jsonReader) Skip() error {
	t, err := r.take()
	if err != nil {
		return err
	}
	d, ok := t.(json.Delim)
	if !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := r.take()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

func (r *jsonReader) Bool() (bool, error) {
	t, err := r.take()
	if b, ok := t.(bool); ok && err == nil {
		return b, nil
	}
	return false, cmpErr(err, "a boolean", t)
}

func (r *jsonReader) number() (json.Number, error) {
	t, err := r.take()
	if n, ok := t.(json.Number); ok && err == nil {
		return n, nil
	}
	return "", cmpErr(err, "a number", t)
}

func (r *jsonReader) Int() (int64, error) {
	n, err := r.number()
	if err != nil {
		return 0, err
	}
	return n.Int64()
}

func (r *jsonReader) Float() (float64, error) {
	n, err := r.number()
	if err != nil {
		return 0, err
	}
	return n.Float64()
}

func (r *jsonReader) String() (string, error) {
	t, err := r.take()
	if s, ok := t.(string); ok && err == nil {
		return s, nil
	}
	return "", cmpErr(err, "a string", t)
}

func (r *jsonReader) Decimal() (string, error) { return r.String() }

func (r *jsonReader) Bytes() ([]byte, error) {
	s, err := r.String()
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(s)
}

func (r *jsonReader) Time() (time.Time, error) {
	s, err := r.String()
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s)
}

func (r *jsonReader) Duration() (time.Duration, error) {
	s, err := r.String()
	if err != nil {
		return 0, err
	}
	return time.ParseDuration(s)
}

func (r *jsonReader) UUID() ([16]byte, error) {
	s, err := r.String()
	if err != nil {
		return [16]byte{}, err
	}
	return parseUUID(s)
}

func (r *jsonReader) Enum() (string, int, error) {
	s, err := r.String()
	return s, 0, err
}

func cmpErr(err error, want string, got json.Token) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("expected %s, got %v", want, got)
}
