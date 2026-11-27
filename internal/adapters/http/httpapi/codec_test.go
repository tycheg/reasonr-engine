package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/codecs/codecstest"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/port"
)

// TestCodecs: an ingress with a codec besides json takes the command
// arguments in it by Content-Type and answers in it by Accept; a query
// result comes in it; a media type the ingress does not have is 415, and
// json stays the default.
func TestCodecs(t *testing.T) {
	m := taxi(t)
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, m)
	store := memory.NewStore(programs)
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store, Hub: memory.NewHub(false),
		Clock: memory.NewClock(memory.DefaultNow)})
	if err != nil {
		t.Fatal(err)
	}
	s := httpapi.New(k, ports.EdgeDeps{Auth: auth.Chain{"Test": auth.Test{Catalog: programs}}},
		httpapi.Options{AllPublic: true, Codecs: []port.Codec{codecstest.TLV}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	tlv := codecstest.TLV.ContentType()
	do := func(method, path string, body []byte, header ...string) (int, string, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", as("Customer", c1))
		for i := 0; i < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), b
	}

	sc := m.Schema()
	signUp := sc.Lookup("Ordering.Customer").Method("SignUp")
	a := reasonr.NewArena()
	vals := codecs.New(codecstest.TLV, sc, codecs.Options{})
	args, err := vals.AppendArgs(nil, a, signUp.Params, a.Record(0, a.String("+79990000000")))
	if err != nil {
		t.Fatal(err)
	}
	if status, ct, b := do("POST", "/api/Customer/"+c1+"/SignUp", args, "Content-Type", tlv, "Accept", tlv); status != http.StatusOK || ct != tlv || len(b) == 0 {
		t.Fatalf("a command in tlv: %d %s %q", status, ct, b)
	}
	// The same command again: a business error comes in tlv too.
	if status, ct, _ := do("POST", "/api/Customer/"+c1+"/SignUp", args, "Content-Type", tlv, "Accept", tlv); status != http.StatusUnprocessableEntity || ct != tlv {
		t.Fatalf("a rejected command in tlv: %d %s", status, ct)
	}
	if status, _, _ := do("POST", "/api/Customer/"+c1+"/SignUp", args, "Content-Type", "application/x-unknown"); status != http.StatusUnsupportedMediaType {
		t.Fatalf("an unknown media type: %d", status)
	}

	profile := sc.Lookup("Ordering.Customer").Method("Profile")
	if profile == nil || profile.Result == nil {
		t.Fatal("no Customer.Profile query")
	}
	status, ct, b := do("GET", "/api/Customer/Profile", nil, "Accept", tlv)
	if status != http.StatusOK || ct != tlv {
		t.Fatalf("a query in tlv: %d %s %q", status, ct, b)
	}
	v, err := vals.Decode(a, *profile.Result, b)
	if err != nil {
		t.Fatalf("decoding the tlv answer: %v", err)
	}
	js, err := codecs.JSONOf(sc, codecs.Options{}).Append(nil, a, *profile.Result, v)
	if err != nil || !bytes.Contains(js, []byte(`"+79990000000"`)) {
		t.Fatalf("the tlv answer: %s %v", js, err)
	}
	if status, ct, b := do("GET", "/api/Customer/Profile", nil); status != http.StatusOK || ct != "application/json" || !bytes.Contains(b, []byte(`"+79990000000"`)) {
		t.Fatalf("json by default: %d %s %s", status, ct, b)
	}
}
