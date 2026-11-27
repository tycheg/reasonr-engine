package httpapi

import (
	"mime"
	"net/http"
	"strconv"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/port"
)

// Codecs of the model API (plan 0021, decision 6): the arguments of a
// command come in the codec of Content-Type, a command or query answer goes
// in the codec of Accept, among json and the codecs of the ingress
// (ingress[].codecs). Other routes — the app API, MCP, WebSocket, the test
// API — are JSON protocols.

// requestCodec returns the codec of the request body; false — a media type
// the ingress does not accept (415 is written).
func (s *Server) requestCodec(w http.ResponseWriter, r *http.Request) (port.Codec, bool) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return codecs.JSON, true
	}
	if c := s.codecOf(ct); c != nil {
		return c, true
	}
	http.Error(w, "unsupported media type "+ct, http.StatusUnsupportedMediaType)
	return nil, false
}

// responseCodec returns the first codec of Accept the ingress has; json
// without one.
func (s *Server) responseCodec(r *http.Request) port.Codec {
	for _, item := range strings.Split(r.Header.Get("Accept"), ",") {
		if c := s.codecOf(item); c != nil {
			return c
		}
	}
	return codecs.JSON
}

// codecOf returns the codec of a media type among json and the ingress
// codecs; nil — none.
func (s *Server) codecOf(ct string) port.Codec {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(ct))
	if err != nil {
		return nil
	}
	if mt == codecs.JSON.ContentType() {
		return codecs.JSON
	}
	for _, c := range s.opts.Codecs {
		if c.ContentType() == mt {
			return c
		}
	}
	return nil
}

// values returns the values of schema sc in codec c.
func (s *Server) values(sc *reasonr.Schema, c port.Codec) *codecs.Values {
	if c == nil || c.Name() == codecs.JSON.Name() {
		return s.codec(sc)
	}
	return codecs.New(c, sc, codecs.Options{})
}

// envelope writes a command answer or a rejection in a codec other than
// json: the same object the JSON answer has.
type envelope struct {
	w  port.ValueWriter
	sc *reasonr.Schema
	a  *reasonr.Arena
}

func newEnvelope(c port.Codec, sc *reasonr.Schema, a *reasonr.Arena) *envelope {
	return &envelope{w: c.NewWriter(), sc: sc, a: a}
}

// accepted writes {status, position, id?, notifications}.
func (e *envelope) accepted(pos port.Position, id reasonr.Value, ns []reasonr.Notification) error {
	fields := 3
	if id.Kind == reasonr.KindID {
		fields++
	}
	e.w.Object(fields)
	e.w.Field("status", 1)
	e.w.String("accepted")
	e.w.Field("position", 2)
	e.w.String(strconv.FormatUint(uint64(pos), 10))
	if id.Kind == reasonr.KindID {
		e.w.Field("id", 3)
		if err := reasonr.EncodeValue(e.sc, reasonr.Type{Kind: reasonr.KindID, Ref: id.Type}, id, e.a, e.w, codecs.Options{}); err != nil {
			return err
		}
	}
	e.w.Field("notifications", 4)
	e.w.List(len(ns))
	for _, n := range ns {
		ti := e.sc.Type(n.Owner)
		if ti == nil || int(n.Name) >= len(ti.Methods) {
			continue
		}
		mi := &ti.Methods[n.Name]
		if err := e.call(mi.Name, ti.Name, mi.Params, n.Args); err != nil {
			return err
		}
	}
	e.w.End()
	e.w.End()
	return nil
}

// rejected writes {status, errors}.
func (e *envelope) rejected(errs []reasonr.BusinessError) error {
	e.w.Object(2)
	e.w.Field("status", 1)
	e.w.String("rejected")
	e.w.Field("errors", 2)
	e.w.List(len(errs))
	for _, be := range errs {
		var params []reasonr.FieldInfo
		typ := ""
		if ti := e.sc.Type(be.Owner); ti != nil {
			typ = ti.Name
			if mi := ti.Method(be.Name); mi != nil && mi.Kind == reasonr.MethodError {
				params = mi.Params
			}
		} else {
			params = builtinParams[be.Name]
		}
		if len(params) == 0 || len(e.a.Fields(be.Args)) != len(params) {
			params = nil
		}
		if err := e.call(be.Name, typ, params, be.Args); err != nil {
			return err
		}
	}
	e.w.End()
	e.w.End()
	return nil
}

// call writes {name, type?, args?}.
func (e *envelope) call(name, typ string, params []reasonr.FieldInfo, args reasonr.Record) error {
	n := 1
	if typ != "" {
		n++
	}
	if params != nil {
		n++
	}
	e.w.Object(n)
	e.w.Field("name", 1)
	e.w.String(name)
	if typ != "" {
		e.w.Field("type", 2)
		e.w.String(typ)
	}
	if params != nil {
		e.w.Field("args", 3)
		if err := reasonr.EncodeFields(e.sc, params, args, e.a, e.w, codecs.Options{}); err != nil {
			return err
		}
	}
	e.w.End()
	return nil
}

func writeBody(w http.ResponseWriter, c port.Codec, status int, body []byte) {
	w.Header().Set("Content-Type", c.ContentType())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
