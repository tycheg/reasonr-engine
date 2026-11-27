package port

import "time"

// Codec is a serialization format of model values (plan 0021, decision
// 6): the HTTP API arguments and results by Content-Type and Accept, bus
// messages, requests to the integration service. The built-in codec is
// json (the canonical JSON); others are adapters registered with
// registry.RegisterCodec and chosen in engine.yaml.
//
// The model walks a value by its schema and calls the ValueWriter methods; it
// decodes by pulling tokens from the ValueReader. A codec does not know the
// model's types.
type Codec interface {
	// Name is the codec's name in engine.yaml: json, msgpack, protobuf.
	Name() string
	// ContentType is its media type: application/json.
	ContentType() string
	NewWriter() ValueWriter
	// NewReader reads data; empty data is null.
	NewReader(data []byte) ValueReader
}

// ValueWriter receives a value in walk order and writes its bytes. A record, a
// page and a page request are objects; a list and a set are lists; a map
// is a list of key and value pairs. A date is a Time at midnight UTC, a
// time of day a Duration, a bigint an Int, a timer identifier a UUID, an
// identifier its inner value.
type ValueWriter interface {
	Object(fields int)
	Field(name string, num int) // num is the field's number from 1
	List(n int)
	Map(n int)
	End()
	Null()
	Bool(v bool)
	Int(v int64)
	Float(v float64)
	Decimal(v string) // the decimal text without loss of precision
	String(v string)
	Bytes(v []byte)
	Time(v time.Time)
	Duration(v time.Duration)
	UUID(v [16]byte)
	Enum(name string, num int)
	// Result is what was written; Reset starts a new value.
	Result() []byte
	Reset()
}

// ValueReader gives the tokens of a value in the order a ValueWriter got them; the
// model asks for the kind it expects.
type ValueReader interface {
	// Null reports whether the next value is null and, if so, consumes it.
	Null() (bool, error)
	// Object starts an object; Field gives its next field (a name, or a
	// number for formats without names), ok is false at its end.
	Object() error
	Field() (name string, num int, ok bool, err error)
	// List and Map start a list or a map; More reports whether another
	// element (for a map: a key, then a value) follows.
	List() error
	Map() error
	More() (bool, error)
	// Skip skips the next value.
	Skip() error
	Bool() (bool, error)
	Int() (int64, error)
	Float() (float64, error)
	Decimal() (string, error)
	String() (string, error)
	Bytes() ([]byte, error)
	Time() (time.Time, error)
	Duration() (time.Duration, error)
	UUID() ([16]byte, error)
	Enum() (name string, num int, err error)
}
