// Package integration holds the transports of outgoing calls to the
// integration service (RFC §30–31, §18): a model's external request is
// delivered as POST {url}/requests/{System}/{request} with the arguments in
// the canonical JSON or another codec, an idempotency key and the engine's JWT. External systems
// answer with ordinary commands with the system role (RFC §18).
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// HTTP is the transport over HTTP (RFC §18): POST
// {URL}/requests/{System}/{request}, the body is the arguments without
// timeout in canonical JSON, the headers are Authorization (the engine's
// JWT), Idempotency-Key, X-Reasonr-Tenant, X-Reasonr-Deadline (the time of
// the timeout timer), X-Reasonr-Attempt. 2xx means delivered; any other
// response and a network error are retried by the integrations.retry
// strategy (RFC §18): the service, not the engine, decides that a retry is
// useless.
type HTTP struct {
	URL     string              // the base address of the integration service
	Signer  ports.RequestSigner // the engine's JWT; nil — without Authorization
	Client  *http.Client
	Catalog kind.Catalog
	// Codec is the format of the request body (integrations.<system>.codec);
	// nil — the canonical JSON.
	Codec port.Codec
}

var _ ports.Transport = (*HTTP)(nil)

// Send makes an attempt to deliver a request.
func (h *HTTP) Send(ctx context.Context, lr *kind.LeasedRequest) error {
	s, err := h.Catalog.Schema(lr.Tenant)
	if err != nil {
		return err
	}
	req := &lr.Request
	ti := s.Type(req.System)
	if ti == nil || int(req.Request) >= len(ti.Methods) {
		return fmt.Errorf("%w: unknown request %d.%d", port.ErrPermanent, req.System, req.Request)
	}
	mi := &ti.Methods[req.Request]
	body, err := Body(s, lr.Arena, mi, req.Args, h.Codec)
	if err != nil {
		return fmt.Errorf("%w: arguments of %s.%s: %v", port.ErrPermanent, ti.Name, mi.Name, err)
	}
	target := h.URL + "/requests/" + url.PathEscape(ti.Short()) + "/" + url.PathEscape(mi.Name)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: %v", port.ErrPermanent, err)
	}
	key := Key(lr.Arena, req.Key)
	r.Header.Set("Content-Type", codecs.New(h.Codec, s, codecs.Options{}).Codec().ContentType())
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set("X-Reasonr-Tenant", string(lr.Tenant))
	r.Header.Set("X-Reasonr-Attempt", strconv.Itoa(max(lr.Attempts, 1)))
	if !lr.Deadline.IsZero() {
		r.Header.Set("X-Reasonr-Deadline", lr.Deadline.UTC().Format(time.RFC3339Nano))
	}
	if h.Signer != nil {
		token, err := h.Signer.Token(ti.Short(), lr.Tenant, key)
		if err != nil {
			return fmt.Errorf("signing request %s.%s: %w", ti.Name, mi.Name, err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return fmt.Errorf("%s.%s: HTTP %d %s", ti.Name, mi.Name, resp.StatusCode, bytes.TrimSpace(msg))
}

// Body is the body of the request mi: the arguments args in codec c
// (nil — the canonical JSON) without the timeout parameter (its deadline is
// the X-Reasonr-Deadline header).
func Body(s *reasonr.Schema, a *reasonr.Arena, mi *reasonr.MethodInfo, args reasonr.Record, c port.Codec) ([]byte, error) {
	v := codecs.New(c, s, codecs.Options{})
	if mi.TimeoutParam == 0 {
		return v.AppendFields(nil, a, mi.Params, args)
	}
	vals := a.Fields(args)
	if len(vals) != len(mi.Params) {
		return nil, fmt.Errorf("the record has %d fields, the schema %d", len(vals), len(mi.Params))
	}
	// The record without the timeout goes into an arena of its own: a is
	// the committed unit's, which other deliveries read at the same time.
	own := reasonr.NewArena()
	params := make([]reasonr.FieldInfo, 0, len(mi.Params)-1)
	kept := make([]reasonr.Value, 0, len(mi.Params)-1)
	for i := range mi.Params {
		if i != mi.TimeoutParam-1 {
			params, kept = append(params, mi.Params[i]), append(kept, own.Copy(a, vals[i]))
		}
	}
	return v.AppendFields(nil, own, params, own.Record(0, kept...))
}

// Key is the text form of a request's idempotency key: uuid — 8-4-4-4-12,
// otherwise the value without a schema in base64.
func Key(a *reasonr.Arena, k reasonr.Value) string {
	if k.Kind == reasonr.KindUUID || k.Kind == reasonr.KindTimerID {
		out, _ := codecs.JSONOf(&reasonr.Schema{}, codecs.Options{}).Append(nil, a, reasonr.Type{Kind: reasonr.KindUUID}, reasonr.UUID(k.UUID()))
		return string(out[1 : len(out)-1])
	}
	return fmt.Sprintf("%x", storekit.AppendValue(nil, a, k))
}
